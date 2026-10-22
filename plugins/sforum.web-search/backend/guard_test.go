package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckEndpointAcceptsPublicHTTPS(t *testing.T) {
	for _, raw := range []string{
		"https://api.anysearch.com/v1/search",
		"https://api.deepseek.com",
		"https://api.tavily.com/search",
		"https://api.search.brave.com/res/v1/web/search?q=x",
	} {
		if _, err := checkEndpoint(raw, false, false); err != nil {
			t.Errorf("checkEndpoint(%q) = %v, want it to pass", raw, err)
		}
	}
}

func TestCheckEndpointRejectsUnsafeShapes(t *testing.T) {
	cases := []struct {
		name         string
		raw          string
		allowHTTP    bool
		allowPrivate bool
		wantError    bool
	}{
		{"plain http rejected by default", "http://example.com/", false, false, true},
		{"plain http allowed on demand", "http://example.com/", true, false, false},
		{"userinfo rejected", "https://user:pass@example.com/", false, false, true},
		{"localhost rejected", "https://localhost:8080/", false, false, true},
		{"dot localhost rejected", "https://api.localhost/", false, false, true},
		{"dot local rejected", "https://printer.local/", false, false, true},
		{"dot internal rejected", "https://search.internal/", false, false, true},
		{"loopback ipv4 rejected", "https://127.0.0.1/", false, false, true},
		{"loopback ipv6 rejected", "https://[::1]/", false, false, true},
		{"private ipv4 rejected", "https://10.1.2.3/", false, false, true},
		{"private ipv6 rejected", "https://[fd00::1]/", false, false, true},
		{"link local rejected", "https://169.254.169.254/", false, false, true},
		{"cgnat rejected", "https://100.64.0.1/", false, false, true},
		{"private allowed on demand", "https://127.0.0.1:8080/", false, true, false},
		{"internal name allowed on demand", "https://search.internal/", false, true, false},
		{"unsupported scheme rejected", "ftp://example.com/", false, false, true},
		{"empty rejected", "   ", false, false, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := checkEndpoint(testCase.raw, testCase.allowHTTP, testCase.allowPrivate)
			if (err != nil) != testCase.wantError {
				t.Fatalf("checkEndpoint(%q) error = %v, want error=%v", testCase.raw, err, testCase.wantError)
			}
			if err != nil {
				// 配置类失败不重试：换后端也修不好一个写错的地址。
				failure := transportFailure("p", err)
				if failure.Retryable {
					t.Errorf("endpoint failures must not be retryable: %+v", failure)
				}
			}
		})
	}
}

func TestOutboundDoesNotFollowRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		t.Error("the redirect target must never be requested")
	}))
	defer target.Close()

	redirecting := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL+"/moved", http.StatusFound)
	}))
	defer redirecting.Close()

	client := testOutbound(t)
	status, _, err := client.do(context.Background(), http.MethodGet, redirecting.URL, http.Header{}, nil)
	if err != nil {
		t.Fatalf("do returned a transport error: %v", err)
	}
	if status != http.StatusFound {
		t.Fatalf("status = %d, want 302 (redirects must not be followed)", status)
	}
	failure := statusFailure("p", status, "")
	if !failure.Retryable {
		t.Errorf("a 3xx must be treated as an upstream failure: %+v", failure)
	}
}

func TestOutboundRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
		chunk := strings.Repeat("x", 64*1024)
		for written := 0; written <= maxProviderResponseBytes; written += len(chunk) {
			if _, err := io.WriteString(writer, chunk); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	client := testOutbound(t)
	_, _, err := client.do(context.Background(), http.MethodGet, server.URL, http.Header{}, nil)
	if err == nil {
		t.Fatal("expected an oversized-response error")
	}
	failure := transportFailure("p", err)
	if !failure.Retryable {
		t.Errorf("oversized responses are an upstream condition and should be retryable: %+v", failure)
	}
	if !strings.Contains(failure.Reason, "上限") {
		t.Errorf("reason = %q", failure.Reason)
	}
}

func TestOutboundSendsUserAgent(t *testing.T) {
	var userAgent string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		userAgent = request.Header.Get("User-Agent")
		io.WriteString(writer, `{}`)
	}))
	defer server.Close()

	client := testOutbound(t)
	if _, _, err := client.do(context.Background(), http.MethodGet, server.URL, http.Header{}, nil); err != nil {
		t.Fatalf("do: %v", err)
	}
	if userAgent != providerUserAgent {
		t.Errorf("user-agent = %q, want %q", userAgent, providerUserAgent)
	}
}

func TestParseProxyURL(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:7897", "https://proxy.example.com:8443", "socks5://127.0.0.1:1080"} {
		if _, err := parseProxyURL(raw); err != nil {
			t.Errorf("parseProxyURL(%q) = %v", raw, err)
		}
	}
	for _, raw := range []string{"ftp://127.0.0.1:21", "http://user:pass@127.0.0.1:8080", "http://"} {
		if _, err := parseProxyURL(raw); err == nil {
			t.Errorf("parseProxyURL(%q) accepted an unsupported proxy", raw)
		}
	}
}

func TestNewProviderClientRejectsUnsupportedProxy(t *testing.T) {
	config := DefaultConfig()
	config.ProxyURL = "ftp://127.0.0.1:21"
	if _, err := newProviderClient(config); err == nil {
		t.Fatal("expected an error for an unsupported proxy scheme")
	}
}
