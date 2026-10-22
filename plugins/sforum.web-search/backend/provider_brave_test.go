package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestBraveWebSearchSendsExpectedRequest(t *testing.T) {
	var (
		path   string
		query  url.Values
		header http.Header
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path = request.URL.Path
		query = request.URL.Query()
		header = request.Header.Clone()
		io.WriteString(writer, `{"type":"search","web":{"type":"search","results":[{"title":"Brave 结果","url":"https://example.com/a","description":"来自 Brave 的描述","page_age":"2026-04-12T14:22:41","meta_url":{"hostname":"example.com"}}]}}`)
	}))
	defer server.Close()

	config := DefaultConfig()
	config.BraveKey = "brave-token"
	provider := newBraveProvider(config, testOutbound(t))
	provider.baseURL = server.URL

	results, err := provider.Search(context.Background(), Request{
		Query: "sforum", Topic: topicGeneral, Freshness: "week", Site: "docs.example.com",
		Language: "zh", SafeSearch: safeSearchModerate, Count: 6,
	})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if path != "/res/v1/web/search" {
		t.Fatalf("path = %q, want /res/v1/web/search", path)
	}
	if got := header.Get("X-Subscription-Token"); got != "brave-token" {
		t.Errorf("X-Subscription-Token = %q", got)
	}
	if got := query.Get("q"); got != "sforum site:docs.example.com" {
		t.Errorf("q = %q", got)
	}
	if got := query.Get("count"); got != "6" {
		t.Errorf("count = %q", got)
	}
	if got := query.Get("freshness"); got != "pw" {
		t.Errorf("freshness = %q, want pw", got)
	}
	if got := query.Get("safesearch"); got != "moderate" {
		t.Errorf("safesearch = %q", got)
	}
	if got := query.Get("search_lang"); got != "zh" {
		t.Errorf("search_lang = %q", got)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	if results[0].Snippet != "来自 Brave 的描述" || results[0].Site != "example.com" {
		t.Errorf("result = %+v", results[0])
	}
	if results[0].Date != "2026-04-12" {
		t.Errorf("date = %q, want 2026-04-12", results[0].Date)
	}
}

func TestBraveNewsSearchUsesNewsEndpoint(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		path = request.URL.Path
		// news 端点的结果在顶层 results[]，不在 web.results[]。
		io.WriteString(writer, `{"type":"news","results":[{"title":"Breaking","url":"https://news.example.com/a","description":"新闻摘要","page_age":"2026-10-18T08:00:00","meta_url":{"hostname":"news.example.com"},"age":"1 day ago","breaking":true}]}`)
	}))
	defer server.Close()

	config := DefaultConfig()
	config.BraveKey = "k"
	provider := newBraveProvider(config, testOutbound(t))
	provider.baseURL = server.URL

	results, err := provider.Search(context.Background(), Request{Query: "sforum", Topic: topicNews, Count: 2, SafeSearch: safeSearchStrict})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if path != "/res/v1/news/search" {
		t.Fatalf("path = %q, want /res/v1/news/search", path)
	}
	if len(results) != 1 || results[0].Title != "Breaking" || results[0].Date != "2026-10-18" {
		t.Fatalf("results = %+v", results)
	}
}

func TestBraveFreshnessMapping(t *testing.T) {
	cases := map[string]string{"day": "pd", "week": "pw", "month": "pm", "year": "py", "": ""}
	for freshness, want := range cases {
		if got := braveFreshness(freshness); got != want {
			t.Errorf("braveFreshness(%q) = %q, want %q", freshness, got, want)
		}
	}
}

func TestBraveErrorMapping(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		body          string
		wantRetryable bool
		wantAuth      bool
		wantReason    string
	}{
		{"unauthorized", http.StatusUnauthorized, `{"error":{"status":401,"code":"UNAUTHENTICATED","detail":"Invalid token"}}`, true, true, "Invalid token"},
		{"rate limited", http.StatusTooManyRequests, `{"error":{"code":"RATE_LIMITED","detail":"Slow down"}}`, true, false, "被限流"},
		{"unprocessable", http.StatusUnprocessableEntity, `{"error":{"detail":"Invalid parameter"}}`, false, false, "Invalid parameter"},
		{"server error", http.StatusBadGateway, ``, true, false, "后端故障"},
		{"malformed json", http.StatusOK, `<html>`, true, false, "JSON"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(testCase.status)
				io.WriteString(writer, testCase.body)
			}))
			defer server.Close()

			config := DefaultConfig()
			config.BraveKey = "k"
			provider := newBraveProvider(config, testOutbound(t))
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

func TestBraveDeclaresNewsSupport(t *testing.T) {
	provider := newBraveProvider(DefaultConfig(), nil)
	topics := provider.Topics()
	if len(topics) != 2 || topics[0] != topicGeneral || topics[1] != topicNews {
		t.Fatalf("topics = %v, want [general news]", topics)
	}
}
