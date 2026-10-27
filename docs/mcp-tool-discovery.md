# MCP 工具发现与返回结果

[English](mcp-tool-discovery.en.md)

默认 `/mcp` 保留精简工具目录。页面助手和外部客户端都可以用 `find_api`
按中文需求、故障现象、英文关键词或精确操作编号查找工具，再调用对应的
`api_<operationId>` 或 `call_api`。工具发现不会执行管理操作。

```json
{"query":"给房间设置定时备份","limit":5}
```

匹配结果包含英文 `summary`、中文 `purpose`、分类、运行前提和 `resultSummary`。
需要调用参数时加 `includeSchema: true`；需要完整业务返回结构时指定一个精确操作：

```json
{"query":"getRoomPlayerInventory","includeResultSchema":true}
```

`includeResultSchema` 一次返回一个操作的参数、回执 Schema 和 `resultContract`，
避免一次扩展大量响应结构。模糊查询使用该选项会返回参数错误；先检索，再使用
返回的 `operationId` 查询详情。接口运行条件和参数校验仍由原管理接口执行。

## 中文检索元数据

规范工具的 `description` 包含简短中文用途、原有英文说明和结果摘要。用于索引
的完整场景词在 `_meta["io.github.lcy0828.dst-admin/tool"]`：

| 字段 | 用途 |
| --- | --- |
| `cn_purpose` | 中文用途与能力边界 |
| `actions` | 明确操作及同义表达 |
| `symptoms` | 适用的需求或故障提问 |
| `slang` | 饥荒术语、技术词与简称 |
| `resultSummary` | 返回字段及结果解读提示 |
| `locale` / `semanticVersion` | 元数据语言和格式版本 |
| `catalogRevision` | OpenAPI 与中文词库共同决定的内容指纹 |

原有 `tags`、`access`、`risk`、`runtime`、弃用信息与工具名称保持兼容。
备份、通知等领域不会统一降权，按本次查询相关性排序。运行前提是能力说明，
不代表调用已经满足条件，也不引入新的审批步骤。

需要自建向量索引的客户端可分页读取 `/mcp?view=all`，使用上述元数据构造检索文本；
按 `catalogRevision` 缓存，在指纹改变时重建。分页仍使用 `nextCursor`，不应把
完整词库每轮全部发送给模型。默认面板检索在内存中完成，不请求 embedding 服务，
也没有常驻模型、向量数据库或闲时扫描任务。

## 正确理解返回结果

MCP `outputSchema` 描述实际调用回执，`structuredContent` 与文本结果包含相同数据。
以下是省略部分业务字段的结构示意，表示备份任务已提交：

```json
{
  "callId": "example-receipt",
  "status": "succeeded",
  "data": {
    "operationId": "createRoomBackup",
    "httpStatus": 202,
    "data": { "id": "example-job", "status": "queued" }
  }
}
```

- 外层 `status` 表示工具调用状态，不能当作内部备份、更新或重启任务的完成状态。
  HTTP 202 或内部 `queued` / `running` 需要继续查询任务。
- 常规 API 业务数据位于 `data.data`；`resultContract.responses` 按 HTTP 成功状态码
  给出原始业务数据 Schema，引用已转换为可独立解析的 `$defs`。
- 业务数据可能脱敏或截断。`_aiTruncated` 表示只返回部分内容，缺失字段不能解释为
  空、未开启或没有问题。完整业务 Schema 用于解读，不强制套在截断回执上。
- 调用失败可能返回 `status: failed` 与 `data.error`；结果不确定时为 `uncertain`，
  先核查实际任务或记录，再决定是否重试。协议、认证或参数错误也可能返回 MCP 错误。
- 下载和事件流工具返回 `data.resource` 认证链接。`fetched: false` 表示内容尚未读取；
  生成链接不代表已经下载文件、读取日志或建立持续订阅。
- 玩家历史观测不是当前在线状态，存档文件时间不是离线时间，已保存的模组配置也不代表
  游戏进程已加载。按每个工具的结果说明和实际采样时间判断。

## 自定义 Lua 的输出与回执

生成实体和发放物品时，优先使用 `executeRoomCommand` 的内置命令；`spawnAudit` 包含本次回执确认的[落点与去向](command-spawn-records.md)。原始 Lua 不会自动采集生成坐标。

需要查询数字或文本时，使用 `executeRawRoomCommand`，在代码中同步 `print(...)`。
例如通过 `call_api` 查询所选世界的名称（替换真实房间、世界编号与房间名）：

```json
{"operationId":"executeRawRoomCommand","arguments":{"path":{"roomId":"实际房间编号","worldId":"实际世界编号"},"body":{"command":"print(\"World:\", TheWorld.prefab)","confirmation":"实际完整房间名"},"requestId":"console-query-001"}}
```

规范 API 工具的业务回执位于 `data.data`，同步打印内容在其 `output.text` 中；
兼容工具 `world_console` 的业务回执直接位于 `data`。检查内层 `status` 与
`executionOutcome`，不要把外层工具成功或“已下发”当成游戏执行成功。
返回的 `id` 是命令运行编号，可用 `getCommandRun`（兼容工具为 `command_receipt`）
读取同一条结果；查询回执不会再次运行 Lua。

