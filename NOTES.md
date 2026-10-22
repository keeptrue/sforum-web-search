# NOTES · sforum.web-search

设计取舍、核对结论与踩过的坑。面向「下一个接手的人」，不是用户文档（那是
`README.md`）也不是接手速查（那是 `AGENTS.md`）。

## 1. 为什么是「AI 工具 + 四个后端」，而不是让 Core 支持联网

SForum 的 Core 已经有一条完整的插件工具链路：manifest `aiTools[]` 声明 → 装配期
登记进宿主工具表 → 补全循环按用途白名单派发 → 保留 slot `sforum.ai.tool`/`invoke`
回到插件进程执行。它自带走步预算、闸门、执行记录与结果截断。

把联网搜索做成 Core 能力会同时失去三件事：搜索供应商的选择不再属于运营者、
工具超时不再受清单约束、以及「用哪个引擎、花谁的额度」变成 Core 的硬编码决定。

## 2. 后端字段核对结论（2026-10-19）

### 2.1 Tavily

核对来源：<https://docs.tavily.com/documentation/api-reference/endpoint/search>

- 端点：`POST https://api.tavily.com/search`
- 鉴权：`Authorization: Bearer <key>`、`Content-Type: application/json`
- 请求字段（本插件用到的）：`query`、`max_results`（0–20）、`topic`
  （`general` / `news` / `finance`）、`time_range`（`day` / `week` / `month` / `year`）、
  `include_domains`（≤300）、`search_depth`（`basic` 1 额度 / `advanced` 2 额度）、
  `include_published_date`、`filter_by_published_date`、`safe_search`（布尔）、
  `language`（ISO 639-1 或英文语言名，默认只加权）
- 响应字段：`results[]{title, url, content, score, published_date, raw_content, favicon}`、
  `response_time`、`query`、`answer`、`usage{credits}`
- 错误体：`{"detail":{"error":"…"}}`，校验错误是 `{"detail":[{type,loc,msg,input}]}`；
  状态码 400 / 401 / 429 / 432 / 433 / 500

**与抽象契约的偏差（刻意）**：任务规格里的抽象映射提到 `days`，但当前官方文档
**没有** `days` 参数。时间窗口改由 `time_range` + `filter_by_published_date=true`
表达——只发 `time_range` 时 Tavily 默认只对可识别发布日的结果加权、不会剔除范围外的
结果，与「只返回该时间范围内发布的结果」不符，因此必须同时发过滤开关。

其他映射：`query→query`、`max_results→max_results`、`topic→topic`、
`include_domains→[site]`。`safe_search` 只有布尔两态，`moderate` 与 `strict` 都发
`true`（静默降级，设置页说明里写明了）。Tavily 不返回站点名，`Result.Site` 取 URL 主机名。

### 2.2 Brave

核对来源：

- <https://api-dashboard.search.brave.com/app/documentation/web-search/query>
- <https://api-dashboard.search.brave.com/api-reference/web/search/get>
- <https://api-dashboard.search.brave.com/app/documentation/news-search/query>
- <https://api-dashboard.search.brave.com/api-reference/news/news_search/get>
- <https://raw.githubusercontent.com/brave/brave-search-skills/main/skills/web-search/SKILL.md>（响应字段清单）

- 端点：`GET https://api.search.brave.com/res/v1/web/search`（通用）
  与 `GET https://api.search.brave.com/res/v1/news/search`（新闻）
- 鉴权头：`X-Subscription-Token: <key>`（**不是** `Authorization: Bearer`）
- 请求参数：`q`、`count`（web 1–20，news 1–50）、`offset`（0–9）、
  `freshness`（`pd` / `pw` / `pm` / `py` 或 `YYYY-MM-DDtoYYYY-MM-DD`）、
  `safesearch`（`off` / `moderate` / `strict`；web 默认 moderate、news 默认 strict）、
  `search_lang`、`ui_lang`、`country`、`extra_snippets`、`result_filter`、`operators`
- 站点限定不是独立参数，而是 `q` 里的 `site:` 算子（官方 Search Operators 文档）
- Web 响应：`web.results[]{title, url, description, age, language, page_age,
  meta_url{scheme,netloc,hostname,path}, thumbnail, profile, extra_snippets}`
