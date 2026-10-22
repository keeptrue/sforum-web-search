# SForum AI Web Search

SForum 的 AI 联网搜索插件：给模型一个 `web-search` 工具，让它能检索站外互联网。
外部扩展包，不属于 SForum 内置插件。

- 仓库：`~/Code/github/sforum-web-search`（独立仓库，仓库根即 SForum 的外部扩展源根）
- 框架检出：`~/Code/github/SForum`（**同级目录**，插件通过本地 `replace` 解析宿主 SDK）
- 扩展 id：`sforum.web-search`；版本在 `plugins/sforum.web-search/sforum.extension.json`
- 面向运营者的说明见仓库根 `README.md`，设计取舍与核对结论见 `NOTES.md`

## 目录布局

```text
sforum-web-search/
├── plugins/sforum.web-search/         ← 扩展包：manifest、settings、langs、backend
├── scripts/prepare-package.sh         ← build → digest --write → validate → test
├── .github/workflows/                 ← ci.yml / release.yml
├── README.md  NOTES.md  AGENTS.md
└── .gitignore                         ← 忽略 backend/plugin
```

宿主扫描规则是 `<EXTERNAL_EXTENSION_ROOTS>/plugins/<包目录>`，**同一扩展 id 只能出现在
一个源**。宿主 `DigestTree` 禁止符号链接，不要用软链挂载，也不要保留第二份副本。

## 后端文件

| 文件 | 职责 |
| --- | --- |
| `main.go` | 两条 AI 工具声明与 handler；注册表装配入口 |
| `lifecycle.go` | 六个生命周期步骤的幂等 no-op（PLANNED → RUNNING → SUCCEEDED） |
| `config.go` | 从 `SFORUM_SETTING_*` 环境变量装载配置，含默认值与 clamp |
| `searcher.go` | `Provider` / `Request` / `Result` 契约、失败分类、参数规范化 |
| `provider_anysearch.go` | AnySearch 适配器（可匿名） |
| `provider_deepseek.go` | DeepSeek 适配器（检索算子表达 site/freshness/lang） |
| `provider_tavily.go` | Tavily 适配器 + 跨后端日期归一化 |
| `provider_brave.go` | Brave 适配器（web / news 两个端点） |
| `engine.go` | 候选筛选、主备/轮询、熔断、时间预算、RunModel / RunProbe |
| `cache.go` | 进程内 LRU + TTL |
| `format.go` | 去重、清洗、截断、模型可见文本 |
| `guard.go` | 出站守卫（scheme / userinfo / 字面量私网 / 不跟随重定向 / 1 MiB 上限） |
| `*_test.go` | 表驱动单测；`httptest` 假端点覆盖四家的请求形状与错误映射 |

## 开发闭环

```bash
# 1. 改 plugins/sforum.web-search/ 下的后端代码
cd plugins/sforum.web-search/backend && go vet ./... && go test ./...

# 2. 构建二进制 + 刷新摘要 + 宿主校验（SFORUM_DIR=… 可覆盖框架路径）
cd ../../.. && ./scripts/prepare-package.sh

# 3. 让站点加载：重启 API 扫描到候选 → 后台「插件管理 → 升级 → 启用」
```

**改任何包内文件都必须重跑第 2 步**：宿主以 exact-artifact 校验哈希，摘要不匹配的包
会被拒绝启用。同版本号只改内容时，卡片上的「重启」不会切换制品，必须走
「升级（授权当前精确制品 → 确认并升级）」。

## CI 与发布

- `ci.yml`：push `main` / PR / 手动。检出插件仓库 + 宿主框架（`SFORUM_REF`，默认 `main`）
  → `go mod edit -replace` 指向检出目录 → `go vet` / `go test` → 构建宿主平台二进制
  → 其余发布平台交叉编译冒烟 → `extension digest --write` / `validate` / `test`。
  构建出的摘要与提交值不一致只发 notice，不判失败（工具链差异常见）。
- `release.yml`：`v*` 标签（或手动）触发；`verify` → 四平台 `build` → `publish`
  （`SHA256SUMS` + Release）。标签版本必须等于清单 `version`，否则 verify 直接失败。
- **`backend/go.mod` 的 `go` 指令不得低于宿主 `apps/api/go.mod`**：插件是 replace 进宿主
  模块构建的，低了只会得到一句 `updates to go.mod needed`；CI 的前置检查会直接指出该改哪行。
- 构建参数三处必须一致（宿主 dev watcher、`scripts/prepare-package.sh`、CI）：
  `CGO_ENABLED=0 go build -trimpath -buildvcs=false`。任何一处不同，同一份源码的摘要就不同，
  包会被宿主判为 exact-artifact 不匹配。

