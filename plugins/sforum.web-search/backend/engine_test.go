package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeProvider 是可编排的 Provider：按顺序返回预设失败，失败用完后返回固定结果。
type fakeProvider struct {
	name        string
	topics      []string
	results     []Result
	failures    []error
	delay       time.Duration
	calls       int
	lastRequest Request
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Topics() []string {
	if len(f.topics) == 0 {
		return []string{topicGeneral}
	}
	return f.topics
}

func (f *fakeProvider) Search(ctx context.Context, req Request) ([]Result, error) {
	f.calls++
	f.lastRequest = req
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if len(f.failures) > 0 {
		failure := f.failures[0]
		f.failures = f.failures[1:]
		return nil, failure
	}
	return f.results, nil
}

func sampleResults() []Result {
	return []Result{
		{Title: "SForum 文档", URL: "https://example.com/docs", Snippet: "一段摘要", Date: "2026-06-01", Site: "Example"},
	}
}

func retryableFailure(provider string) error {
	return &searchFailure{Provider: provider, Reason: "后端故障（HTTP 500）", Retryable: true}
}

func TestEngineRoutesNewsOnlyToBackendsThatDeclareIt(t *testing.T) {
	anysearch := &fakeProvider{name: providerAnySearch, topics: []string{topicGeneral}, results: sampleResults()}
	deepseek := &fakeProvider{name: providerDeepSeek, topics: []string{topicGeneral, topicNews}, results: sampleResults()}
	engine := newEngineWith(DefaultConfig(), []Provider{anysearch, deepseek})

	result, err := engine.RunModel(context.Background(), "zh-CN", Request{Query: "ai", Topic: topicNews})
	if err != nil {
		t.Fatalf("RunModel returned an internal error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected failure: %s", result.Content)
	}
	if anysearch.calls != 0 {
		t.Errorf("anysearch calls = %d, want 0 for topic=news", anysearch.calls)
	}
	if deepseek.calls != 1 {
		t.Errorf("deepseek calls = %d, want 1", deepseek.calls)
	}
	if deepseek.lastRequest.Topic != topicNews {
		t.Errorf("topic forwarded = %q, want news", deepseek.lastRequest.Topic)
	}
}

func TestEngineExplainsMissingNewsBackend(t *testing.T) {
	anysearch := &fakeProvider{name: providerAnySearch, topics: []string{topicGeneral}, results: sampleResults()}
	engine := newEngineWith(DefaultConfig(), []Provider{anysearch})

	result, err := engine.RunModel(context.Background(), "", Request{Query: "ai", Topic: topicNews})
	if err != nil {
		t.Fatalf("RunModel returned an internal error: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "支持新闻检索的搜索后端") {
		t.Fatalf("result = %+v, want the missing-news-backend explanation", result)
	}
	if anysearch.calls != 0 {
		t.Errorf("anysearch calls = %d, want 0", anysearch.calls)
	}
}

func TestEngineFailsOverOnRetryableFailure(t *testing.T) {
	first := &fakeProvider{name: providerAnySearch, failures: []error{retryableFailure(providerAnySearch)}}
	second := &fakeProvider{name: providerDeepSeek, topics: []string{topicGeneral, topicNews}, results: sampleResults()}
	engine := newEngineWith(DefaultConfig(), []Provider{first, second})

	result, err := engine.RunModel(context.Background(), "", Request{Query: "sforum"})
	if err != nil {
		t.Fatalf("RunModel returned an internal error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected failure: %s", result.Content)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Errorf("calls = %d/%d, want 1/1", first.calls, second.calls)
	}
	// 结果头必须如实标出真正答话的后端。
	if !strings.Contains(result.Content, "引擎：DeepSeek") {
		t.Errorf("content does not name the backend that answered:\n%s", result.Content)
	}
}

func TestEngineStopsOnNonRetryableFailure(t *testing.T) {
	first := &fakeProvider{name: providerAnySearch, failures: []error{
		&searchFailure{Provider: providerAnySearch, Reason: "请求被拒绝（HTTP 400）：bad query", Retryable: false},
	}}
	second := &fakeProvider{name: providerDeepSeek, topics: []string{topicGeneral, topicNews}, results: sampleResults()}
	engine := newEngineWith(DefaultConfig(), []Provider{first, second})

	result, err := engine.RunModel(context.Background(), "", Request{Query: "sforum"})
	if err != nil {
		t.Fatalf("RunModel returned an internal error: %v", err)
	}
	if !result.IsError || result.Content != messageUnavailable {
		t.Fatalf("result = %+v, want the stable unavailable message", result)
	}
	if second.calls != 0 {
		t.Errorf("second backend calls = %d, want 0 after a request-construction failure", second.calls)
	}
}

func TestEngineBreakerSkipsCoolingBackend(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	first := &fakeProvider{name: providerAnySearch, failures: []error{retryableFailure(providerAnySearch)}}
	second := &fakeProvider{name: providerDeepSeek, topics: []string{topicGeneral, topicNews}, results: sampleResults()}
	engine := newEngineWith(DefaultConfig(), []Provider{first, second})
	engine.now = func() time.Time { return now }

	if _, err := engine.RunModel(context.Background(), "", Request{Query: "sforum"}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if first.calls != 1 {
		t.Fatalf("first backend calls = %d, want 1", first.calls)
	}
	// 冷却期内：第一个后端不再被尝试。
	if _, err := engine.RunModel(context.Background(), "", Request{Query: "sforum-2"}); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if first.calls != 1 {
		t.Errorf("first backend calls = %d during cooldown, want 1", first.calls)
	}
	// 冷却结束后重新参与。
	now = now.Add(breakerCooldown + time.Second)
	first.failures = nil
	first.results = sampleResults()
	if _, err := engine.RunModel(context.Background(), "", Request{Query: "sforum-3"}); err != nil {
		t.Fatalf("third call: %v", err)
	}
	if first.calls != 2 {
		t.Errorf("first backend calls = %d after cooldown, want 2", first.calls)
	}
}

func TestEngineUsesLongerCooldownForCredentialFailures(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	first := &fakeProvider{name: providerAnySearch, failures: []error{
		&searchFailure{Provider: providerAnySearch, Reason: "凭据被拒绝（HTTP 401）", Retryable: true, Auth: true},
	}}
	second := &fakeProvider{name: providerDeepSeek, topics: []string{topicGeneral, topicNews}, results: sampleResults()}
	engine := newEngineWith(DefaultConfig(), []Provider{first, second})
	engine.now = func() time.Time { return now }

	if _, err := engine.RunModel(context.Background(), "", Request{Query: "q1"}); err != nil {
		t.Fatalf("first call: %v", err)
	}
	now = now.Add(breakerCooldown + time.Second)
	if _, err := engine.RunModel(context.Background(), "", Request{Query: "q2"}); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if first.calls != 1 {
		t.Errorf("first backend calls = %d, want 1 (auth cooldown is %s)", first.calls, breakerAuthCooldown)
	}
	now = now.Add(breakerAuthCooldown)
	if _, err := engine.RunModel(context.Background(), "", Request{Query: "q3"}); err != nil {
		t.Fatalf("third call: %v", err)
	}
	if first.calls != 2 {
		t.Errorf("first backend calls = %d after auth cooldown, want 2", first.calls)
	}
}

func TestEngineRoundRobinAdvancesCursor(t *testing.T) {
	config := DefaultConfig()
	config.Strategy = strategyRoundRobin
	providers := []Provider{
		&fakeProvider{name: providerAnySearch, results: sampleResults()},
		&fakeProvider{name: providerDeepSeek, topics: []string{topicGeneral, topicNews}, results: sampleResults()},
		&fakeProvider{name: providerTavily, topics: []string{topicGeneral, topicNews}, results: sampleResults()},
	}
	engine := newEngineWith(config, providers)

	want := []string{providerAnySearch, providerDeepSeek, providerTavily, providerAnySearch}
	for index, expected := range want {
		result, err := engine.RunModel(context.Background(), "", Request{Query: "query-" + string(rune('a'+index))})
		if err != nil {
			t.Fatalf("call %d: %v", index, err)
		}
		if !strings.Contains(result.Content, "引擎："+displayProviderName(expected)) {
			t.Errorf("call %d used %q, want %q", index, result.Content, expected)
		}
	}
}

func TestEnginePriorityUsesFirstHealthyBackendOnly(t *testing.T) {
	first := &fakeProvider{name: providerAnySearch, results: sampleResults()}
	second := &fakeProvider{name: providerDeepSeek, topics: []string{topicGeneral, topicNews}, results: sampleResults()}
	engine := newEngineWith(DefaultConfig(), []Provider{first, second})

	for index := 0; index < 3; index++ {
		if _, err := engine.RunModel(context.Background(), "", Request{Query: "q" + string(rune('a'+index))}); err != nil {
			t.Fatalf("call %d: %v", index, err)
		}
	}
	if first.calls != 3 || second.calls != 0 {
		t.Errorf("calls = %d/%d, want 3/0 for priority strategy", first.calls, second.calls)
	}
}

func TestEngineCachesSuccessfulSearch(t *testing.T) {
	provider := &fakeProvider{name: providerAnySearch, results: sampleResults()}
	engine := newEngineWith(DefaultConfig(), []Provider{provider})

	first, err := engine.RunModel(context.Background(), "", Request{Query: "sforum"})
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	second, err := engine.RunModel(context.Background(), "", Request{Query: "sforum"})
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want 1 (second call must hit the cache)", provider.calls)
	}
	if first.Content != second.Content {
		t.Errorf("cached content differs:\n%s\n---\n%s", first.Content, second.Content)
	}
}

func TestEngineCacheExpiresAndCanBeDisabled(t *testing.T) {
	t.Run("ttl expires", func(t *testing.T) {
		now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
		provider := &fakeProvider{name: providerAnySearch, results: sampleResults()}
		config := DefaultConfig()
		config.CacheTTLSec = 60
		engine := newEngineWith(config, []Provider{provider})
		engine.now = func() time.Time { return now }

		if _, err := engine.RunModel(context.Background(), "", Request{Query: "sforum"}); err != nil {
			t.Fatalf("first call: %v", err)
		}
		now = now.Add(61 * time.Second)
		if _, err := engine.RunModel(context.Background(), "", Request{Query: "sforum"}); err != nil {
			t.Fatalf("second call: %v", err)
		}
		if provider.calls != 2 {
			t.Errorf("provider calls = %d, want 2 after TTL expiry", provider.calls)
		}
	})

	t.Run("ttl zero disables cache", func(t *testing.T) {
		provider := &fakeProvider{name: providerAnySearch, results: sampleResults()}
		config := DefaultConfig()
		config.CacheTTLSec = 0
		engine := newEngineWith(config, []Provider{provider})
		for index := 0; index < 2; index++ {
			if _, err := engine.RunModel(context.Background(), "", Request{Query: "sforum"}); err != nil {
				t.Fatalf("call %d: %v", index, err)
			}
		}
		if provider.calls != 2 {
			t.Errorf("provider calls = %d, want 2 with cache disabled", provider.calls)
		}
	})
}

func TestEngineEmptyResultsIsNotAnError(t *testing.T) {
	provider := &fakeProvider{name: providerAnySearch, results: nil}
	engine := newEngineWith(DefaultConfig(), []Provider{provider})

	result, err := engine.RunModel(context.Background(), "", Request{Query: "sforum"})
	if err != nil {
		t.Fatalf("RunModel returned an internal error: %v", err)
	}
	if result.IsError {
		t.Fatalf("empty results must not be an error: %+v", result)
	}
	if !strings.Contains(result.Content, "没有找到相关结果") {
		t.Errorf("content = %q", result.Content)
	}
}

func TestEngineDisabledReturnsExplainableFailure(t *testing.T) {
	provider := &fakeProvider{name: providerAnySearch, results: sampleResults()}
	config := DefaultConfig()
	config.Enabled = false
	engine := newEngineWith(config, []Provider{provider})

	result, err := engine.RunModel(context.Background(), "", Request{Query: "sforum"})
	if err != nil {
		t.Fatalf("RunModel returned an internal error: %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "已被管理员关闭") {
		t.Fatalf("result = %+v", result)
	}
	if provider.calls != 0 {
		t.Errorf("provider calls = %d, want 0 when disabled", provider.calls)
	}
}

func TestEngineAllFailuresReturnStableMessage(t *testing.T) {
	first := &fakeProvider{name: providerAnySearch, failures: []error{retryableFailure(providerAnySearch)}}
	second := &fakeProvider{name: providerDeepSeek, topics: []string{topicGeneral, topicNews},
		failures: []error{retryableFailure(providerDeepSeek)}}
	engine := newEngineWith(DefaultConfig(), []Provider{first, second})

	result, err := engine.RunModel(context.Background(), "", Request{Query: "sforum"})
	if err != nil {
		t.Fatalf("RunModel returned an internal error: %v", err)
	}
	if !result.IsError || result.Content != messageUnavailable {
		t.Fatalf("result = %+v, want the stable unavailable message", result)
	}
}

// TestEngineTimeBudgetStillTriesASecondBackend 钉死「一次调用内至少能试两个后端」：
// 第一个后端吃满自己的尝试预算后，第二个仍然有剩余时间。
func TestEngineTimeBudgetStillTriesASecondBackend(t *testing.T) {
	if testing.Short() {
		t.Skip("this test intentionally burns one 2500ms attempt budget")
	}
	slow := &fakeProvider{name: providerAnySearch, delay: 5 * time.Second}
	fast := &fakeProvider{name: providerDeepSeek, topics: []string{topicGeneral, topicNews}, results: sampleResults()}
	// 用默认预算：第一次尝试吃满 2500ms 上限后，第二次仍有约 1300ms。
	config := DefaultConfig()
	engine := newEngineWith(config, []Provider{slow, fast})

	started := time.Now()
	result, err := engine.RunModel(context.Background(), "", Request{Query: "sforum"})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("RunModel returned an internal error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected failure: %s", result.Content)
	}
	if fast.calls != 1 {
		t.Fatalf("second backend calls = %d, want 1 (budget must leave room for a second attempt)", fast.calls)
	}
	if elapsed > config.Timeout+300*time.Millisecond {
		t.Errorf("elapsed = %s, want it to stay inside the %s budget", elapsed, config.Timeout)
	}
}

func TestNextAttemptBudget(t *testing.T) {
	cases := []struct {
		remaining time.Duration
		total     time.Duration
		want      time.Duration
	}{
		// 默认 4 秒总预算：单次仍是旧的 2.5 秒上限，保证至少能试两个后端。
		{4000 * time.Millisecond, 4000 * time.Millisecond, minAttemptBudget},
		{2700 * time.Millisecond, 4000 * time.Millisecond, minAttemptBudget},
		{1000 * time.Millisecond, 4000 * time.Millisecond, 800 * time.Millisecond},
		// 运营者为慢后端调大预算：单次跟着放大到总预算的一半，而不是被 2.5 秒截断。
		{60000 * time.Millisecond, 60000 * time.Millisecond, 30000 * time.Millisecond},
		{29000 * time.Millisecond, 29000 * time.Millisecond, 14500 * time.Millisecond},
		{200 * time.Millisecond, 4000 * time.Millisecond, 0},
		{100 * time.Millisecond, 4000 * time.Millisecond, 0},
		{-5 * time.Second, 4000 * time.Millisecond, 0},
	}
	for _, testCase := range cases {
		if got := nextAttemptBudget(testCase.remaining, testCase.total); got != testCase.want {
			t.Errorf("nextAttemptBudget(%s, %s) = %s, want %s",
				testCase.remaining, testCase.total, got, testCase.want)
		}
	}
}

func TestEngineCountResolution(t *testing.T) {
	cases := []struct {
		name       string
		requested  int
		maxResults int
		want       int
	}{
		{"explicit count wins", 3, 5, 3},
		{"missing count falls back to settings", 0, 7, 7},
		{"too large is clamped", 99, 5, 10},
		{"negative falls back then clamps", -1, 0, 1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := &fakeProvider{name: providerAnySearch, results: sampleResults()}
			config := DefaultConfig()
			config.MaxResults = testCase.maxResults
			engine := newEngineWith(config, []Provider{provider})
			if _, err := engine.RunModel(context.Background(), "", Request{Query: "sforum", Count: testCase.requested}); err != nil {
				t.Fatalf("RunModel: %v", err)
			}
			if provider.lastRequest.Count != testCase.want {
				t.Errorf("count = %d, want %d", provider.lastRequest.Count, testCase.want)
			}
		})
	}
}

func TestEngineResolvesLanguageFromLocale(t *testing.T) {
	cases := []struct {
		name     string
		language string
		locale   string
		want     string
	}{
		{"auto follows locale", languageAuto, "zh-CN", "zh"},
		{"auto without locale sends nothing", languageAuto, "", ""},
		{"fixed language wins", "en-US", "zh-CN", "en"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := &fakeProvider{name: providerAnySearch, results: sampleResults()}
			config := DefaultConfig()
			config.Language = testCase.language
			engine := newEngineWith(config, []Provider{provider})
			if _, err := engine.RunModel(context.Background(), testCase.locale, Request{Query: "sforum"}); err != nil {
				t.Fatalf("RunModel: %v", err)
			}
			if provider.lastRequest.Language != testCase.want {
				t.Errorf("language = %q, want %q", provider.lastRequest.Language, testCase.want)
			}
		})
	}
}

