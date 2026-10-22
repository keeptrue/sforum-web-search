package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	pluginv2 "github.com/zhuchunshu/sforum/apps/api/sdk/plugin/v2"
	protocolwire "github.com/zhuchunshu/sforum/apps/api/sdk/plugin/v2/gen/sforum/protocol/v2"
)

func callTool(t *testing.T, handler pluginv2.AIToolHandler, arguments string) *pluginv2.AIToolResult {
	t.Helper()
	result, err := handler(context.Background(), &pluginv2.AIToolCall{Arguments: arguments})
	if err != nil {
		t.Fatalf("tool handler returned an internal error: %v", err)
	}
	if result == nil {
		t.Fatalf("tool handler returned no result")
	}
	return result
}

func TestWebSearchHandlerRejectsMalformedArguments(t *testing.T) {
	provider := &fakeProvider{name: providerAnySearch, results: sampleResults()}
	engine := newEngineWith(DefaultConfig(), []Provider{provider})
	handler := webSearchHandler(engine)

	for _, arguments := range []string{``, `not json`, `{"query": 42}`, `{"query": ""}`, `{"query": "   "}`} {
		result := callTool(t, handler, arguments)
		if !result.IsError {
			t.Errorf("arguments %q were accepted: %+v", arguments, result)
		}
	}
	if provider.calls != 0 {
		t.Errorf("provider calls = %d, want 0 for malformed arguments", provider.calls)
	}
}

func TestWebSearchHandlerForwardsArgumentsToTheEngine(t *testing.T) {
	// 这个用例带 topic=news，因此需要一个声明支持新闻的后端。
	provider := &fakeProvider{name: providerDeepSeek, topics: []string{topicGeneral, topicNews}, results: sampleResults()}
	engine := newEngineWith(DefaultConfig(), []Provider{provider})

	result := callTool(t, webSearchHandler(engine),
		`{"query":"sforum 插件","topic":"news","freshness":"week","site":"docs.example.com","count":3}`)
	if result.IsError {
		t.Fatalf("unexpected failure: %s", result.Content)
	}
	request := provider.lastRequest
	if request.Query != "sforum 插件" {
		t.Errorf("query = %q", request.Query)
	}
	if request.Topic != topicNews {
		t.Errorf("topic = %q, want news", request.Topic)
	}
	if request.Freshness != "week" {
		t.Errorf("freshness = %q", request.Freshness)
	}
	if request.Site != "docs.example.com" {
		t.Errorf("site = %q", request.Site)
	}
	if request.Count != 3 {
		t.Errorf("count = %d", request.Count)
	}
}

func TestProbeHandlerUsesProcessConfigurationNotArguments(t *testing.T) {
	provider := &fakeProvider{name: providerAnySearch, results: sampleResults()}
	config := DefaultConfig()
	config.ProbeQuery = "进程里的检索词"
	engine := newEngineWith(config, []Provider{provider})

	// 参数里放一个不该被采用的检索词：探测目标只能来自本进程（即草稿）配置。
	result := callTool(t, probeHandler(engine), `{"probe_query":"参数里的检索词"}`)
	if result.IsError {
		t.Fatalf("probe failed: %s", result.Content)
	}
	if provider.lastRequest.Query != "进程里的检索词" {
		t.Fatalf("probe query = %q, want the process configuration", provider.lastRequest.Query)
	}
}

