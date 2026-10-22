package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	pluginv2 "github.com/zhuchunshu/sforum/apps/api/sdk/plugin/v2"
)

// 本插件贡献两个 AI 工具声明，与 sforum.extension.json 的 aiTools[] 一一对应：
//
//   - web-search 给模型用；
//   - sforum-web-search-probe 只给设置页的「测试搜索」按钮用（Core 的
//     ai_tool_probe settings action）。它永远不进任何用途白名单，因此模型看不到它。
//
// 声明是静态事实：宿主在插件进程启动前就知道这些工具存在，派发只认 id。
const (
	toolIDWebSearch = "sforum.web-search.tool.web_search"
	toolIDProbe     = "sforum.web-search.tool.probe_search"

	toolNameWebSearch = "web-search"
	toolNameProbe     = "sforum-web-search-probe"

	// declaredTimeoutMS 必须与 manifest 的 timeoutMs 逐字一致：宿主会拿清单里
	// 的数字作为每次调用的截止时间，并在派发前校验它没有漂移。
	//
	// 声明值随宿主上限一起收紧（2026-10-19：5000 → 180000 → 30000）。
	// 30 秒是宿主允许的上限（AIToolMaximumTimeoutMS），不是一个必然的等待：
	// 实际等多久由设置里的 timeout_ms 决定（默认 4 秒）。声明到上限是为了让
	// 运营者能把慢后端的预算调高，而不必改插件源码。
	declaredTimeoutMS = 30_000
)

const webSearchToolDescription = "检索站外互联网，返回标题、链接、摘要与发布时间。" +
	"需要站外事实、最新动态、官方文档或社区之外的资料时使用。" +
	"topic=news 只返回新闻类结果，freshness 限定发布时间，site 限定单一域名。" +
	"它只返回摘要与链接，不返回网页正文；搜索不可用或没有结果时会给出可解释的说明。"

const probeToolDescription = "仅供管理员在插件设置页执行「测试搜索」使用，模型不应调用它。" +
	"它用设置页当前草稿值做一次真实搜索并回报命中的后端、条数与耗时。"

// webSearchToolParameters 是模型看到的参数 schema。宿主只校验它是合法 JSON 对象、
// 从不重新解释它，因此这份声明就是模型与插件之间唯一的参数契约。
var webSearchToolParameters = mustCompactJSON(map[string]any{
	"type": "object",
	"properties": map[string]any{
		"query": map[string]any{
			"type":        "string",
			"description": "检索词，越具体越准",
		},
		"topic": map[string]any{
			"type":        "string",
			"enum":        []string{"general", "news"},
			"description": "general=通用资料（默认）；news=只找有时效的新闻类结果",
		},
		"freshness": map[string]any{
			"type":        "string",
			"enum":        []string{"day", "week", "month", "year"},
			"description": "只返回该时间范围内发布的结果",
		},
		"site": map[string]any{
			"type":        "string",
			"description": "限定单一域名，例如 docs.example.com",
		},
		"count": map[string]any{
			"type":        "integer",
			"minimum":     1,
			"maximum":     10,
			"description": "返回条数，默认 5",
		},
	},
	"required": []string{"query"},
})

// probeToolParameters 与设置页草稿字段同名。草稿值全部以字符串形态存在
// （extension_settings 存的就是字符串），所以这里一律声明为 string。
var probeToolParameters = mustCompactJSON(map[string]any{
	"type": "object",
	"properties": map[string]any{
		"probe_query":            map[string]any{"type": "string", "description": "设置页草稿中的测试检索词。"},
		"provider_order":         map[string]any{"type": "string", "description": "设置页草稿中的后端顺序，英文逗号分隔。"},
		"strategy":               map[string]any{"type": "string", "description": "设置页草稿中的调度策略（priority / round_robin）。"},
		"anysearch_api_key":      map[string]any{"type": "string", "description": "设置页草稿中的 AnySearch API Key；留空表示匿名调用。"},
		"anysearch_zone":         map[string]any{"type": "string", "description": "设置页草稿中的 AnySearch 区域（auto / cn / intl）。"},
		"deepseek_api_key":       map[string]any{"type": "string", "description": "设置页草稿中的 DeepSeek API Key。"},
		"tavily_api_key":         map[string]any{"type": "string", "description": "设置页草稿中的 Tavily API Key。"},
		"brave_api_key":          map[string]any{"type": "string", "description": "设置页草稿中的 Brave API Key。"},
		"language":               map[string]any{"type": "string", "description": "设置页草稿中的检索语言（auto 或语言码）。"},
		"safe_search":            map[string]any{"type": "string", "description": "设置页草稿中的安全搜索档位（off / moderate / strict）。"},
		"max_results":            map[string]any{"type": "string", "description": "设置页草稿中的默认返回条数。"},
		"timeout_ms":             map[string]any{"type": "string", "description": "设置页草稿中的单次检索总预算（毫秒）。"},
		"proxy_url":              map[string]any{"type": "string", "description": "设置页草稿中的出站代理地址。"},
		"allow_http":             map[string]any{"type": "string", "description": "设置页草稿中的「允许明文 http」值。"},
		"allow_private_endpoint": map[string]any{"type": "string", "description": "设置页草稿中的「允许内网端点」值。"},
	},
})

