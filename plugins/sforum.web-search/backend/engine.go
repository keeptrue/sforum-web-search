package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	pluginv2 "github.com/zhuchunshu/sforum/apps/api/sdk/plugin/v2"
)

const (
	// minAttemptBudget 是单次尝试的下限预算。总预算较小时（默认 4000ms）它保证
	// 一次调用内至少能试两个后端：第一次 2500ms、第二次最多 1300ms。
	minAttemptBudget = 2500 * time.Millisecond
	// probeBudget 是设置页「测试搜索」的硬预算。宿主对 settings action 有 15 秒
	// 上限，探测必须在这之前返回，否则运营者只会看到一句「暂不可用」。
	probeBudget = 10 * time.Second
	// attemptReserve 是留给「封装、切换与返回」的余量：尝试预算永远从剩余时间里
	// 再扣掉它，否则最后一次尝试会用光宿主给的截止时间，插件来不及回结果。
	attemptReserve = 200 * time.Millisecond
	// breakerCooldown 是普通失败后的冷却时间；凭据失败冷却更久，
	// 因为密钥不会在 60 秒内自己变好。
	breakerCooldown     = 60 * time.Second
	breakerAuthCooldown = 300 * time.Second
	// cacheCapacity 是进程内 LRU 的容量（条）。
	cacheCapacity = 256
)

// 给模型看的稳定文案。它们是提示词的一部分，改动会影响模型行为，因此集中在这里。
const (
	messageDisabled    = "联网搜索功能当前已被管理员关闭。请基于已有信息回答。"
	messageUnavailable = "联网搜索当前不可用，请基于已有信息回答。"
	messageNoNewsTool  = "当前没有启用支持新闻检索的搜索后端。可以改用 topic=general 再试一次。"
	messageEmptyResult = "没有找到相关结果。可以换一个更具体的检索词，或放宽 freshness/site 限制。"
)

// knownProviders 是 provider_order 允许出现的名字。未知名字被忽略而不是报错：
// 一个拼错的名字不该让整个搜索不可用。
var knownProviders = map[string]bool{
	providerAnySearch: true,
	providerDeepSeek:  true,
	providerTavily:    true,
	providerBrave:     true,
}

func knownProvider(name string) bool { return knownProviders[name] }

// buildProviders 按 provider_order 装配后端。
//
// 缺 key 且不支持匿名访问的后端在这里就被跳过：DeepSeek/Tavily/Brave 必须配 key，
// AnySearch 可以匿名调用。跳过发生在装配期而不是调用期，是为了让「没有配置的后端」
// 不出现在熔断与候选逻辑里，也不占用一次尝试预算。
func buildProviders(config Config, out *outbound) []Provider {
	providers := make([]Provider, 0, 4)
	for _, name := range config.orderedProviders() {
		switch name {
		case providerAnySearch:
			providers = append(providers, newAnySearchProvider(config, out))
		case providerDeepSeek:
			if strings.TrimSpace(config.DeepSeekKey) == "" {
				continue
			}
			providers = append(providers, newDeepSeekProvider(config, out))
		case providerTavily:
			if strings.TrimSpace(config.TavilyKey) == "" {
				continue
			}
			providers = append(providers, newTavilyProvider(config, out))
		case providerBrave:
			if strings.TrimSpace(config.BraveKey) == "" {
				continue
			}
			providers = append(providers, newBraveProvider(config, out))
		}
	}
	return providers
}

// breakerState 是一个后端的熔断状态：在 until 之前不再派发。
type breakerState struct{ until time.Time }

// Engine 持有进程生命周期内不变的配置、后端集合、缓存与熔断状态。
//
// 配置变化（运营者保存设置）会让宿主重启本插件的精确制品，因此这里不需要热重载；
// 设置页的「测试搜索」也是用一个带草稿环境的短命进程完成的。
type Engine struct {
	config    Config
	providers []Provider
	cache     *resultCache
	client    *http.Client
	// clientErr 记录代理配置错误：它不让进程起不来，而是在每次调用时变成
	// 一条可解释的失败。
	clientErr error

	mu       sync.Mutex
	breakers map[string]breakerState
	cursor   int
	now      func() time.Time
}

// NewEngine 构造生产引擎。
func NewEngine(config Config) *Engine {
	client, err := newProviderClient(config)
	engine := &Engine{
		config:    config,
		cache:     newResultCache(cacheCapacity),
		client:    client,
		clientErr: err,
		breakers:  map[string]breakerState{},
		now:       time.Now,
	}
	if err != nil {
		return engine
	}
	engine.providers = buildProviders(config, &outbound{client: client, config: config})
	return engine
}

// newEngineWith 用注入的后端构造引擎，供测试替换掉真实网络。
func newEngineWith(config Config, providers []Provider) *Engine {
	return &Engine{
		config:    config,
		providers: providers,
		cache:     newResultCache(cacheCapacity),
		breakers:  map[string]breakerState{},
		now:       time.Now,
	}
}

