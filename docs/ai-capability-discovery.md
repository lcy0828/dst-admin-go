# API、MCP 与机器人能力发现

面板的 OpenAPI 契约同时用于接口校验、MCP 和插件能力发现。新增符合现有协议的接口后，支持 `companion-gateway-v1` 的插件可以搜索并调用，无需为每个新接口增加 Python 分支或关键词。常见快捷指令仍直接调用面板；其他自然语言请求自动交给模型，不要求 `AI:` 前缀。

## 接入与兼容

新版插件在私有 `panel_url` / `panel_token` 连接上协商协议。该连接需要完整的面板管理凭据；凭据留在插件内部，不能交给模型。插件根据真实平台用户身份、群配置和原生工具权限缩小每次请求的权限。群管理员获得已开放的游戏管理操作；全局面板管理仅向已验证的面板维护者开放。

| 组合 | 行为 |
| --- | --- |
| 新面板 + 新插件 | 使用按需发现和通用调用 |
| 新面板 + 旧插件 | 原 API、MCP 工具名、能力目录及输入输出继续可用；旧插件不会凭空获得新适配逻辑 |
| 旧面板 + 新插件 | 未协商到新特性时，保留原固定能力路径 |
| 插件先升级一次，此后面板增加兼容接口 | 新接口自动收录，可通过发现和通用调用使用；新业务的专用快捷句式和卡片仍是可选增强 |
| 新协议类型或不支持的消息资源 | 明确说明当前客户端限制，不冒充已执行或已发送 |

普通 HTTP 接口、原 `api_*` 工具、`find_api/call_api`、MCP 视图及 `pagination=off` 保持原契约。新增字段均为契约扩展或新入口；不删除旧数据库表。通用执行记录使用独立新增表，回退面板时旧版可继续使用原记录。

新安装默认使用简单权限策略；升级时缺少 `access_policy` 的旧配置保留自定义限制。不会因补上默认配置而扩大旧群的权限、房间范围或工作台权限。

## 模型看到的工具

新协议下最多保留以下四个 DST 工具；普通查询场景和没有修改权限的用户不获得 `dst_run`。其他插件的工具不受影响。

| 工具 | 用途 |
| --- | --- |
| `dst_find(query)` | 在当前授权范围搜索用途或接口 ID，返回最多三个候选和参数结构 |
| `dst_read(capability_id, parameters, room?, world?, player?)` | 读取；服务端拒绝把修改接口作为读取执行 |
| `dst_run(capability_id, parameters, room?, world?, player?)` | 准备或执行用户明确要求的修改；确认要求由服务端决定 |
| `dst_result(task_ref)` | 读取原请求、原任务或受控资源，不重新执行修改 |

例如，先调用：

```json
{"query":"最近一天服务器 CPU 内存性能报表"}
```

对返回的 `api:getRoomPerformanceReport`，按照返回 Schema 读取：

```json
{
  "capability_id":"api:getRoomPerformanceReport",
  "room":"示例房间",
  "parameters":{"query":{"from":"2026-01-01T00:00:00Z","to":"2026-01-02T00:00:00Z"}}
}
```

公共解析器可将唯一房间名称、地面/洞穴、玩家昵称转换为对应 ID。重名、引用冲突或目标缺失会要求补充，不按在线人数猜玩家。全局版本和机器接口不强制选择房间。参数保留原来的 `path/query/body/headers` 结构；`PATCH` 的省略、`null`、`false`、`0` 各有不同含义，插件不会自动填入 Schema 默认值。

## 执行与结果

修改请求发送前先在插件保存原请求编号，服务端再持久化执行记录。同一消息的同一操作不会因为重试、断线或插件重新加载而重复下发。不同参数复用同一请求会被拒绝。

- `generic:<引用>`：读取已返回的原执行记录。
- `generic-request:<引用>`：响应丢失时，按发送前的请求编号找回记录；找不到也不会自动重发。
- `resource:<引用>`：读取当前用户拥有的认证资源，不接受任意 URL。

异步任务的 `queued/running/accepted`、控制台的 `sent` 均不代表完成。`succeeded` 表示对应接口或指令的完成情况，游戏内效果仍以业务结果、状态采样或事件为准。空响应和缺失输出不是数量零。任务后续读取只使用服务端声明的只读关系；如果读取结果已经属于另一项任务，会保留原回执并说明无法获取原任务进度。