func main() {
	registry, err := pluginv2.NewAIToolRegistry(toolDefinitions(NewEngine(LoadConfig(os.Getenv)))...)
	if err != nil {
		log.Fatalf("configure sforum.web-search: %v", err)
	}
	server := pluginv2.NewServer().WithAIToolRegistry(registry)
	// 声明 lifecycle（manifest lifecycle.*）后宿主会走 V2 升级路径,
	// dev auto 才能自动 promote;步骤实现见 lifecycle.go。
	server.WithRuntimeStreams(pluginv2.RuntimeStreams{Lifecycle: runLifecycle})
	pluginv2.Serve(server)
}

// toolDefinitions 把两条声明集中在一处，让 main 与测试看到同一份事实
// （main_test.go 会拿它与 sforum.extension.json 对照）。
func toolDefinitions(engine *Engine) []pluginv2.AIToolDefinition {
	return []pluginv2.AIToolDefinition{
		{
			ID: toolIDWebSearch, ContractVersion: toolIDWebSearch + "@1",
			Name: toolNameWebSearch, Description: webSearchToolDescription,
			Parameters: webSearchToolParameters, TimeoutMS: declaredTimeoutMS,
			Execute: webSearchHandler(engine),
		},
		{
			ID: toolIDProbe, ContractVersion: toolIDProbe + "@1",
			Name: toolNameProbe, Description: probeToolDescription,
			Parameters: probeToolParameters, TimeoutMS: declaredTimeoutMS,
			Execute: probeHandler(engine),
		},
	}
}

// webSearchHandler 是模型-facing 的 web-search：参数是检索意图，配置来自本进程环境。
func webSearchHandler(engine *Engine) pluginv2.AIToolHandler {
	return func(ctx context.Context, call *pluginv2.AIToolCall) (*pluginv2.AIToolResult, error) {
		var arguments struct {
			Query     string `json:"query"`
			Topic     string `json:"topic"`
			Freshness string `json:"freshness"`
			Site      string `json:"site"`
			Count     int    `json:"count"`
		}
		if err := json.Unmarshal([]byte(call.Arguments), &arguments); err != nil {
			return explainable("参数必须是 JSON 对象，且包含字符串字段 query。"), nil
		}
		if strings.TrimSpace(arguments.Query) == "" {
			return explainable("缺少必填参数 query。请给出更具体的检索词。"), nil
		}
		// locale 是宿主按清单 aiTools[].context 声明传入的会话事实。本插件没有
		// 声明 context，因此走缺省集合 ["locale"]：宿主填 AIToolCall.Locale，
		// 未提供时为空。language=auto 时用它决定检索语言。
		//
		// 刻意不读 call.Context.GetLocale()：那是传输层的请求上下文，宿主对它
		// 填的是 "und"（未确定），不是站点语言。
		locale := call.Locale
		return engine.RunModel(ctx, locale, Request{
			Query:     arguments.Query,
			Topic:     arguments.Topic,
			Freshness: arguments.Freshness,
			Site:      arguments.Site,
			Count:     arguments.Count,
		})
	}
}

// probeHandler 是设置页探测入口。宿主用草稿值启动一个短命进程，所以本进程的
// 环境里就是运营者正在编辑的那份配置——工具参数只是同一份事实的冗余副本，
// 因此这里不读参数，只读本进程配置。
func probeHandler(engine *Engine) pluginv2.AIToolHandler {
	return func(ctx context.Context, _ *pluginv2.AIToolCall) (*pluginv2.AIToolResult, error) {
		return engine.RunProbe(ctx)
	}
}

// explainable 构造「工具可解释的失败」：模型会读到 Content 并可能换一种方式重试。
// 它与 handler 返回 Go error 是两回事——后者表示插件内部故障，宿主只给模型一句
// 通用说明，插件内部错误串永远不会借模型之口发布到社区。
func explainable(message string) *pluginv2.AIToolResult {
	return &pluginv2.AIToolResult{Content: message, IsError: true}
}

func mustCompactJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("encode AI tool parameters: %v", err))
	}
	return string(encoded)
}