## 验证标准（不要只跑单测就报完成）

1. `cd plugins/sforum.web-search/backend && go test ./...`
2. `cd ~/Code/github/SForum/apps/api && go run ./cmd/sforum extension validate|test <包路径>`
3. 设置页「测试搜索」按钮要能出成功与失败两种就地解释（依赖宿主已实现
   `ai_tool_probe` settings action）。
4. 真实检索冒烟：在站内发一条 @ 机器人的主题问一个只有联网才能答的事实，
   确认回复引用了结果里的事实与链接，而不是模型记忆。

## 宿主约束速查（已核对源码）

1. **AI 工具超时上限 30000ms**（宿主 `AIToolMaximumTimeoutMS`），且运行期截止时间
   必须逐字等于清单 `timeoutMs`；插件自己的检索预算要更小（本插件上限 29000ms），
   否则宿主会在插件返回结果前掐断。默认只给 4 秒——上限是天花板，不是等待时长。
2. **工具结果被宿主截断到 8000 字节/条**（单次回复累计 16000）。所以本插件自己把
   payload 压到 7000 字节以内，让切点由插件决定。
3. **登记 ≠ 暴露**：模型只看到用途白名单里的工具名。装上插件后必须在
   AI 控制台 → 用途 → `forum.reply` → 模型可见工具 → 自定义 里点名 `web-search`。
4. **`aiTools[].contractVersion` 必须严格是 `<id>@1`**；`id` 与 `handler` 必须落在
   `<扩展 id>.` 命名空间下（`ExtensionManifest.IsOwnedID`）。
5. **`aiTools[].context` 可以省略**：缺省等价于只要 `locale`。本插件依赖这个缺省值
   决定检索语言，因此清单里**不写** `context` 字段。
6. **`ai_tool_probe` 动作要求**：`tool` 必须逐字等于本插件某条 `aiTools[].id`，
   `fields` 至少一个且每个 key 都必须存在于 `fields[]`。`fields` 里出现 `secret`
   字段时，前端会为它提交 `{mode:preserve|replace}`，否则宿主拒绝整次探测。
7. **插件设置以 `SFORUM_SETTING_<KEY>` 环境变量注入子进程**（大写、非字母数字转下划线），
   而且从未保存过的设置不会被注入——插件必须自带与清单 `default` 一致的默认值
   （本插件用 `config_test.go` 的 `TestManifestDefaultsMatchRuntimeDefaults` 钉死）。
8. **插件子进程的环境是白名单**：`HTTP_PROXY`/`HTTPS_PROXY` **不会**透传，
   出站代理只能来自插件自己的设置项。
9. **`admin.pages[].label` / `description` 是纯字符串**，不是本地化对象；
   扩展 name/description 的本地化走 `includes.langs`。
10. **`ui.groups[].columns` 只在 `layout: "tabs"` 时生效**：当前渲染器在
    `layout: "form"` 下按 `items[].group`（解析后的分组标题）分段，`columns` 被忽略。
    本包沿用 fetch-url 的 form 布局，因此 `columns` 只是声明、不影响排版。
11. **`ui.groups[].columns` 只能是 0/1/2**，其它值会被清单校验拒绝。
12. **Go 的 RE2 不支持 `\uXXXX` 转义**，必须写 `\x{...}`。写错的后果是
    `regexp.MustCompile` 在包初始化时 panic，插件连 go-plugin 握手都进不去。
13. **AI 工具登记表曾只在启动期构建一次**（宿主 `apps/api/bootstrap/ai_plugin_tools.go`
    的 `registerPluginAITools()`：装配时只登记 `status = enabled` 插件的 `aiTools[]`，
    装配之后启停/升级都不改表）——该行为已由宿主热重建修掉：启用/停用/升级并等期望集
    应用完成后登记表自动跟随，不必重启。未登记的名字仍被 `ToolRegistry.Descriptors`
    静默跳过，控制台靠「没有 `· sforum.web-search` 来源标注、并出现在待确认芯片里」
    暴露它；刷新页面后仍未登记即宿主版本不含该热重建，重启 API 进程是兜底路径。

## 设计取舍（NOTES.md 有完整记录）

- **四个后端一个契约**：适配器只做参数映射，编排（候选、主备、熔断、预算）只在引擎里，
  所以新增一家后端不需要碰引擎。
- **缺 Key 的后端在装配期跳过**：不进候选、不占预算、不进熔断表。
- **400 不切换、401/403/402 切换**：前者是请求构造问题，后两者是这家后端的问题。
- **缓存 key 不含后端名**：缓存的是「这个问题问过没有」，结果头会标出实际答话的引擎。
- **不跟随重定向**：3xx 直接算失败，避免请求被跳到一个没有配置过的地址。