游戏服务端更新先用 `dst_read` 调用 `api:previewGameRelease`，这一步只检查版本和影响范围，不要求群内确认。预检显示可以更新后，`dst_run` 准备 `api:publishGameRelease`；请求体所需的 `confirmed=true` 不能跳过插件向原用户展示的更新确认。确认消息只展示最近预检的房间范围、停服等待和备份选择，不展开安装能力清单或原始参数。提交后回复一个更新任务编号；应查询该任务的备份、更新和世界恢复状态，不能把提交回执称为更新完成。结果不确定时只查询原请求，不重新提交。

图片和常见文件可通过聊天适配器发送。单个下载限制 4 MiB；图片最多 2000 万像素；文件缓存最多 32 MiB、64 个文件，24 小时后按需清理。文件发送沿用 AstrBot 的文件服务配置：分容器部署须保证消息适配器能读取文件，或配置 AstrBot 的回调文件服务。投递异常不会自动重发。持续事件流不作为普通文件下载；订阅继续使用既有播报机制。

结果传给模型时最多 24 KiB，标记截断并保留原引用。密码、令牌等不在群里填写；敏感输入继续使用已授权的私密输入引用或面板配置，不把原始秘密写入准备记录。

## 接口开发约定

普通接口的源是 `docs/openapi-v2.yaml`；固定工作台的源是服务端内置定义。新接口不必登记 `BotCapabilities`、`capreg` 或插件 `TERMS`。旧固定能力和增强语义继续服务兼容客户端。

| 原始定义 | 作用 |
| --- | --- |
| `operationId`、summary、description、参数和响应 | 自动收录、检索、输入输出校验 |
| `x-dst-access` | 服务端执行策略：profile、access、confirmation、category；通用执行器用 executor 标明工作台/脚本边界 |
| `x-dst-intent` | 可选中文用途、别名和例句；缺少时仍从原接口生成可搜索信息 |
| Schema 的 `x-dst-reference` | 可选公共引用类型，如 room、world、player；常见 ID 参数名继续兼容 |
| Schema 的 `x-dst-task-state` | 原任务的状态字段，支持嵌套字段 |
| Schema 的 `x-dst-task-outcome` | 区分任务全部完成、部分完成和没有完成；部分完成不显示为全部成功 |
| Schema 的 `x-dst-read` 或操作的 `x-dst-result-reads` | 从原响应提取任务标识，再读取已登记的 GET 接口；可声明原任务身份匹配 |

`risk=low` 或 `readOnlyHint=true` 不能赋予读取权限。只读声明必须对应实际只读实现；新增操作默认要求面板管理权限，修改默认需要确认。身份、模型传输及旧机器人包装入口有明确排除原因。自定义命令及调用者填写的分类字段不能改变固定工作台授权。

检索同时使用接口用途和契约字段，接口用途的权重高于长返回结构中的偶然匹配。发现结果的 `matchingFields` 最多列出四项相关字段说明，方便模型判断该接口是否提供所需数据；这些说明来自契约，不包含真实玩家数据。新指标写入原响应契约后即可参与搜索，不需要在插件维护同义词表。

搜索索引按内容按需缓存，最多八个索引项；用途与字段索引分别计数，无空闲轮询、向量数据库或额外 Runtime 进程。插件只缓存近期 32 个发现结果，并在执行和回读时重新鉴权。

## 校验

```sh
go run ./cmd/capability-catalog -semantics docs/ai-tool-semantics.json
go run ./cmd/capability-catalog -semantics docs/ai-tool-semantics.json -check
go run ./cmd/openapi-manifest -check
go test ./internal/apicontract ./internal/aicontrol ./internal/httpapi ./routers
python -m unittest discover -s integrations/astrbot/tests -q
DST_GENERIC_PYTHON=python go test ./internal/httpapi -run TestCompanionGatewayRealRouterIdentityScopeAndDedup -count=1
```

最后一项启动本地测试 HTTP 服务、使用真实 AstrBot 框架和私有传输，验证仅服务端新增的接口可以被现有插件发现和调用。凭据和数据均为临时合成数据。逐接口发现、契约及对话回归不等于真实 QQ 收信、模型理解质量或游戏效果已验收；线上部署和群内测试需单独进行。
