package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// 后端标识。它们是设置里 provider_order 的可取值，也是结果头里展示的引擎名。
const (
	providerAnySearch = "anysearch"
	providerDeepSeek  = "deepseek"
	providerTavily    = "tavily"
	providerBrave     = "brave"
)

// 检索主题。general 是默认值；news 只派给声明支持新闻的后端。
const (
	topicGeneral = "general"
	topicNews    = "news"
)

// Request 是一次检索的抽象输入。
//
// 它是引擎与后端之间的唯一契约：后端只负责把自己的 API 参数映射到这些字段，
// 不理解「候选筛选」「主备切换」「熔断」这些编排语义——那些属于 engine.go。
type Request struct {
	Query      string
	Topic      string
	Freshness  string
	Site       string
	Language   string
	SafeSearch string
	Count      int
}

// Result 是一条检索结果。Summary 与 Snippet 分开保留：前者是「给模型读的长摘要」
// （目前只有 DeepSeek 提供），后者是「给界面看的短摘要」。format.go 优先用 Summary。
type Result struct {
	Title   string
	URL     string
	Snippet string
	Summary string
	Date    string
	Site    string
}

// Provider 是一个搜索后端。
//
// Topics 决定能力路由：引擎只会把 topic=news 派给 Topics 里含 news 的后端，
// 因此一个只支持通用检索的后端不会收到它无法表达的请求。
type Provider interface {
	Name() string
	Topics() []string
	Search(ctx context.Context, req Request) ([]Result, error)
}

// 失败分类。Kind 只用于把「为什么没有候选」翻译成给模型/管理员看的那一句话；
// Retryable/Auth 决定熔断与切换行为。
type failureKind int

const (
	failureGeneric failureKind = iota
	failureNoNewsBackend
	failureNoBackend
	failureAllCooling
)

// searchFailure 是一次后端尝试（或一次候选筛选）的失败。
//
// 它同时服务三个读者：引擎（是否切换、冷却多久）、模型（一句可解释的失败）、
// 设置页探测（哪一步、哪个后端、什么原因）。因此 Reason 必须是稳定、可读、
// 且不含密钥的短句。
type searchFailure struct {
	Provider  string
	Status    int
	Reason    string
	Retryable bool
	Auth      bool
	Kind      failureKind
	// Attempts 按尝试顺序记录「后端: 原因」，只给设置页探测用。
	Attempts []string
}

func (f *searchFailure) Error() string { return f.Reason }

// statusFailure 把 HTTP 状态码翻译成失败语义。
//
// 关键判断是「换一个后端有没有用」：
//   - 网络、超时、408/429/5xx、3xx：上游条件，换后端有意义 → 切换；
//   - 401/403：凭据问题，换后端也有意义，但该后端要冷却更久（密钥不会自己变好）；
//   - 400 及其它 4xx：请求构造问题，换谁都一样 → 直接作为可解释失败返回。
func statusFailure(provider string, status int, detail string) *searchFailure {
	failure := &searchFailure{Provider: provider, Status: status}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		failure.Retryable = true
		failure.Auth = true
		failure.Reason = fmt.Sprintf("凭据被拒绝（HTTP %d）", status)
	case status == http.StatusPaymentRequired:
		failure.Retryable = true
		failure.Reason = "账户余额不足（HTTP 402）"
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests:
		failure.Retryable = true
		failure.Reason = fmt.Sprintf("被限流或超时（HTTP %d）", status)
	case status >= 500:
		failure.Retryable = true
		failure.Reason = fmt.Sprintf("后端故障（HTTP %d）", status)
	case status >= 300 && status < 400:
		failure.Retryable = true
		failure.Reason = fmt.Sprintf("被重定向（HTTP %d），已拒绝跟随", status)
	default:
		failure.Retryable = false
		failure.Reason = fmt.Sprintf("请求被拒绝（HTTP %d）", status)
	}
	if detail != "" {
		failure.Reason += "：" + detail
	}
	return failure
}

// transportFailure 把传输层错误翻译成失败语义。端点地址不合法属于配置问题
// （不切换），其余（网络、超时、响应过大）都属于上游条件（切换）。
func transportFailure(provider string, err error) *searchFailure {
	var endpoint endpointError
	if errors.As(err, &endpoint) {
		return &searchFailure{Provider: provider, Reason: endpoint.Error(), Retryable: false}
	}
	var tooLarge responseTooLargeError
	if errors.As(err, &tooLarge) {
		return &searchFailure{Provider: provider, Reason: tooLarge.Error(), Retryable: true}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return &searchFailure{Provider: provider, Reason: "请求超时", Retryable: true}
	}
	return &searchFailure{Provider: provider, Reason: "网络错误：" + shortNetworkError(err), Retryable: true}
}