func TestEngineProbeReportsBackendAndCount(t *testing.T) {
	provider := &fakeProvider{name: providerAnySearch, results: sampleResults()}
	engine := newEngineWith(DefaultConfig(), []Provider{provider})

	result, err := engine.RunProbe(context.Background())
	if err != nil {
		t.Fatalf("RunProbe returned an internal error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected failure: %s", result.Content)
	}
	for _, want := range []string{"后端=anysearch", "1 条", "首条：SForum 文档"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("probe content %q is missing %q", result.Content, want)
		}
	}
}

func TestEngineProbeBypassesCacheAndEnabledSwitch(t *testing.T) {
	provider := &fakeProvider{name: providerAnySearch, results: sampleResults()}
	config := DefaultConfig()
	config.Enabled = false
	engine := newEngineWith(config, []Provider{provider})

	if _, err := engine.RunModel(context.Background(), "", Request{Query: config.ProbeQuery}); err != nil {
		t.Fatalf("RunModel: %v", err)
	}
	// enabled=false 时 RunModel 不会发起请求，探测必须自己打一次后端。
	result, err := engine.RunProbe(context.Background())
	if err != nil {
		t.Fatalf("RunProbe: %v", err)
	}
	if result.IsError {
		t.Fatalf("probe must ignore the enable switch: %s", result.Content)
	}
	if provider.calls != 1 {
		t.Errorf("provider calls = %d, want 1 (probe ignores the disabled switch)", provider.calls)
	}
}

