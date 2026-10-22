package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDeepSeekSendsExpectedRequest(t *testing.T) {
	fixed := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
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
		json.NewDecoder(request.Body).Decode(&body)
		io.WriteString(writer, `{"results":[{"title":"DeepSeek V4 released","url":"https://example.com/v4","snippet":"短摘要","summary":"长摘要，给模型读","content":"正文","site_name":"Example News","date":"2026-06-01T00:00:00+00:00","authority":3,"rerank_score":2.4}]}`)
	}))
	defer server.Close()

	config := DefaultConfig()
	config.DeepSeekKey = "ds-key"
	provider := newDeepSeekProvider(config, testOutbound(t))
	provider.baseURL = server.URL
	provider.now = func() time.Time { return fixed }

	results, err := provider.Search(context.Background(), Request{
		Query: "deepseek v4", Site: "docs.example.com", Freshness: "week", Language: "zh", Count: 5,
	})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if method != http.MethodPost || path != "/v1/tools/search" {
		t.Fatalf("request = %s %s, want POST /v1/tools/search", method, path)
	}
	if got := header.Get("Authorization"); got != "Bearer ds-key" {
		t.Errorf("authorization = %q", got)
	}
	query, _ := body["query"].(string)
	// 站点、时间范围与语言都是检索算子：site: / after: / lang:
	for _, want := range []string{"deepseek v4", "site:docs.example.com", "after:2026-10-12", "lang:zh"} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q is missing %q", query, want)
		}
	}
	if count, ok := body["count"].(float64); !ok || count != 5 {
		t.Errorf("count = %v, want 5", body["count"])
	}
	if needContent, ok := body["need_content"].(bool); !ok || needContent {
		t.Errorf("need_content = %v, want false", body["need_content"])
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	item := results[0]
	if item.Summary != "长摘要，给模型读" || item.Snippet != "短摘要" {
		t.Errorf("summary/snippet = %q/%q", item.Summary, item.Snippet)
	}
	if item.Date != "2026-06-01" || item.Site != "Example News" {
		t.Errorf("date/site = %q/%q", item.Date, item.Site)
	}
}

func TestDeepSeekFreshnessWindows(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	cases := map[string]string{
		"day":   "2026-10-18",
		"week":  "2026-10-12",
		"month": "2026-09-19",
		"year":  "2025-10-19",
		"":      "",
	}
	for freshness, want := range cases {
		if got := deepSeekAfter(freshness, now); got != want {
			t.Errorf("deepSeekAfter(%q) = %q, want %q", freshness, got, want)
		}
	}
}

func TestDeepSeekErrorMapping(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		body          string
		wantRetryable bool
		wantAuth      bool
		wantReason    string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":{"message":"invalid key","type":"auth","code":"401"}}`, true, true, "凭据被拒绝"},
		{"insufficient balance", http.StatusPaymentRequired, `{"error":{"message":"insufficient balance"}}`, true, false, "余额不足"},
		{"rate limited", http.StatusTooManyRequests, ``, true, false, "被限流"},
		{"downstream timeout", http.StatusGatewayTimeout, ``, true, false, "后端故障"},
		{"bad request", http.StatusBadRequest, `{"error":{"message":"'count' must be between 1 and 50"}}`, false, false, "must be between"},
		{"malformed json", http.StatusOK, `oops`, true, false, "JSON"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(testCase.status)
				io.WriteString(writer, testCase.body)
			}))
			defer server.Close()

			config := DefaultConfig()
			config.DeepSeekKey = "ds-key"
			provider := newDeepSeekProvider(config, testOutbound(t))
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

func TestDeepSeekDeclaresNewsSupport(t *testing.T) {
	provider := newDeepSeekProvider(DefaultConfig(), nil)
	topics := provider.Topics()
	if len(topics) != 2 || topics[0] != topicGeneral || topics[1] != topicNews {
		t.Fatalf("topics = %v, want [general news]", topics)
	}
}