// decodeFailure 表示响应体不是预期的 JSON。它属于上游条件：同一份请求换一个
// 后端仍然可能成功。
func decodeFailure(provider string) *searchFailure {
	return &searchFailure{Provider: provider, Reason: "响应不是预期的 JSON", Retryable: true}
}

func asSearchFailure(provider string, err error) *searchFailure {
	var failure *searchFailure
	if errors.As(err, &failure) {
		return failure
	}
	return transportFailure(provider, err)
}

// providerErrorText 尽力从各家错误响应体里取出人可读的原因。
//
// 四家的错误形状不同（DeepSeek 是 OpenAI 兼容的 error.message，Tavily 是
// detail.error 或 detail 数组，Brave 是 error.detail，AnySearch 未公开），
// 这里用一个宽口径解析器覆盖它们，而不是给每家写一份只用来取一个字符串的代码。
func providerErrorText(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var generic struct {
		Error   json.RawMessage `json:"error"`
		Detail  json.RawMessage `json:"detail"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(body, &generic); err != nil {
		return ""
	}
	if text := trimErrorText(generic.Message); text != "" {
		return text
	}
	if text := rawMessageText(generic.Error); text != "" {
		return text
	}
	return rawMessageText(generic.Detail)
}

// rawMessageText 处理三种常见形态：字符串、带 message/detail 的对象、数组。
func rawMessageText(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return trimErrorText(asString)
	}
	var asObject struct {
		Message string          `json:"message"`
		Detail  json.RawMessage `json:"detail"`
		Error   string          `json:"error"`
	}
	if err := json.Unmarshal(raw, &asObject); err == nil {
		if text := trimErrorText(asObject.Message); text != "" {
			return text
		}
		if text := trimErrorText(asObject.Error); text != "" {
			return text
		}
		if text := rawMessageText(asObject.Detail); text != "" {
			return text
		}
	}
	var asArray []json.RawMessage
	if err := json.Unmarshal(raw, &asArray); err == nil {
		for _, item := range asArray {
			if text := rawMessageText(item); text != "" {
				return text
			}
		}
	}
	return ""
}

func trimErrorText(raw string) string {
	text := sanitizeText(raw)
	if len(text) > 160 {
		text = clampBytes(text, 160)
	}
	return text
}

// normalizeTopic 把模型给的 topic 收敛到已知取值。未知值回落 general：
// 一个写错的 topic 不该让整次检索失败，它只是让结果范围变大。
func normalizeTopic(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case topicNews:
		return topicNews
	default:
		return topicGeneral
	}
}

// normalizeFreshness 把模型给的时间范围收敛到 day/week/month/year。
// 空值与未知值都表示「不限时间」。
func normalizeFreshness(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "day":
		return "day"
	case "week":
		return "week"
	case "month":
		return "month"
	case "year":
		return "year"
	default:
		return ""
	}
}

// normalizeSite 把模型给的域名收敛成一个裸域名。
//
// 模型可能给出 "docs.example.com"、也可能给出完整 URL（"https://docs.example.com/a"）。
// 两种都接受，但只取主机名：site 参数的语义是「限定单一域名」，路径没有意义。
func normalizeSite(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	trimmed = strings.TrimPrefix(trimmed, "site:")
	trimmed = strings.TrimSpace(trimmed)
	if strings.Contains(trimmed, "://") {
		if parsed, err := url.Parse(trimmed); err == nil && parsed.Hostname() != "" {
			trimmed = parsed.Hostname()
		}
	}
	trimmed = strings.TrimSuffix(trimmed, "/")
	return strings.ToLower(strings.TrimSpace(trimmed))
}

// providerSupportsTopic 判断一个后端能否表达该 topic。
func providerSupportsTopic(provider Provider, topic string) bool {
	for _, supported := range provider.Topics() {
		if supported == topic {
			return true
		}
	}
	return false
}

// hostOf 取 URL 的主机名，用作结果的站点标签。
func hostOf(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

// displayProviderName 是结果头里展示的引擎名。
func displayProviderName(name string) string {
	switch name {
	case providerAnySearch:
		return "AnySearch"
	case providerDeepSeek:
		return "DeepSeek"
	case providerTavily:
		return "Tavily"
	case providerBrave:
		return "Brave"
	default:
		if name == "" {
			return "未知"
		}
		return name
	}
}