// TestDeclarationsMatchManifest 是清单与代码之间的漂移钉死。
// 宿主按清单里的 id/name/parameters/timeoutMs 派发与计时，插件按同样的 id 解析
// handler；两边分叉的表现是「模型调用了工具但永远失败」，非常难排查，所以用测试钉住。
func TestDeclarationsMatchManifest(t *testing.T) {
	path := filepath.Join("..", "sforum.extension.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var manifest struct {
		ID      string `json:"id"`
		Backend struct {
			Entry           string `json:"entry"`
			RPC             string `json:"rpc"`
			ProtocolVersion int    `json:"protocolVersion"`
		} `json:"backend"`
		AITools []struct {
			ID              string          `json:"id"`
			ContractVersion string          `json:"contractVersion"`
			Name            string          `json:"name"`
			Description     string          `json:"description"`
			Parameters      json.RawMessage `json:"parameters"`
			Handler         string          `json:"handler"`
			TimeoutMS       int             `json:"timeoutMs"`
		} `json:"aiTools"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if manifest.Backend.Entry != "backend/plugin" {
		t.Errorf("backend entry = %q", manifest.Backend.Entry)
	}
	if manifest.Backend.RPC != "hashicorp-go-plugin" || manifest.Backend.ProtocolVersion != 2 {
		t.Fatalf("manifest backend must be Protocol V2 over hashicorp-go-plugin: %+v", manifest.Backend)
	}

	definitions := toolDefinitions(NewEngine(DefaultConfig()))
	if len(definitions) != len(manifest.AITools) {
		t.Fatalf("code declares %d tools, manifest declares %d", len(definitions), len(manifest.AITools))
	}
	byID := map[string]pluginv2.AIToolDefinition{}
	for _, definition := range definitions {
		byID[definition.ID] = definition
	}
	for _, declared := range manifest.AITools {
		definition, ok := byID[declared.ID]
		if !ok {
			t.Errorf("manifest declares %q but the code has no such tool", declared.ID)
			continue
		}
		if definition.Name != declared.Name {
			t.Errorf("%s: code name %q, manifest name %q", declared.ID, definition.Name, declared.Name)
		}
		if definition.TimeoutMS != declared.TimeoutMS {
			t.Errorf("%s: code timeout %dms, manifest timeout %dms", declared.ID, definition.TimeoutMS, declared.TimeoutMS)
		}
		if definition.ContractVersion != declared.ContractVersion {
			t.Errorf("%s: code contract %q, manifest contract %q", declared.ID, definition.ContractVersion, declared.ContractVersion)
		}
		if definition.Description != declared.Description {
			t.Errorf("%s: code description drifted from the manifest:\ncode:     %s\nmanifest: %s",
				declared.ID, definition.Description, declared.Description)
		}
		// parameters 按规范化 JSON 比较：键序与缩进不影响语义，值必须逐字一致。
		if !reflect.DeepEqual(canonicalJSON(t, definition.Parameters), canonicalJSON(t, string(declared.Parameters))) {
			t.Errorf("%s: code parameters drifted from the manifest:\ncode:     %s\nmanifest: %s",
				declared.ID, definition.Parameters, string(declared.Parameters))
		}
		// handler 必须与 id 同名：宿主按 id 派发，插件按 id 解析，两边用的是同一个键。
		if declared.Handler != declared.ID {
			t.Errorf("%s: manifest handler %q must equal the declaration id", declared.ID, declared.Handler)
		}
		if !strings.HasPrefix(declared.ID, manifest.ID+".") {
			t.Errorf("%s: declaration id must be namespaced under %q", declared.ID, manifest.ID)
		}
		if !strings.HasPrefix(declared.ContractVersion, declared.ID+"@") {
			t.Errorf("%s: contract version %q must be <id>@1", declared.ID, declared.ContractVersion)
		}
		// 上限来自 SDK 常量而不是字面量：宿主放宽上限时插件测试不该跟着改。
		if declared.TimeoutMS <= 0 || declared.TimeoutMS > pluginv2.HostAIToolMaximumTimeoutMS {
			t.Errorf("%s: timeout %dms is outside the host range 1..%d",
				declared.ID, declared.TimeoutMS, pluginv2.HostAIToolMaximumTimeoutMS)
		}
		if len(declared.Parameters) == 0 {
			t.Errorf("%s: manifest parameters are required", declared.ID)
		}
	}
}

func canonicalJSON(t *testing.T, raw string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("decode JSON %q: %v", raw, err)
	}
	return value
}

func TestToolDeclarationsStayUnderHostLimits(t *testing.T) {
	limits := struct {
		schema      int
		description int
	}{schema: 8192, description: 1024}
	if len(webSearchToolParameters) > limits.schema || len(probeToolParameters) > limits.schema {
		t.Fatalf("tool schema exceeds the host limit of %d bytes", limits.schema)
	}
	if len(webSearchToolDescription) > limits.description || len(probeToolDescription) > limits.description {
		t.Fatalf("tool description exceeds the host limit of %d bytes", limits.description)
	}
	if declaredTimeoutMS <= 0 || declaredTimeoutMS > pluginv2.HostAIToolMaximumTimeoutMS {
		t.Fatalf("declared timeout %dms is outside the host's 1..%d range",
			declaredTimeoutMS, pluginv2.HostAIToolMaximumTimeoutMS)
	}
}

// TestProbeToolCoversEveryDraftField 钉死「设置页动作的草稿字段」与「探测工具的
// 参数名」是同一份事实：宿主会把草稿值既塞进进程环境、也编码成工具参数，
// 两边不同名时探测会静默地读到默认值。
func TestProbeToolCoversEveryDraftField(t *testing.T) {
	path := filepath.Join("..", "manifest", "settings.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var document struct {
		Actions []struct {
			Kind   string   `json:"kind"`
			Fields []string `json:"fields"`
			Tool   string   `json:"tool"`
		} `json:"actions"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	if len(document.Actions) != 1 {
		t.Fatalf("settings.json must declare exactly one action, got %d", len(document.Actions))
	}
	action := document.Actions[0]
	if action.Kind != "ai_tool_probe" || action.Tool != toolIDProbe {
		t.Fatalf("action = %+v, want ai_tool_probe → %s", action, toolIDProbe)
	}
	var parameters struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal([]byte(probeToolParameters), &parameters); err != nil {
		t.Fatalf("decode probe parameters: %v", err)
	}
	// 唯一的不对称：探测从不读写缓存，因此 cache_ttl_s 只作为草稿值存在于动作里，
	// 不进入工具参数。其余字段必须一一对应，否则探测会静默读到默认值。
	const cacheTTLField = "cache_ttl_s"
	for _, field := range action.Fields {
		if field == "enabled" {
			t.Errorf("enabled must not be a draft field: the probe answers \"can this configuration search?\"")
			continue
		}
		_, declared := parameters.Properties[field]
		if field == cacheTTLField {
			if declared {
				t.Errorf("%q must not be a probe parameter: the probe bypasses the cache", field)
			}
			continue
		}
		if !declared {
			t.Errorf("probe tool parameters are missing draft field %q", field)
		}
	}
	if len(parameters.Properties)+1 != len(action.Fields) {
		t.Errorf("probe tool declares %d parameters, action declares %d fields (expected exactly one extra: %s)",
			len(parameters.Properties), len(action.Fields), cacheTTLField)
	}
}

// TestToolRegistryAcceptsDeclarations 走一遍宿主 SDK 的注册校验：清单能过校验
// 不代表运行期能注册，两边的边界值必须一致（SDK 会重新检查 name/description/
// parameters/timeout）。
func TestToolRegistryAcceptsDeclarations(t *testing.T) {
	registry, err := pluginv2.NewAIToolRegistry(toolDefinitions(NewEngine(DefaultConfig()))...)
	if err != nil {
		t.Fatalf("registry rejected the declarations: %v", err)
	}
	definitions := registry.Definitions()
	if len(definitions) != 2 {
		t.Fatalf("registry exposes %d definitions, want 2", len(definitions))
	}
	ids := map[string]bool{}
	for _, definition := range definitions {
		ids[definition.ID] = true
		if definition.ContractVersion != definition.ID+"@1" {
			t.Errorf("%s: contract version = %q", definition.ID, definition.ContractVersion)
		}
	}
	if !ids[toolIDWebSearch] || !ids[toolIDProbe] {
		t.Fatalf("registry ids = %v", ids)
	}
}

// locale 来自宿主按清单 aiTools[].context 填入的 AIToolCall.Locale；缺省集合是
// ["locale"]。传输层的 RequestContext.Locale 恒为 "und"（未确定），读它会让检索
// 语言变成一个无意义的值，因此这条用例同时钉住「不读 Context」。
func TestWebSearchHandlerUsesDeclaredLocaleForLanguage(t *testing.T) {
	provider := &fakeProvider{name: providerAnySearch, topics: []string{topicGeneral}, results: sampleResults()}
	config := DefaultConfig()
	config.Language = languageAuto
	engine := newEngineWith(config, []Provider{provider})

	result, err := webSearchHandler(engine)(context.Background(), &pluginv2.AIToolCall{
		Arguments: `{"query":"sforum"}`,
		Locale:    "zh-CN",
		Context:   &protocolwire.RequestContext{Locale: "und"},
	})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("handler failed: result=%#v err=%v", result, err)
	}
	if got := provider.lastRequest.Language; got != "zh" {
		t.Fatalf("language = %q，应当来自 AIToolCall.Locale 而不是 Context", got)
	}
}