func TestEngineProbeFailureNamesTheBackend(t *testing.T) {
	provider := &fakeProvider{name: providerAnySearch,
		failures: []error{&searchFailure{Provider: providerAnySearch, Reason: "后端故障（HTTP 500）", Retryable: true}}}
	engine := newEngineWith(DefaultConfig(), []Provider{provider})

	result, err := engine.RunProbe(context.Background())
	if err != nil {
		t.Fatalf("RunProbe returned an internal error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("probe must fail: %+v", result)
	}
	for _, want := range []string{"测试搜索失败", "anysearch", "HTTP 500", "当前配置"} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("probe content %q is missing %q", result.Content, want)
		}
	}
}

func TestEngineReportsInvalidProxyAsExplainableFailure(t *testing.T) {
	config := DefaultConfig()
	config.ProxyURL = "ftp://127.0.0.1:21"
	engine := NewEngine(config)

	result, err := engine.RunModel(context.Background(), "", Request{Query: "sforum"})
	if err != nil {
		t.Fatalf("RunModel returned an internal error: %v", err)
	}
	if !result.IsError {
		t.Fatalf("invalid proxy must fail the call: %+v", result)
	}
}

// TestStatusFailureClassification 钉死「哪些状态码值得换后端」这张表。
func TestStatusFailureClassification(t *testing.T) {
	cases := []struct {
		status        int
		wantRetryable bool
		wantAuth      bool
	}{
		{http.StatusBadRequest, false, false},
		{http.StatusUnauthorized, true, true},
		{http.StatusForbidden, true, true},
		{http.StatusPaymentRequired, true, false},
		{http.StatusNotFound, false, false},
		{http.StatusRequestTimeout, true, false},
		{http.StatusTooManyRequests, true, false},
		{http.StatusInternalServerError, true, false},
		{http.StatusServiceUnavailable, true, false},
		{http.StatusMovedPermanently, true, false},
	}
	for _, testCase := range cases {
		failure := statusFailure("p", testCase.status, "")
		if failure.Retryable != testCase.wantRetryable || failure.Auth != testCase.wantAuth {
			t.Errorf("status %d → retryable=%v auth=%v, want %v/%v",
				testCase.status, failure.Retryable, failure.Auth, testCase.wantRetryable, testCase.wantAuth)
		}
	}
}