输出最多 16 KiB，`output.truncated` 表示截断。仅 `return` 数字不会变成打印输出；
定时器、协程等延后执行的打印仍在世界日志中。缺少 `output` 表示没有取得捕获能力证据，
空 `output.text` 表示没有捕获到同步打印，两者都不能解释为实体数量为零。
结果不确定时先读原回执，不自动重发；不同世界的统计应分别保留世界与回执对应关系。

`resultState` 统一表示结果证据：`waiting` 等待回执、`output` 已取得输出、
`no_output` 确认完成但没有同步打印、`output_unavailable` 已完成但不提供捕获输出、
`failed` 执行失败、`unconfirmed` 未确认。失败时即使有部分输出，也不能当成查询成功。

对于提供 `receiptRefreshUntil` 的未确认记录，可在提交后 2 分钟内设置
`query.refresh=true` 补读原始 Runtime 回执；兼容工具使用 `refresh:true`。例如：

```json
{"operationId":"getCommandRun","arguments":{"path":{"roomId":"实际房间编号","runId":"返回的运行编号"},"query":{"refresh":true}}}
```

`receiptCheck.status` 表示 `found` 已找到并保存、`not_found` 暂未找到、
`unavailable` 目标暂时不可读、`expired` 超过补查时间、`unsupported` 不支持补查。
暂未找到可能是尚未写入或已被后续命令覆盖，仍然属于未确认，不是数量零。
补查只读一次，不发送命令或探针；默认查询和历史列表只读数据库，没有后台补查轮询。

## 游戏更新的调用与跟踪

用 `find_api` 查询精确操作编号并加 `includeSchema: true`，可以拿到完整 arguments
示例；规范 `api_` 工具的描述也包含同一示例。示例中的编号和哈希需要替换为实际值。
`path` 必须是嵌套对象，`confirmation` 必须是预检返回的哈希字符串。

例如，使用 `call_api` 预检本机安装：

```json
{"operationId":"previewGameRelease","arguments":{"body":{"installations":[{"targetId":"local","installationId":"default"}],"policy":{"restartRunning":true,"loadConfirmation":"logs","timeoutSeconds":300}},"requestId":"game-preview-001"}}
```

预检会按需刷新受影响安装的清单，无需用户打开页面或手动刷新所有节点。
预检就绪后，将返回的精确范围、策略和 `planHash` 交给 `publishGameRelease`；
`confirmation` 使用同一 `planHash`。不同操作使用不同 `requestId`，仅同一次请求的
安全重发复用编号。

| 操作 | 查询编号与用途 |
| --- | --- |
| `getGameRelease` | 使用原 `releaseId`，查看备份、更新、分片恢复及恢复建议 |
| `getJob` | 使用本次返回的 `jobId`，查看任务状态与目标执行结果 |
| `retryGameRelease` | `path.releaseId` 使用原编号，返回新的重试 Job |
| `getGameUpdateRun` | 仅用于旧 `game.update` 流程，不用于 `game.release` 或其重试 |

首次提交返回的 Job ID 同时也是 Release ID；重试后两个编号不同。以下是 `call_api`
的查询和重试参数，替换示例编号后使用：

```json
{"operationId":"getGameRelease","arguments":{"path":{"releaseId":"24a00000-0000-4000-8000-000000000001"}}}
{"operationId":"retryGameRelease","arguments":{"path":{"releaseId":"24a00000-0000-4000-8000-000000000001"},"requestId":"game-retry-001"}}
{"operationId":"getJob","arguments":{"path":{"jobId":"24a00000-0000-4000-8000-000000000002"}}}
```

`stage=protecting` 表示正在维护保护备份。维护期间暂时停服不代表独立崩溃。
进度 100%、`outcome=none` 或旧更新日志不存在，也不能作为成功或回滚的证据；
结合本次 Job 和原 Release 的实际版本、验证与恢复结果判断。

## 数据来源与维护

中文词库位于 [ai-tool-semantics.json](ai-tool-semantics.json)，以 OpenAPI `operationId`
为键。初始词汇由 [hxyinan 在 issue #23](https://github.com/lcy0828/dst-admin-go/issues/23)
贡献，源自其 [291 条检索元数据](https://gist.github.com/hxyinan/70eaf31ce16c63e9b45f257b2d8cbfbd)。
项目按实际接口修正用途、运行条件和场景表达，并补充后续接口；原附件的检索测试数字
不代表当前面板的检索效果。

新增可调用接口时需同步维护用途、操作表达、提问示例和术语；返回字段来自 OpenAPI，
不要在词库复制另一份参数或返回结构。CI 检查工具覆盖、过期条目、Schema 引用、中文
检索场景以及 MCP 新旧回执兼容性。弃用别名有明确替代入口时继承其中文词汇，保留弃用提示。