- News 响应：顶层 `results[]{title, url, description, age, page_age, page_fetched,
  profile, meta_url{hostname,…}, breaking, thumbnail, extra_snippets, icons}`
- 错误体：`{"type":"ErrorResponse","error":{id,status,detail,meta,code}}`，
  `code` ∈ `INTERNAL` / `QUOTA_LIMITED` / `RATE_LIMITED`；状态码 404 / 422 / 429

映射：`q→q`（带 `site:` 前缀）、`count→count`、`freshness→pd/pw/pm/py`、
`safesearch` 三档直通、`search_lang` 取主语言子标签。
`Result.Snippet` 取 `description`，`Result.Date` 取 `page_age`（RFC 3339 → `YYYY-MM-DD`），
`Result.Site` 优先取 `meta_url.hostname`（缺失时回落到 URL 主机名）。
`age`（"2 days ago"）不使用：它是人类可读相对时间，无法稳定归一化。

### 2.3 DeepSeek

核对来源：<https://api-docs.deepseek.com/guides/search/>

- 端点：`POST https://api.deepseek.com/v1/tools/search`
- 鉴权：`Authorization: Bearer <key>`、`Content-Type: application/json`
- 请求字段：`query`（1–128 字符）、`count`（1–50，默认 10）、`need_content`（本插件恒 false）、
  `ori_question`、`localization`
- 检索算子（写在 `query` 里）：`site:`、`-site:`、`-`、`"…"`、`lang:`、`after:`、`before:`
- 响应字段：`results[]{rerank_score, title, url, snippet, summary, content, site_name,
  site_icon, date, authority}`；`date` 是 RFC 3339 UTC
- 错误体：OpenAI 兼容 `{"error":{"message","type","param","code"}}`；
  400 / 401 / 402（余额不足）/ 403 / 404 / 408 / 413 / 429 / 500 / 504
- 账号级 QPS 5（搜索与抓取各自独立）

映射：`site→site:<domain>`、`freshness→after:YYYY-MM-DD`（day=1 / week=7 / month=30 /
year=365，从「今天」往前推）、`language→lang:<主语言>`。
`Result.Summary` 填 `summary`（官方说明它是「给模型读」的长摘要），`Snippet` 填 `snippet`。

### 2.4 AnySearch

按任务规格给出的官方客户端源码结论实现（本次**没有**重新抓取官方文档/源码）：

- `POST {base}/v1/search`，base 默认 `https://api.anysearch.com`
- 头：`accept: application/json`、`content-type: application/json`、
  `x-anysearch-client: sforum-web-search/1.0.0`；有 key 才带 `authorization: Bearer <key>`
- body：`query`、`max_results`、`tag: "general.general"`；`zone`（非 auto 时）、
  `language`（仅 zh / en）
- 响应：`data.results[]{title, url, snippet?, content?}`、`data.metadata{total_results, search_time_ms}`
- 只声明 `Topics() = ["general"]`：它的 `tag` 没有「只找新闻」的语义

## 3. 设计取舍

### 3.1 适配器只做映射，编排只在引擎

四个适配器都只实现 `Provider`（`Name` / `Topics` / `Search`）。候选筛选、主备切换、
轮询、熔断、时间预算、缓存、格式化全部在 `engine.go` / `format.go`。
新增一家后端不需要碰引擎，也不需要碰另外三家。

### 3.2 缺 Key 的后端在装配期就被跳过

`buildProviders` 只装配「可用」的后端：AnySearch 永远可用（可匿名），
其余三家没有 key 直接不装配。这样它们不进候选、不占一次尝试预算、也不进熔断表——
「没有配置」和「配置了但挂了」是两种事实，不应该共用一套状态。

### 3.3 切换语义表（`statusFailure`）

| 情况 | 切换 | 冷却 |
| --- | --- | --- |
| 网络错误 / 超时 | 是 | 60s |
| HTTP 408 / 429 / 5xx | 是 | 60s |
| HTTP 3xx（不跟随重定向） | 是 | 60s |
| 响应体不是预期 JSON / 超过 1 MiB | 是 | 60s |
| HTTP 401 / 403 | 是 | **300s** |
| HTTP 402（余额不足） | 是 | 60s |
| HTTP 400 及其它 4xx | **否** | — |

