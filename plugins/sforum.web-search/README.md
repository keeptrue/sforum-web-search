# sforum.web-search

![AI 联网搜索 社交卡](./brand/social-preview.png)

SForum 扩展包：给 AI 回复一个 `web-search` 工具，让它能检索站外互联网
（AnySearch / DeepSeek / Tavily / Brave 四选一或多选，带主备、轮询与熔断）。

面向使用者的完整说明（安装、设置、编排语义、出站边界）见仓库根 `README.md`；
设计取舍与字段核对结论见仓库根 `NOTES.md`，宿主约束速查见 `AGENTS.md`。

## 包内结构

```text
sforum.extension.json      Manifest V3：两条 aiTools[] 声明 + 设置页 + backend
manifest/settings.json     设置文档（schemaVersion 1，Schema UI，5 组 17 字段）
manifest/langs/            zh-CN / en-US 的扩展名与描述
backend/                   Protocol V2 服务（独立 Go module）
```

## 后端文件

| 文件 | 职责 |
| --- | --- |
| `main.go` | 两条 AI 工具声明与 handler；注册表装配入口 |
| `lifecycle.go` | 六个生命周期步骤的幂等 no-op |
| `config.go` | `SFORUM_SETTING_*` → Config，含默认值与 clamp |
| `searcher.go` | `Provider` / `Request` / `Result` 契约、失败分类、参数规范化 |
| `provider_anysearch.go` | AnySearch 适配器（可匿名调用） |
| `provider_deepseek.go` | DeepSeek 适配器（site/freshness/lang 走检索算子） |
| `provider_tavily.go` | Tavily 适配器 + 跨后端日期归一化 |
| `provider_brave.go` | Brave 适配器（web / news 两个端点） |
| `engine.go` | 候选筛选、主备/轮询、熔断、时间预算、`RunModel` / `RunProbe` |
| `cache.go` | 进程内 LRU + TTL（key 不含后端名） |
| `format.go` | 去重、清洗、截断、模型可见文本（≤7000 字节） |
| `guard.go` | 出站守卫：scheme / userinfo / 字面量私网 / 不跟随重定向 / 1 MiB 上限 |
| `*_test.go` | 表驱动单测；`httptest` 假端点覆盖四家的请求形状与错误映射 |

## 两条 AI 工具

| id / handler | 模型可见名 | 用途 |
| --- | --- | --- |
| `sforum.web-search.tool.web_search` | `web-search` | 给模型用 |
| `sforum.web-search.tool.probe_search` | `sforum-web-search-probe` | 只给设置页「测试搜索」按钮用 |

第二条被登记但永不进入用途白名单，因此模型看不到它。`main_test.go` 的
`TestDeclarationsMatchManifest` 钉死清单与代码之间的 id / name / 描述 /
parameters / timeout 一致。

## 快速验证

```bash
cd backend && go vet ./... && go test ./...
cd ../../.. && ./scripts/prepare-package.sh
```
