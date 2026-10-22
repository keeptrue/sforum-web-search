package main

import (
	"strconv"
	"strings"
	"time"
)

// Config 是插件运行期配置。
//
// 来源只有一个：宿主在启动插件子进程时把已保存的设置注入成 SFORUM_SETTING_*
// 环境变量（host: Support/Extensions/plugin_command_transport.go 的
// pluginSettingEnvName）。从未保存过的设置不会被注入，所以下面的默认值必须与
// manifest/settings.json 的 default 一致——否则「运营者什么都没改」和「运营者点了
// 重置」会得到两种不同的行为。这个一致性由 config_test.go 钉死。
//
// 设置保存会让宿主重启本插件的精确制品，因此改完设置不需要重启 API。
type Config struct {
	Enabled              bool
	ProviderOrder        string
	Strategy             string
	AnySearchKey         string
	AnySearchZone        string
	DeepSeekKey          string
	TavilyKey            string
	BraveKey             string
	Language             string
	SafeSearch           string
	MaxResults           int
	Timeout              time.Duration
	CacheTTLSec          int
	ProxyURL             string
	AllowHTTP            bool
	AllowPrivateEndpoint bool
	ProbeQuery           string
}

const (
	// 单次检索的总预算。宿主会拿清单声明的 timeoutMs（30000）作为调用截止时间，
	// 插件自己必须留出封装与返回的余量，所以上限少 1 秒。
	minTimeoutMS = 1000
	// maxTimeoutMS 比清单声明的 30000ms 少留 1 秒：插件必须在宿主截止时间之前
	// 返回结果，否则运营者看到的是「调用超时」而不是插件给出的失败原因。
	maxTimeoutMS     = 29_000
	defaultTimeoutMS = 4000

	minResults     = 1
	maxResults     = 10
	defaultResults = 5

	// 缓存时长上限一天：再长就只是把「搜索结果过时」变成默认行为。
	minCacheTTLSec     = 0
	maxCacheTTLSec     = 86400
	defaultCacheTTLSec = 600

	defaultProviderOrder = "anysearch,deepseek,tavily,brave"
	defaultProbeQuery    = "SForum"

	strategyPriority   = "priority"
	strategyRoundRobin = "round_robin"

	zoneAuto = "auto"

	safeSearchOff      = "off"
	safeSearchModerate = "moderate"
	safeSearchStrict   = "strict"

	languageAuto = "auto"
)

// settingsEnvPrefix 与宿主的 pluginSettingEnvName 同源：SFORUM_SETTING_ +
// 大写的 key，非 [A-Z0-9] 字符替换成下划线。
const settingsEnvPrefix = "SFORUM_SETTING_"

// DefaultConfig 返回「什么都没配」时的默认：开关打开、顺序为四家默认顺序、
// 没有任何 API Key。此时 AnySearch 仍可按匿名模式工作，其余三家被跳过。
func DefaultConfig() Config {
	return Config{
		Enabled:              true,
		ProviderOrder:        defaultProviderOrder,
		Strategy:             strategyPriority,
		AnySearchKey:         "",
		AnySearchZone:        zoneAuto,
		DeepSeekKey:          "",
		TavilyKey:            "",
		BraveKey:             "",
		Language:             languageAuto,
		SafeSearch:           safeSearchModerate,
		MaxResults:           defaultResults,
		Timeout:              defaultTimeoutMS * time.Millisecond,
		CacheTTLSec:          defaultCacheTTLSec,
		ProxyURL:             "",
		AllowHTTP:            false,
		AllowPrivateEndpoint: false,
		ProbeQuery:           defaultProbeQuery,
	}
}

