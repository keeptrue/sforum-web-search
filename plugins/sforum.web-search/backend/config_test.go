package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestLoadConfigUsesDefaultsWhenNothingIsSaved(t *testing.T) {
	config := LoadConfig(func(string) string { return "" })
	want := DefaultConfig()

	if config.Enabled != want.Enabled || !config.Enabled {
		t.Fatalf("enabled = %v, want true", config.Enabled)
	}
	if config.ProviderOrder != defaultProviderOrder {
		t.Fatalf("provider_order = %q", config.ProviderOrder)
	}
	if config.Strategy != strategyPriority {
		t.Fatalf("strategy = %q", config.Strategy)
	}
	if config.AnySearchKey != "" || config.DeepSeekKey != "" || config.TavilyKey != "" || config.BraveKey != "" {
		t.Fatalf("api keys must default to empty")
	}
	if config.AnySearchZone != zoneAuto {
		t.Fatalf("anysearch_zone = %q", config.AnySearchZone)
	}
	if config.Language != languageAuto {
		t.Fatalf("language = %q", config.Language)
	}
	if config.SafeSearch != safeSearchModerate {
		t.Fatalf("safe_search = %q", config.SafeSearch)
	}
	if config.MaxResults != defaultResults {
		t.Fatalf("max_results = %d", config.MaxResults)
	}
	if config.Timeout != defaultTimeoutMS*time.Millisecond {
		t.Fatalf("timeout = %s", config.Timeout)
	}
	if config.CacheTTLSec != defaultCacheTTLSec {
		t.Fatalf("cache_ttl_s = %d", config.CacheTTLSec)
	}
	if config.ProxyURL != "" || config.AllowHTTP || config.AllowPrivateEndpoint {
		t.Fatalf("network defaults = %q/%v/%v", config.ProxyURL, config.AllowHTTP, config.AllowPrivateEndpoint)
	}
	if config.ProbeQuery != defaultProbeQuery {
		t.Fatalf("probe_query = %q", config.ProbeQuery)
	}
}

func TestLoadConfigReadsInjectedSettings(t *testing.T) {
	env := map[string]string{
		"SFORUM_SETTING_ENABLED":                "false",
		"SFORUM_SETTING_PROVIDER_ORDER":         " brave , deepseek ,unknown,brave ",
		"SFORUM_SETTING_STRATEGY":               "round_robin",
		"SFORUM_SETTING_ANYSEARCH_API_KEY":      " as-key ",
		"SFORUM_SETTING_ANYSEARCH_ZONE":         "intl",
		"SFORUM_SETTING_DEEPSEEK_API_KEY":       "ds-key",
		"SFORUM_SETTING_TAVILY_API_KEY":         "tvly-key",
		"SFORUM_SETTING_BRAVE_API_KEY":          "brave-key",
		"SFORUM_SETTING_LANGUAGE":               "zh-CN",
		"SFORUM_SETTING_SAFE_SEARCH":            "strict",
		"SFORUM_SETTING_MAX_RESULTS":            "8",
		"SFORUM_SETTING_TIMEOUT_MS":             "2500",
		"SFORUM_SETTING_CACHE_TTL_S":            "30",
		"SFORUM_SETTING_PROXY_URL":              " socks5://127.0.0.1:1080 ",
		"SFORUM_SETTING_ALLOW_HTTP":             "true",
		"SFORUM_SETTING_ALLOW_PRIVATE_ENDPOINT": "true",
		"SFORUM_SETTING_PROBE_QUERY":            "SForum 插件",
	}
	config := LoadConfig(func(key string) string { return env[key] })

	if config.Enabled {
		t.Error("enabled = true, want false")
	}
	if config.ProviderOrder != " brave , deepseek ,unknown,brave " {
		t.Errorf("provider_order must be stored raw: %q", config.ProviderOrder)
	}
	if got := config.orderedProviders(); len(got) != 2 || got[0] != providerBrave || got[1] != providerDeepSeek {
		t.Errorf("orderedProviders = %v, want [brave deepseek]", got)
	}
	if config.Strategy != strategyRoundRobin {
		t.Errorf("strategy = %q", config.Strategy)
	}
	if config.AnySearchKey != "as-key" || config.DeepSeekKey != "ds-key" ||
		config.TavilyKey != "tvly-key" || config.BraveKey != "brave-key" {
		t.Errorf("keys = %q/%q/%q/%q", config.AnySearchKey, config.DeepSeekKey, config.TavilyKey, config.BraveKey)
	}
	if config.AnySearchZone != "intl" {
		t.Errorf("zone = %q", config.AnySearchZone)
	}
	if config.Language != "zh-CN" {
		t.Errorf("language = %q", config.Language)
	}
	if config.SafeSearch != safeSearchStrict {
		t.Errorf("safe_search = %q", config.SafeSearch)
	}
	if config.MaxResults != 8 {
		t.Errorf("max_results = %d", config.MaxResults)
	}
	if config.Timeout != 2500*time.Millisecond {
		t.Errorf("timeout = %s", config.Timeout)
	}
	if config.CacheTTLSec != 30 {
		t.Errorf("cache_ttl_s = %d", config.CacheTTLSec)
	}
	if config.ProxyURL != "socks5://127.0.0.1:1080" {
		t.Errorf("proxy_url = %q, want trimmed value", config.ProxyURL)
	}
	if !config.AllowHTTP || !config.AllowPrivateEndpoint {
		t.Errorf("network flags = %v/%v", config.AllowHTTP, config.AllowPrivateEndpoint)
	}
	if config.ProbeQuery != "SForum 插件" {
		t.Errorf("probe_query = %q", config.ProbeQuery)
	}
}