规格只点名了「400 不切换」与「401/403 冷却 300s」，其余由这张表补齐：
判断标准是「换一个后端有没有意义」。402 是这家后端账户的问题（换一家就能答），
因此归入可切换；400/404/422 是请求本身的问题（换谁都一样），直接作为可解释失败。

### 3.4 时间预算与它的边界

```
总预算 = timeout_ms（1000–29000ms，硬编码 clamp；宿主上限 30s）
单次尝试 = min(剩余 - 200ms, 2500ms)
```

`200ms` 是留给「封装、切换与返回」的余量；`2500ms` 是单次上限。
**默认 4000ms 下**第一次吃满 2500ms 后仍有约 1300ms 给第二个后端，满足「一次调用
至少能试两个后端」。但这条保证**不成立于极小预算**：总预算 1000ms 时第一次上限是
800ms，若它真的吃满，剩余 200ms 扣掉余量后为 0，第二次不会发起。
这是规格公式的直接结果，不是实现偏差；`TestNextAttemptBudget` 与
`TestEngineTimeBudgetStillTriesASecondBackend` 分别钉住算术与默认预算下的行为。

### 3.5 缓存 key 不含后端名

缓存描述的是「这个问题问过没有」，不是「这个后端答过没有」。代价是换后端后短时间内
仍可能拿到旧后端的结果。这一点在三处写明了：README、设置页 callout、本文件。
缓存值里记住 `Provider`，所以结果头永远标出**实际答话**的引擎，不会撒谎。

### 3.6 7000 字节的 payload 预算

宿主把工具结果硬截到 8000 字节并追加截断标记。让插件先截到 7000，切点就在插件手里
（结果头 + 若干完整条目 + 明确的截断说明），而不是由宿主在结构中间切一刀。
超预算时按**整条**丢弃并追加「…（结果已截断）」；单条结果本身超预算时（超长 URL）
再做一次 UTF-8 安全的字节级兜底截断。

### 3.7 守卫比 fetch-url 窄：不做 DNS 解析层

fetch-url 的调用者是任意论坛用户（模型照着用户给的链接去抓），所以它必须做
「解析出的每个 IP 都必须是公网地址」。本插件的目标端点由**运营者**配置、默认是四个
写死的官方域名，因此只保留字面量层（私网/回环/link-local/保留地址、`localhost`、
`*.local`、`*.internal`）与「不跟随重定向」。在代理或内网 DNS 环境里做本机公网校验
只会制造误报，而不会增加任何真实的边界。

### 3.8 探测是第二条 `aiTools[]` 声明

Core 的 `ai_tool_probe` 设置动作需要一条「可派发目标」。复用 `aiTools[]` 比新增
`probes[]` 声明面小得多。代价是探测工具会被登记进宿主工具表——但登记不等于暴露，
它永远不进任何用途白名单。

探测**不受 `enabled` 影响**，也**不读写缓存**：运营者按下按钮要回答的是
「按当前草稿值能不能搜到」，而不是「现在生效了吗」或「上次结果是什么」。
`enabled` 也因此刻意不在动作的 `fields` 里。

## 4. 踩过的坑 / 核对时发现的事实

### 4.1 设置动作里的 `secret` 字段需要前端同时提交 `secrets`

宿主 `settingsActionValues` 对 `UseDraftValues` 的动作要求：`action.fields` 里
每一个 `secret` 字段都必须在 `input.Secrets` 里出现（`preserve` 或 `replace`）。
前端 `executeSettingsAction` 确实会为这些字段提交 `{mode:"preserve"}`，
`replace` 才带值。因此把 4 个 Key 放进 `fields` 是**必要**的——不放进去，探测
进程的环境里就只有已保存的 Key（`sanitizeSettingValues` 会补 `current`），
草稿里刚填的 Key 测不到。

### 4.2 `cache_ttl_s` 是动作字段，但不是探测参数

任务规格给的探测参数清单里没有 `cache_ttl_s`（探测不走缓存，这个草稿值没有意义），
而动作字段要求「除 `enabled` 外的全部 key」。两者因此有**一处刻意的不对称**，
由 `TestProbeToolCoversEveryDraftField` 显式钉住（它同时断言 `cache_ttl_s`
**不**出现在探测参数里）。多出来的草稿值只会进入探测进程的环境，handler 不读它。