// LoadConfig 从环境变量装载配置，缺项与非法项一律回落到默认值。
//
// 「非法值回落而不是报错」是有意的：一个写错的数字不该让工具变成不可用，
// 而应该让它按默认值继续工作——运营者在设置页看到的说明里写明了取值范围。
func LoadConfig(getenv func(string) string) Config {
	config := DefaultConfig()
	if getenv == nil {
		return config
	}
	if raw, ok := readSetting(getenv, "enabled"); ok {
		config.Enabled = parseBool(raw, config.Enabled)
	}
	if raw, ok := readSetting(getenv, "provider_order"); ok {
		config.ProviderOrder = raw
	}
	if raw, ok := readSetting(getenv, "strategy"); ok {
		config.Strategy = normalizeStrategy(raw)
	}
	if raw, ok := readSetting(getenv, "anysearch_api_key"); ok {
		config.AnySearchKey = strings.TrimSpace(raw)
	}
	if raw, ok := readSetting(getenv, "anysearch_zone"); ok {
		config.AnySearchZone = normalizeZone(raw)
	}
	if raw, ok := readSetting(getenv, "deepseek_api_key"); ok {
		config.DeepSeekKey = strings.TrimSpace(raw)
	}
	if raw, ok := readSetting(getenv, "tavily_api_key"); ok {
		config.TavilyKey = strings.TrimSpace(raw)
	}
	if raw, ok := readSetting(getenv, "brave_api_key"); ok {
		config.BraveKey = strings.TrimSpace(raw)
	}
	if raw, ok := readSetting(getenv, "language"); ok {
		config.Language = normalizeLanguage(raw)
	}
	if raw, ok := readSetting(getenv, "safe_search"); ok {
		config.SafeSearch = normalizeSafeSearch(raw)
	}
	if raw, ok := readSetting(getenv, "max_results"); ok {
		config.MaxResults = clampInt(parseInt(raw, defaultResults), minResults, maxResults)
	}
	if raw, ok := readSetting(getenv, "timeout_ms"); ok {
		config.Timeout = time.Duration(clampInt(parseInt(raw, defaultTimeoutMS), minTimeoutMS, maxTimeoutMS)) * time.Millisecond
	}
	if raw, ok := readSetting(getenv, "cache_ttl_s"); ok {
		config.CacheTTLSec = clampInt(parseInt(raw, defaultCacheTTLSec), minCacheTTLSec, maxCacheTTLSec)
	}
	if raw, ok := readSetting(getenv, "proxy_url"); ok {
		config.ProxyURL = strings.TrimSpace(raw)
	}
	if raw, ok := readSetting(getenv, "allow_http"); ok {
		config.AllowHTTP = parseBool(raw, config.AllowHTTP)
	}
	if raw, ok := readSetting(getenv, "allow_private_endpoint"); ok {
		config.AllowPrivateEndpoint = parseBool(raw, config.AllowPrivateEndpoint)
	}
	if raw, ok := readSetting(getenv, "probe_query"); ok {
		if trimmed := strings.TrimSpace(raw); trimmed != "" {
			config.ProbeQuery = trimmed
		}
	}
	return config
}

// orderedProviders 解析 provider_order：英文逗号分隔、大小写不敏感、
// 未知名字忽略、重复名字只保留第一次出现。
func (c Config) orderedProviders() []string {
	order := make([]string, 0, 4)
	seen := make(map[string]bool, 4)
	for _, raw := range strings.Split(c.ProviderOrder, ",") {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" || seen[name] || !knownProvider(name) {
			continue
		}
		seen[name] = true
		order = append(order, name)
	}
	return order
}

// resolveLanguage 把「设置里的 language」与「工具调用带来的 locale」合成一个
// 主语言子标签（zh-CN → zh）。auto 时优先 locale；两者都不可用时返回空串，
// 让各后端使用自己的默认语言。
func (c Config) resolveLanguage(locale string) string {
	candidate := c.Language
	if candidate == "" || candidate == languageAuto {
		candidate = locale
	}
	return primaryLanguageSubtag(candidate)
}

// primaryLanguageSubtag 只接受 2~3 个 ASCII 字母的语言码。
// 其它值（und、数字串、空串）一律当作「没有语言偏好」。
func primaryLanguageSubtag(raw string) string {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return ""
	}
	// und 是 BCP-47 的「未确定」，不是一种语言：把它当语言码发给后端只会让
	// 检索语言变成一个无意义的值。
	if trimmed == "und" {
		return ""
	}
	if index := strings.IndexAny(trimmed, "-_"); index > 0 {
		trimmed = trimmed[:index]
	}
	if len(trimmed) < 2 || len(trimmed) > 3 {
		return ""
	}
	for _, char := range trimmed {
		if char < 'a' || char > 'z' {
			return ""
		}
	}
	return trimmed
}

func normalizeStrategy(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case strategyRoundRobin:
		return strategyRoundRobin
	default:
		return strategyPriority
	}
}

func normalizeZone(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "cn":
		return "cn"
	case "intl":
		return "intl"
	default:
		return zoneAuto
	}
}

func normalizeSafeSearch(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case safeSearchOff:
		return safeSearchOff
	case safeSearchStrict:
		return safeSearchStrict
	default:
		return safeSearchModerate
	}
}

func normalizeLanguage(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.EqualFold(trimmed, languageAuto) {
		return languageAuto
	}
	return trimmed
}

// settingEnvName 复刻宿主的键名映射，供测试钉死两边的约定不漂移。
func settingEnvName(key string) string {
	var builder strings.Builder
	builder.WriteString(settingsEnvPrefix)
	for _, char := range strings.ToUpper(strings.TrimSpace(key)) {
		switch {
		case char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
			builder.WriteRune(char)
		default:
			builder.WriteByte('_')
		}
	}
	return builder.String()
}

func readSetting(getenv func(string) string, key string) (string, bool) {
	value := getenv(settingEnvName(key))
	// 宿主对「从未保存」与「保存为空串」注入的都是空值，两者在这里无法区分。
	// 这不影响任何字段的语义：每个字段的默认值都与空值等价，唯一的例外是
	// probe_query（空值回落成默认检索词），它由 handler 自己再校验一次。
	if value == "" {
		return "", false
	}
	return value, true
}

func parseBool(raw string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return fallback
	}
}

func parseInt(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return value
}

func clampInt(value, minimum, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}
