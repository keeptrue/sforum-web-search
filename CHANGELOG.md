# 变更记录

`sforum.web-search` 的对外变更。格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)。

发布说明由 `.github/workflows/release.yml` 从对应版本的小节生成，因此**打标签前必须先写这一节**，
否则 Release 只会有安装步骤。

## [1.1.0] - 2026-10-26

### 新增

- 品牌资产 `brand/`：`icon.svg`（512 自包含矢量）、`icon.png`（512×512）、
  `social-preview.png`（1280×640 中文主卡）。后台插件卡片与仓库社交卡从此有正式视觉身份。
- 仓库根与包内 `README.md` 展示社交预览图。
- `scripts/prepare-package.sh` 增加 `extension brand check` 门禁：品牌资产的尺寸、体积或
  模板参数不合规时，打包直接失败，而不是把问题留到启用阶段。

### 变更

- `AGENTS.md` 记录品牌资产规范：尺寸与体积上限、生成与校验命令、以及与包摘要的关系
  （改图会改变包摘要，属正常升级，不要为消掉漂移去改版本号）。

### 说明

- 后端行为、清单契约（`sforum.manifest@3` / `sforum.host@2`）、设置项与两条 AI 工具声明
  均未改动。从 `1.0.0` 升级不需要重配任何后端或 Key。
- 品牌文件不属于 `packageFiles`，但属于包树：升级会因包摘要变化走一次正常授权确认。

## [1.0.0] - 2026-10-22

首个公开版本。

- 两条 AI 工具声明：`web-search`（检索站外互联网）与 `probe_search`（设置页「测试搜索」探针）。
- 四个后端一个契约：AnySearch（可匿名）/ DeepSeek / Tavily / Brave；缺 Key 的后端在装配期
  跳过，不占尝试预算也不进熔断表。
- 调度：`priority`（主备 + 熔断，默认）与 `round_robin`（轮询分摊额度）；时间预算、进程内
  LRU + TTL 缓存、出站守卫（不跟随重定向、1 MiB 上限）。
- 后台「AI 能力 → 联网搜索」设置页：5 组 17 字段，含 `ai_tool_probe` 测试搜索动作。
- Protocol V2 后端（独立 Go module）+ Manifest V3 清单，四平台发布包。
