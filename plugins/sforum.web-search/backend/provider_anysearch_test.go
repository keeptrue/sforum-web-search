package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnySearchSendsExpectedRequest(t *testing.T) {
	var (
		method string
		path   string
		header http.Header
		body   map[string]any
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		method = request.Method
		path = request.URL.Path
		header = request.Header.Clone()
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		io.WriteString(writer, `{"data":{"results":[{"title":"SForum 文档","url":"https://example.com/docs","snippet":"一段摘要","content":"正文不取"}],"metadata":{"total_results":1,"search_time_ms":12}}}`)
	}))
	defer server.Close()

	config := DefaultConfig()
	config.AnySearchZone = "cn"
	provider := newAnySearchProvider(config, testOutbound(t))
	provider.baseURL = server.URL

	results, err := provider.Search(context.Background(), Request{Query: "sforum", Count: 3, Language: "zh"})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}

	if method != http.MethodPost || path != "/v1/search" {
		t.Fatalf("request = %s %s, want POST /v1/search", method, path)
	}
	if got := header.Get("accept"); got != "application/json" {
		t.Errorf("accept = %q", got)
	}
	if got := header.Get("content-type"); got != "application/json" {
		t.Errorf("content-type = %q", got)
	}
	if got := header.Get("x-anysearch-client"); got != anySearchClient {
		t.Errorf("x-anysearch-client = %q, want %q", got, anySearchClient)
	}
	// 匿名模式：没有 key 就不带 Authorization。
	if got := header.Get("authorization"); got != "" {
		t.Errorf("authorization = %q, want empty for anonymous calls", got)
	}
	if body["query"] != "sforum" || body["tag"] != anySearchTag {
		t.Errorf("body = %v", body)
	}
	if count, ok := body["max_results"].(float64); !ok || count != 3 {
		t.Errorf("max_results = %v, want 3", body["max_results"])
	}
	if body["zone"] != "cn" {
		t.Errorf("zone = %v, want cn", body["zone"])
	}
	if body["language"] != "zh" {
		t.Errorf("language = %v, want zh", body["language"])
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	if results[0].Title != "SForum 文档" || results[0].URL != "https://example.com/docs" ||
		results[0].Snippet != "一段摘要" || results[0].Site != "example.com" {
		t.Errorf("result = %+v", results[0])
	}
	// content 是正文，本插件只要摘要。
	if results[0].Summary != "" {
		t.Errorf("summary = %q, want empty (content must not be used)", results[0].Summary)
	}
}

func TestAnySearchOmitsOptionalFields(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		json.NewDecoder(request.Body).Decode(&body)
		io.WriteString(writer, `{"data":{"results":[]}}`)
	}))
	defer server.Close()

	config := DefaultConfig() // zone=auto、没有 key
	provider := newAnySearchProvider(config, testOutbound(t))
	provider.baseURL = server.URL
	if _, err := provider.Search(context.Background(), Request{Query: "q", Count: 1, Language: "ja"}); err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if _, ok := body["zone"]; ok {
		t.Errorf("zone must be omitted when auto: %v", body)
	}
	if _, ok := body["language"]; ok {
		t.Errorf("language must be omitted for unsupported languages: %v", body)
	}
}

func TestAnySearchSendsBearerWhenKeyConfigured(t *testing.T) {
	var header http.Header
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		header = request.Header.Clone()
		io.WriteString(writer, `{"data":{"results":[]}}`)
	}))
	defer server.Close()

	config := DefaultConfig()
	config.AnySearchKey = "as-key"
	provider := newAnySearchProvider(config, testOutbound(t))
	provider.baseURL = server.URL
	if _, err := provider.Search(context.Background(), Request{Query: "q", Count: 1}); err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if got := header.Get("authorization"); got != "Bearer as-key" {
		t.Errorf("authorization = %q", got)
	}
}

func TestAnySearchErrorMapping(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		body          string
		wantRetryable bool
		wantAuth      bool
		wantReason    string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"message":"invalid api key"}`, true, true, "凭据被拒绝"},
		{"rate limited", http.StatusTooManyRequests, ``, true, false, "被限流"},
		{"server error", http.StatusInternalServerError, `{"error":{"message":"boom"}}`, true, false, "后端故障"},
		{"bad request", http.StatusBadRequest, `{"error":{"message":"bad query"}}`, false, false, "请求被拒绝"},
		{"malformed json", http.StatusOK, `not json`, true, false, "JSON"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(testCase.status)
				io.WriteString(writer, testCase.body)
			}))
			defer server.Close()

			provider := newAnySearchProvider(DefaultConfig(), testOutbound(t))
			provider.baseURL = server.URL
			_, err := provider.Search(context.Background(), Request{Query: "q", Count: 1})
			failure := asFailure(t, err)
			if failure.Retryable != testCase.wantRetryable {
				t.Errorf("retryable = %v, want %v (reason %q)", failure.Retryable, testCase.wantRetryable, failure.Reason)
			}
			if failure.Auth != testCase.wantAuth {
				t.Errorf("auth = %v, want %v", failure.Auth, testCase.wantAuth)
			}
			if !strings.Contains(failure.Reason, testCase.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", failure.Reason, testCase.wantReason)
			}
		})
	}
}

func TestAnySearchDeclaresGeneralOnly(t *testing.T) {
	provider := newAnySearchProvider(DefaultConfig(), nil)
	topics := provider.Topics()
	if len(topics) != 1 || topics[0] != topicGeneral {
		t.Fatalf("topics = %v, want [general]", topics)
	}
}