### 4.3 `ui.groups[].columns` 只在 `layout: "tabs"` 时生效

`SFExtensionSettingsRenderer` 在 `layout: "form"` 下走 `legacySections()`：按
`items[].group`（解析后的分组标题）分段，`columns` 被忽略。本包沿用 fetch-url 的
form 布局（单页、5 个分组、17 个字段），因此 `columns` 目前只是声明。
若要让它真正生效，需要改成 `layout: "tabs"` 并声明 `ui.tabs`——那会引入一层
规格没有要求的标签页结构，所以本次没做。

### 4.4 `field.group` 用「与分组 label 完全相同的本地化对象」

`normalizeSettingsDocument` 是把 `field.Group` 和 `group.Label` 做
`localizedTextMatches` 比较来推导 `GroupID` 的（**不是**按 group id 匹配）。
写成 `"group": "backends"` 时两边对不上，`GroupID` 会是空串。本包每个字段都写成
`{"zh-CN": "搜索后端", "en-US": "Search backends"}`，于是：
form 布局的分段标题是本地化文案，`GroupID` 也能正确解析（用宿主 `LoadPackage`
实测：17 个字段全部归组）。

### 4.5 `aiTools[].context` 存在但本包不写

当前宿主 `ManifestAITool` 已有 `context` 字段，缺省值是 `["locale"]`。本插件正是
靠 locale 决定检索语言（`language=auto`），所以**省略**它就能拿到需要的事实，
写出来反而会把「只要 locale」这件事重复声明一次。清单里因此没有 `context`。

### 4.6 JSON 键序：drift 测试比的是规范化 JSON

Go 的 `json.Marshal(map[string]any)` 按键名排序输出，而清单里的 `parameters`
是人工排版的。`TestDeclarationsMatchManifest` 因此对 `parameters` 做
「反序列化后深度比较」，对 `name` / `description` / `timeoutMs` / `contractVersion`
做逐字比较——前者是语义契约，后者是逐字事实。

## 5. 已知边界

- **只返回摘要**：四家都只给标题、链接与摘要（DeepSeek 另有一段长摘要）。需要正文时
  由模型再调用 `fetch-url`（另一个插件）。
- **缓存是进程内的**：保存设置会重启插件制品，缓存随之清空；多实例部署时各自缓存。
- **没有每用户配额**：用量闸门由宿主 AI 网关负责，本插件只做后端级熔断与缓存。
- **不重试同一后端**：一次调用里每个后端最多尝试一次，失败就换下一个。
- **Tavily 的 `safe_search` 只有开/关**：`moderate` 与 `strict` 都发 `true`。
- **AnySearch 只发 zh/en 两种语言**：其它语言交给后端默认，避免整单被拒。
- **不拦截单标签主机名**：端点由运营者配置（默认是官方域名），不像 fetch-url
  那样面对任意用户输入，因此只拦私网/回环/内部后缀。
- **Brave 的 `q` 长度上限**（600 字符）未在本插件侧校验：超长检索词会由 Brave
  返回 4xx，引擎按「请求被拒绝」直接作为可解释失败。

## 6. 本次没有验证、只能先按默认/规格写的地方

1. **AnySearch 的真实响应与错误体**：按任务规格给的结论实现，没有重新抓取官方
   源码；错误文案用宽口径解析器（`error.message` / `detail.error` / 字符串 / 数组）
   兜底，取不到时只报状态码。
2. **没有真实 Key 的联网冒烟**：四家的适配器只用 `httptest` 假端点验证请求形状与
   响应解析，测试不访问真实网络、不需要真实 Key。真实额度、限流与区域差异没有实测。
3. **Tavily `language` 与 `topic=news` 的组合**：文档没有说两者互斥，本插件在
   有语言时一律发送；若某家因此返回 400，会得到「请求被拒绝」的可解释失败，
   而不是切换后端。
4. **`ui.groups.columns` 的实际排版效果**：见 4.3，form 布局下不生效。
5. **设置页「测试搜索」的端到端点击**：依赖宿主 `ai_tool_probe` 路径与前端提交
   `secrets` 的行为，本次只核对了源码与单元测试，没有在浏览器里点过。