func TestLoadConfigClampsOutOfRangeNumbers(t *testing.T) {
	cases := []struct {
		name      string
		env       map[string]string
		wantTime  time.Duration
		wantCount int
		wantTTL   int
	}{
		{
			name:      "too small",
			env:       map[string]string{"SFORUM_SETTING_TIMEOUT_MS": "1", "SFORUM_SETTING_MAX_RESULTS": "0", "SFORUM_SETTING_CACHE_TTL_S": "-5"},
			wantTime:  minTimeoutMS * time.Millisecond,
			wantCount: minResults,
			wantTTL:   minCacheTTLSec,
		},
		{
			name:      "too large",
			env:       map[string]string{"SFORUM_SETTING_TIMEOUT_MS": "999999", "SFORUM_SETTING_MAX_RESULTS": "999", "SFORUM_SETTING_CACHE_TTL_S": "999999"},
			wantTime:  maxTimeoutMS * time.Millisecond,
			wantCount: maxResults,
			wantTTL:   maxCacheTTLSec,
		},
		{
			name:      "not a number falls back to defaults",
			env:       map[string]string{"SFORUM_SETTING_TIMEOUT_MS": "soon", "SFORUM_SETTING_MAX_RESULTS": "many", "SFORUM_SETTING_CACHE_TTL_S": "later"},
			wantTime:  defaultTimeoutMS * time.Millisecond,
			wantCount: defaultResults,
			wantTTL:   defaultCacheTTLSec,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := LoadConfig(func(key string) string { return testCase.env[key] })
			if config.Timeout != testCase.wantTime {
				t.Errorf("timeout = %s, want %s", config.Timeout, testCase.wantTime)
			}
			if config.MaxResults != testCase.wantCount {
				t.Errorf("max_results = %d, want %d", config.MaxResults, testCase.wantCount)
			}
			if config.CacheTTLSec != testCase.wantTTL {
				t.Errorf("cache_ttl_s = %d, want %d", config.CacheTTLSec, testCase.wantTTL)
			}
		})
	}
}

func TestLoadConfigNormalizesEnums(t *testing.T) {
	cases := map[string]string{
		"SFORUM_SETTING_STRATEGY":       "RANDOM",
		"SFORUM_SETTING_ANYSEARCH_ZONE": "mars",
		"SFORUM_SETTING_SAFE_SEARCH":    "extreme",
	}
	config := LoadConfig(func(key string) string { return cases[key] })
	if config.Strategy != strategyPriority {
		t.Errorf("strategy = %q, want priority fallback", config.Strategy)
	}
	if config.AnySearchZone != zoneAuto {
		t.Errorf("zone = %q, want auto fallback", config.AnySearchZone)
	}
	if config.SafeSearch != safeSearchModerate {
		t.Errorf("safe_search = %q, want moderate fallback", config.SafeSearch)
	}
}