// searchOutcome 是一次检索的完整事实（含「谁答的」与是否命中缓存）。
type searchOutcome struct {
	Provider string
	Results  []Result
	Elapsed  time.Duration
	Cached   bool
}

// RunModel 服务模型-facing 的 web-search 工具。
func (e *Engine) RunModel(ctx context.Context, locale string, req Request) (*pluginv2.AIToolResult, error) {
	if !e.config.Enabled {
		return explainable(messageDisabled), nil
	}
	request := e.prepareRequest(req, locale)
	outcome, err := e.search(ctx, request, true, e.config.Timeout)
	if err != nil {
		failure := asSearchFailure("", err)
		if failure.Kind == failureNoNewsBackend {
			return explainable(messageNoNewsTool), nil
		}
		return explainable(messageUnavailable), nil
	}
	results := dedupeResults(outcome.Results)
	if len(results) == 0 {
		return &pluginv2.AIToolResult{Content: messageEmptyResult}, nil
	}
	return &pluginv2.AIToolResult{Content: renderResults(outcome, request, results)}, nil
}

// RunProbe 服务设置页的「测试搜索」。它不受 enabled 开关影响：探测要回答的是
// 「按当前草稿值能不能搜到」，而不是「现在生效了吗」。它也不读写缓存——探测
// 必须真实打一次后端，否则运营者看到的是上一次的结果。
func (e *Engine) RunProbe(ctx context.Context) (*pluginv2.AIToolResult, error) {
	query := strings.TrimSpace(e.config.ProbeQuery)
	if query == "" {
		return explainable("测试检索词为空。请先在「测试检索词」里填一个词。"), nil
	}
	request := e.prepareRequest(Request{Query: query}, "")
	outcome, err := e.search(ctx, request, false, minDuration(e.config.Timeout, probeBudget))
	if err != nil {
		failure := asSearchFailure("", err)
		return explainable("测试搜索失败：" + failure.describe() + "\n\n" + e.describeConfiguration()), nil
	}
	results := dedupeResults(outcome.Results)
	if len(results) == 0 {
		return &pluginv2.AIToolResult{Content: fmt.Sprintf(
			"后端=%s · 0 条 · %dms · 没有找到结果", outcome.Provider, outcome.Elapsed.Milliseconds())}, nil
	}
	first := sanitizeText(results[0].Title)
	if first == "" {
		first = results[0].URL
	}
	return &pluginv2.AIToolResult{Content: fmt.Sprintf(
		"后端=%s · %d 条 · %dms · 首条：%s",
		outcome.Provider, len(results), outcome.Elapsed.Milliseconds(), clampText(first, 60))}, nil
}

// prepareRequest 把工具参数与设置合成一份规范化的检索请求。
func (e *Engine) prepareRequest(req Request, locale string) Request {
	prepared := Request{
		Query:      strings.TrimSpace(req.Query),
		Topic:      normalizeTopic(req.Topic),
		Freshness:  normalizeFreshness(req.Freshness),
		Site:       normalizeSite(req.Site),
		Language:   e.config.resolveLanguage(locale),
		SafeSearch: e.config.SafeSearch,
		Count:      req.Count,
	}
	if prepared.Count <= 0 {
		prepared.Count = e.config.MaxResults
	}
	prepared.Count = clampInt(prepared.Count, minResults, maxResults)
	return prepared
}

// search 是编排核心：候选筛选 → 主备/轮询 → 时间预算 → 熔断 → 缓存。
func (e *Engine) search(ctx context.Context, req Request, allowCache bool, total time.Duration) (*searchOutcome, error) {
	if e.clientErr != nil {
		return nil, &searchFailure{
			Reason: fmt.Sprintf("出站代理配置无效：%v", e.clientErr), Retryable: false,
		}
	}
	cacheKey := ""
	if allowCache && e.config.CacheTTLSec > 0 {
		cacheKey = cacheKeyFor(req)
		if entry, ok := e.cache.Get(cacheKey, e.now()); ok {
			return &searchOutcome{Provider: entry.Provider, Results: entry.Results, Cached: true}, nil
		}
	}

	order, failure := e.attemptOrder(req.Topic)
	if failure != nil {
		return nil, failure
	}

	deadline := e.now().Add(total)
	started := e.now()
	attempts := make([]string, 0, len(order))
	var last *searchFailure
	for _, provider := range order {
		attemptBudget := nextAttemptBudget(deadline.Sub(e.now()), total)
		if attemptBudget <= 0 {
			break
		}
		attemptCtx, cancel := context.WithTimeout(ctx, attemptBudget)
		results, err := provider.Search(attemptCtx, req)
		cancel()
		if err == nil {
			e.clearBreaker(provider.Name())
			outcome := &searchOutcome{
				Provider: provider.Name(),
				Results:  results,
				Elapsed:  e.now().Sub(started),
			}
			if cacheKey != "" {
				e.cache.Put(cacheKey, cacheEntry{
					Provider: outcome.Provider,
					Results:  results,
					Expires:  e.now().Add(time.Duration(e.config.CacheTTLSec) * time.Second),
				})
			}
			return outcome, nil
		}
		failure := asSearchFailure(provider.Name(), err)
		attempts = append(attempts, provider.Name()+": "+failure.Reason)
		last = failure
		if !failure.Retryable {
			last.Attempts = attempts
			return nil, last
		}
		e.tripBreaker(provider.Name(), failure)
	}
	if last == nil {
		return nil, &searchFailure{Reason: "搜索时间预算不足，未能发起任何请求。", Attempts: attempts}
	}
	last.Attempts = attempts
	return nil, last
}

