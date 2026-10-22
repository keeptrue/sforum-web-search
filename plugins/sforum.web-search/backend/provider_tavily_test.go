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

func TestTavilySendsExpectedRequest(t *testing.T) {
	var (
		path   string
		header http.Header
		body   map[string]any
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path = request.URL.Path
		header = request.Header.Clone()
		json.NewDecoder(request.Body).Decode(&body)
		io.WriteString(writer, `{"query":"q","results":[{"title":"Tavily 结果","url":"https://news.example.com/a","content":"来自 Tavily 的摘要","score":0.81,"published_date":"Tue, 11 Mar 2025 17:00:00 GMT","raw_content":null}],"response_time":"1.67"}`)
	}))
	defer server.Close()

	config := DefaultConfig()
	config.TavilyKey = "tvly-key"
	provider := newTavilyProvider(config, testOutbound(t))
	provider.baseURL = server.URL

	results, err := provider.Search(context.Background(), Request{
		Query: "ai news", Topic: topicNews, Freshness: "month", Site: "https://news.example.com/x",
		Language: "en", SafeSearch: safeSearchStrict, Count: 4,
	})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if path != "/search" {
		t.Fatalf("path = %q, want /search", path)
	}
	if got := header.Get("Authorization"); got != "Bearer tvly-key" {
		t.Errorf("authorization = %q", got)
	}
	if body["query"] != "ai news" || body["topic"] != topicNews {
		t.Errorf("body = %v", body)
	}
	if count, ok := body["max_results"].(float64); !ok || count != 4 {
		t.Errorf("max_results = %v, want 4", body["max_results"])
	}
	// 抽象契约里的 days 在当前文档里由 time_range + filter_by_published_date 表达。
	if body["time_range"] != "month" {
		t.Errorf("time_range = %v, want month", body["time_range"])
	}
	if filter, ok := body["filter_by_published_date"].(bool); !ok || !filter {
		t.Errorf("filter_by_published_date = %v, want true", body["filter_by_published_date"])
	}
	domains, ok := body["include_domains"].([]any)
	if !ok || len(domains) != 1 || domains[0] != "news.example.com" {
		t.Errorf("include_domains = %v, want [news.example.com]", body["include_domains"])
	}
	if body["search_depth"] != "basic" {
		t.Errorf("search_depth = %v, want basic", body["search_depth"])
	}
	if safe, ok := body["safe_search"].(bool); !ok || !safe {
		t.Errorf("safe_search = %v, want true for strict", body["safe_search"])
	}
	if body["language"] != "en" {
		t.Errorf("language = %v, want en", body["language"])
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	if results[0].Snippet != "来自 Tavily 的摘要" || results[0].Site != "news.example.com" {
		t.Errorf("result = %+v", results[0])
	}
	if results[0].Date != "2025-03-11" {
		t.Errorf("date = %q, want 2025-03-11 (RFC 1123 input)", results[0].Date)
	}
}

func TestTavilyOmitsOptionalFilters(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		json.NewDecoder(request.Body).Decode(&body)
		io.WriteString(writer, `{"results":[]}`)
	}))
	defer server.Close()

	config := DefaultConfig()
	config.TavilyKey = "k"
	provider := newTavilyProvider(config, testOutbound(t))
	provider.baseURL = server.URL
	if _, err := provider.Search(context.Background(), Request{Query: "q", Count: 1, Topic: topicGeneral, SafeSearch: safeSearchOff}); err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if _, ok := body["time_range"]; ok {
		t.Errorf("time_range must be omitted without freshness: %v", body)
	}
	if _, ok := body["include_domains"]; ok {
		t.Errorf("include_domains must be omitted without site: %v", body)
	}
	if safe, ok := body["safe_search"].(bool); !ok || safe {
		t.Errorf("safe_search = %v, want false for off", body["safe_search"])
	}
}

func TestTavilyErrorMapping(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		body          string
		wantRetryable bool
		wantAuth      bool
		wantReason    string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"detail":{"error":"Unauthorized: missing or invalid API key."}}`, true, true, "invalid API key"},
		{"rate limited", http.StatusTooManyRequests, `{"detail":{"error":"excessive requests"}}`, true, false, "被限流"},
		{"bad request", http.StatusBadRequest, `{"detail":{"error":"Invalid topic."}}`, false, false, "Invalid topic"},
		{"plan limit", http.StatusForbidden, `{"detail":{"error":"exceeds your plan's set usage limit"}}`, true, true, "凭据被拒绝"},
		{"server error", http.StatusInternalServerError, `{"detail":{"error":"Internal Server Error"}}`, true, false, "后端故障"},
		{"malformed json", http.StatusOK, `nope`, true, false, "JSON"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(testCase.status)
				io.WriteString(writer, testCase.body)
			}))
			defer server.Close()

			config := DefaultConfig()
			config.TavilyKey = "k"
			provider := newTavilyProvider(config, testOutbound(t))
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

func TestTavilyDeclaresNewsSupport(t *testing.T) {
	provider := newTavilyProvider(DefaultConfig(), nil)
	topics := provider.Topics()
	if len(topics) != 2 || topics[0] != topicGeneral || topics[1] != topicNews {
		t.Fatalf("topics = %v, want [general news]", topics)
	}
}