// TestTimeoutStaysUnderDeclaredToolTimeout 钉死一个很容易被改坏的边界：
// 插件自己的检索预算必须严格小于清单声明的 timeoutMs，否则宿主会在插件还
// 没来得及返回结果时就掐掉整次调用。
func TestTimeoutStaysUnderDeclaredToolTimeout(t *testing.T) {
	if maxTimeoutMS >= declaredTimeoutMS {
		t.Fatalf("max timeout %dms must stay under declared tool timeout %dms", maxTimeoutMS, declaredTimeoutMS)
	}
	// 最紧的总预算下，第一次尝试也必须留出返回余量：否则一次调用连一次尝试
	// 都用不满就被自己的预算掐掉。
	total := time.Duration(minTimeoutMS) * time.Millisecond
	if budget := nextAttemptBudget(total, total); budget <= 0 || budget >= total {
		t.Fatalf("attempt budget at the minimum total budget = %s, want it inside (0, %dms)", budget, minTimeoutMS)
	}
}

func TestSettingEnvNameMatchesHostMapping(t *testing.T) {
	cases := map[string]string{
		"enabled":                "SFORUM_SETTING_ENABLED",
		"provider_order":         "SFORUM_SETTING_PROVIDER_ORDER",
		"anysearch_api_key":      "SFORUM_SETTING_ANYSEARCH_API_KEY",
		"allow_private_endpoint": "SFORUM_SETTING_ALLOW_PRIVATE_ENDPOINT",
		"timeout_ms":             "SFORUM_SETTING_TIMEOUT_MS",
	}
	for key, want := range cases {
		if got := settingEnvName(key); got != want {
			t.Fatalf("settingEnvName(%q) = %q, want %q", key, got, want)
		}
	}
}

// TestManifestDefaultsMatchRuntimeDefaults 是跨文件的漂移钉死：清单里的 default
// 是「运营者什么都没改」时宿主展示的值，代码里的默认值是「宿主没注入任何设置」
// 时实际生效的值。两者分叉过一次就再也不会被发现，所以用测试钉住。
func TestManifestDefaultsMatchRuntimeDefaults(t *testing.T) {
	path := filepath.Join("..", "manifest", "settings.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var document struct {
		Fields []struct {
			Key     string `json:"key"`
			Default string `json:"default"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	defaults := DefaultConfig()
	want := map[string]string{
		"enabled":                strconv.FormatBool(defaults.Enabled),
		"provider_order":         defaults.ProviderOrder,
		"strategy":               defaults.Strategy,
		"anysearch_api_key":      defaults.AnySearchKey,
		"anysearch_zone":         defaults.AnySearchZone,
		"deepseek_api_key":       defaults.DeepSeekKey,
		"tavily_api_key":         defaults.TavilyKey,
		"brave_api_key":          defaults.BraveKey,
		"language":               defaults.Language,
		"safe_search":            defaults.SafeSearch,
		"max_results":            strconv.Itoa(defaults.MaxResults),
		"timeout_ms":             strconv.FormatInt(defaults.Timeout.Milliseconds(), 10),
		"cache_ttl_s":            strconv.Itoa(defaults.CacheTTLSec),
		"proxy_url":              defaults.ProxyURL,
		"allow_http":             strconv.FormatBool(defaults.AllowHTTP),
		"allow_private_endpoint": strconv.FormatBool(defaults.AllowPrivateEndpoint),
		"probe_query":            defaults.ProbeQuery,
	}
	seen := map[string]bool{}
	for _, field := range document.Fields {
		expected, ok := want[field.Key]
		if !ok {
			t.Errorf("manifest/settings.json declares %q, but the runtime reads no such setting", field.Key)
			continue
		}
		seen[field.Key] = true
		if field.Default != expected {
			t.Errorf("manifest default for %q = %q, runtime default = %q", field.Key, field.Default, expected)
		}
	}
	for key := range want {
		if !seen[key] {
			t.Errorf("manifest/settings.json no longer declares %q, but the runtime reads it", key)
		}
	}
}

// und 是 BCP-47 的「未确定」，不是一种语言：它必须被当作「没有语言偏好」，
// 否则 language=auto 且宿主没有站点语言时，请求会带一个无意义的语言码。
func TestPrimaryLanguageSubtagRejectsUndetermined(t *testing.T) {
	for _, raw := range []string{"und", "UND", " zh-CN ", "zh_CN"} {
		got := primaryLanguageSubtag(raw)
		if raw == "und" || raw == "UND" {
			if got != "" {
				t.Fatalf("primaryLanguageSubtag(%q) = %q，应当为空", raw, got)
			}
			continue
		}
		if got != "zh" {
			t.Fatalf("primaryLanguageSubtag(%q) = %q", raw, got)
		}
	}
}