// attemptOrder 产出本次调用的尝试顺序。
//
// priority：按 provider_order 顺序；round_robin：在健康集合上从游标位置开始、
// 环形展开。两者都只包含「能表达该 topic」且「不在冷却中」的后端。
func (e *Engine) attemptOrder(topic string) ([]Provider, *searchFailure) {
	capable := make([]Provider, 0, len(e.providers))
	for _, provider := range e.providers {
		if providerSupportsTopic(provider, topic) {
			capable = append(capable, provider)
		}
	}
	if len(capable) == 0 {
		if topic == topicNews {
			return nil, &searchFailure{Reason: messageNoNewsTool, Kind: failureNoNewsBackend}
		}
		return nil, &searchFailure{Reason: messageUnavailable, Kind: failureNoBackend}
	}
	healthy := make([]Provider, 0, len(capable))
	for _, provider := range capable {
		if e.healthy(provider.Name()) {
			healthy = append(healthy, provider)
		}
	}
	if len(healthy) == 0 {
		return nil, &searchFailure{Reason: "所有搜索后端都在冷却中，请稍后再试。", Kind: failureAllCooling}
	}
	if e.config.Strategy != strategyRoundRobin {
		return healthy, nil
	}
	start := e.advanceCursor(len(healthy))
	ordered := make([]Provider, 0, len(healthy))
	for offset := 0; offset < len(healthy); offset++ {
		ordered = append(ordered, healthy[(start+offset)%len(healthy)])
	}
	return ordered, nil
}

// nextAttemptBudget 把「剩余总预算」换算成「这一次尝试能用多久」：
// 先扣掉返回余量，再受单次上限约束。
// nextAttemptBudget 给单次尝试分配预算：剩余时间扣掉返回余量，再压到总预算的一半。
//
// 一半而不是固定值：总预算小时（默认 4 秒）它等价于旧的 2.5 秒上限，保证至少能试
// 两个后端；运营者为慢后端把总预算调到 60 秒时，单次尝试跟着放大到 30 秒，而不是
// 被一个写死的 2.5 秒截断——那会让「调高超时」变成一句空话。
func nextAttemptBudget(remaining, total time.Duration) time.Duration {
	budget := remaining - attemptReserve
	if budget <= 0 {
		return 0
	}
	limit := total / 2
	if limit < minAttemptBudget {
		limit = minAttemptBudget
	}
	if budget > limit {
		return limit
	}
	return budget
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

func (e *Engine) healthy(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	state, ok := e.breakers[name]
	if !ok {
		return true
	}
	return !e.now().Before(state.until)
}

func (e *Engine) tripBreaker(name string, failure *searchFailure) {
	cooldown := breakerCooldown
	if failure != nil && failure.Auth {
		cooldown = breakerAuthCooldown
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.breakers == nil {
		e.breakers = map[string]breakerState{}
	}
	e.breakers[name] = breakerState{until: e.now().Add(cooldown)}
}

func (e *Engine) clearBreaker(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.breakers, name)
}

// advanceCursor 在健康集合上前进一位，返回本次的起点。
func (e *Engine) advanceCursor(size int) int {
	if size <= 0 {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	start := e.cursor % size
	e.cursor = (start + 1) % size
	return start
}

// describe 把失败展开成「哪一步、哪个后端、什么原因」，只给设置页探测用。
func (f *searchFailure) describe() string {
	if f == nil {
		return "未知原因"
	}
	if len(f.Attempts) > 0 {
		return strings.Join(f.Attempts, "；")
	}
	if f.Provider != "" {
		return f.Provider + ": " + f.Reason
	}
	return f.Reason
}

// describeConfiguration 是探测失败时的配置快照：运营者按下按钮后最需要的
// 就是「到底有哪些后端真的可用」。
func (e *Engine) describeConfiguration() string {
	available := make([]string, 0, len(e.providers))
	for _, provider := range e.providers {
		available = append(available, provider.Name())
	}
	configured := "（空）"
	if len(available) > 0 {
		configured = strings.Join(available, ",")
	}
	return fmt.Sprintf("当前配置：可用后端=%s；策略=%s；预算=%dms。",
		configured, e.config.Strategy, e.config.Timeout.Milliseconds())
}