// TestEngineEndToEndThroughRealAdapter 走一遍「引擎 → 真实适配器 → 出站守卫 →
// httptest 端点 → 格式化」的完整链路：除了后端域名被换成本机测试服务，
// 其余每一层都是生产代码。
func TestEngineEndToEndThroughRealAdapter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		io.WriteString(writer, `{"data":{"results":[{"title":"SForum","url":"https://sforum.example/docs","snippet":"SForum 是一个插件优先的论坛框架。"}]}}`)
	}))
	defer server.Close()

	config := DefaultConfig()
	provider := newAnySearchProvider(config, testOutbound(t))
	provider.baseURL = server.URL
	engine := newEngineWith(config, []Provider{provider})

	result, err := engine.RunModel(context.Background(), "zh-CN", Request{Query: "sforum 插件", Count: 3})
	if err != nil {
		t.Fatalf("RunModel returned an internal error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected failure: %s", result.Content)
	}
	for _, want := range []string{
		"站外搜索（引擎：AnySearch",
		"1. SForum",
		"   https://sforum.example/docs",
		"SForum 是一个插件优先的论坛框架。",
		"   站点：sforum.example",
	} {
		if !strings.Contains(result.Content, want) {
			t.Errorf("content is missing %q:\n%s", want, result.Content)
		}
	}
}
