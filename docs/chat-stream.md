# 游戏聊天推送接入

需要持续接收游戏聊天、系统公告或模组播报时，使用房间聊天流。它读取与面板「聊天记录」相同的数据，自动合并可确认的跨世界副本，兼容本机和 Agent 上的世界。不需要 AI 参与，也不执行游戏指令。

只包含游戏聊天日志中已有、能够解析的记录：玩家发言、私聊、`[Announcement]` 及 `[... Announcement]` 类公告。模组若只 `print` 到普通服务器日志，或使用自定义格式，不会自动变成聊天事件。进退服判断仍可使用 Runtime 事件流；聊天公告不能代替玩家在线状态。

## HTTP 接口

`GET /api/v2/rooms/{roomId}/chat-logs/stream`

需要登录或有效接入密钥，权限与读取房间聊天记录一致。沿用现有权限策略：房间受限的面板管理密钥只能读授权房间；「游戏查询」和「游戏管理」预设未因此开放聊天历史或私聊。

| 参数 | 位置 | 用途 |
| --- | --- | --- |
| `roomId` | 路径 | `listRooms` 返回的真实房间 ID，不是目录路径或房间名 |
| `kind` | query，可省略 | `say` 公屏、`whisper` 私聊、`announcement` 公告；省略则返回全部三类 |
| `cursor` | query，可省略 | 上一次成功处理并保存的 SSE `id`，保持原样 |
| `Last-Event-ID` | 请求头，可省略 | 同样用于续读；显式 `cursor` 优先 |

以下变量由接入方在自己的环境设置，示例不包含密钥或玩家数据：

```bash
curl --no-buffer --fail-with-body \
  --header "Authorization: Bearer ${DST_API_TOKEN}" \
  "${DST_PANEL_URL}/api/v2/rooms/${DST_ROOM_ID}/chat-logs/stream?kind=announcement"
```

首次连接不传游标，从**已存变更的当前末尾**开始。需要之前的聊天，先调用 `GET /api/v2/rooms/{roomId}/chat-logs` 分页读取。随后才导入数据库的旧日志也可能出现在流里，不能把收到事件的时间当成玩家发言时间。

每次成功处理事件后保存 SSE `id`；断线或控制端重启后，以 `Last-Event-ID` 重连。游标绑定房间和 `kind`，切换范围要重新建立订阅。无效游标返回 422，不会自动清空后重试。

## 事件与去重

地面和洞穴可能同时记录同一句公屏。面板只在不同分片间合并内容、玩家相同且相差不超过 30 秒的记录，并按来源顺序配对；同一分片的重复发言保留独立 ID。游戏日志没有原生消息 ID，时间或配对存在歧义时保留记录。第二个分片补充来源只产生 `updated`，插件不会再次播报。

合成示例（`id` 行中的游标仅为格式示意，实际接入必须使用接口返回的值）：

```text
event: chat.record
id: c1.<scope>.<epoch>.42
data: {"id":"example-record","operation":"created","kind":"announcement","recordedAt":"2026-01-01T00:00:02Z","historical":false,"entry":{"id":"example-record","kind":"announcement","content":"示例模组：等级提升","sourceTimestamp":"00:00:01","occurredAt":"2026-01-01T00:00:01Z","sources":[{"worldId":"example-world","worldName":"Master","worldRole":"master"}]}}

```

| SSE 事件 | 含义与接入方处理 |
| --- | --- |
| `chat.cursor` | 当前续读位置，包括被 `kind` 筛掉的变更；保存 `id`，不要转发成聊天 |
| `chat.record` | 已提交的聊天变更；正文结构见 OpenAPI `ChatLogChange` |
| `chat.status` | 采集状态和最近完整同步时间；`partial=true` 表示不能确认完整覆盖 |
| `chat.gap` | 游标早于保留范围；标记缺口，保存新 `id`，继续接收保留范围内的记录 |
| `chat.reset` | 数据库恢复、替换或游标超前；标记覆盖变化，保存新 `id`，从当前末尾继续 |
| `chat.error` | 当前流无法继续读取；保留最后成功提交的游标，退避重连 |
| `chat.reconnect` | 本连接到期，需要带原认证和最后成功提交的游标重新连接 |

`chat.record.operation`：

- `created`：数据库新建记录。转发聊天通常只处理这一种，并检查 `entry.occurredAt` 是否属于需要的时间范围。
- `updated`：同一记录补充其他世界来源或修正历史时间。更新本地记录，不再播报一次。
- `deleted`：历史来源纠正时移除旧记录；此时没有 `entry`，不代表删除了游戏聊天或修改了世界。

SSE `id` 是**变更游标**，正文 `id` 是**聊天记录 ID**。按 SSE `id` 去重重放的变更，按正文 `id` 更新记录；只播报新消息时再用正文 `id` 去重。不要只按「记录 ID + operation」丢弃所有重复更新，同一记录可能多次补充来源。

`historical=true` 表示归档/修复导入；`false` 也不保证刚发生。`truncated=true` 表示内容超过单帧上限，`entry` 可能缺失；不要把它当成完整消息，原记录仍可从聊天历史读取。`timeEstimated=true` 或缺少 `occurredAt` 时不能承诺精确发言时间。

## MCP

工具名称：`api_streamRoomChatLogs`。可通过 `find_api` 搜索「聊天推送」「模组公告」并设置 `includeSchema=true`，授权的 `view=bot`、完整目录中也可发现。`view=tasks` 保持小型任务工具目录，插件的后台播报连接仍需具备对应读取工具。

```json
{
  "path": {"roomId": "example-room"},
  "query": {"kind": "announcement"}
}
```

返回形态：

```json
{
  "resource": {
    "url": "/api/v2/system/ai/resources/example-resource",
    "kind": "stream",
    "authenticated": true,
    "fetched": false
  }
}
```

此对象位于 MCP 执行回执的 `data` 中，实际返回还包含名称、到期时间等信息。取得链接不等于完成订阅；接入程序必须携带原凭据向**同一面板来源**发起 SSE GET，并持续消费。链接不携带密钥，不能匿名访问或转交其他调用者。

建议创建链接时不固定 `cursor`，在链接有效期内复用，通过每次 GET 的 `Last-Event-ID` 续读。这样不必每次重连再调用模型或创建新资源。每条连接最长 5 分钟，重连会重新检查权限；资源链接有效期以 `expiresAt` 为准。

## AstrBot 与兼容性

新版统一助手检测到新工具后优先使用 SSE，一房间共用一条聊天流；旧面板没有此工具时，仍用现有聊天分页轮询。认证链接只用于后台连接，不需要把聊天交给模型。

现有「聊天播报」继续只转发 `say`，不会自动公开私聊或将进退服公告重复播报。第三方接入需要模组公告时，订阅 `kind=announcement`；需要完整记录时省略 `kind`，由接入方自行控制可见范围。

插件把待发送消息、去重记录和游标一起保存；保存失败不确认新游标。正常重连续读持久化变更。多个群的游标不一致时，先捕获当前末尾，再分别补查近期公屏，并保留去重记录：仅补播最近 2 分钟、最多读取 400 条，超出范围在插件诊断日志提示，避免长时间断线后刷屏。第一次启用只建立基线，不补播旧消息。QQ 发送结果不确定时沿用现有机制，不自动重复投递。

## 资源限制与故障处理

- 无订阅者时不增加后台采集；同房间订阅者共用采集任务。通常每 2 秒同步一次，收到已提交记录后推送；这是持续 HTTP 推送，并非游戏进程零延迟直连。Agent 离线、历史补读和 QQ 投递会增加延迟。
- 最多 8 个活跃房间、32 条连接、每房间 8 条连接，同时最多采集 2 个房间。达到上限返回 429 与 `Retry-After: 5`。
- 每房间最多保留 2048 条变更或约 4 MiB 逻辑变更数据，单条载荷最多 64 KiB；不承诺固定小时数。普通聊天历史不因流的保留限制被删除，数据库文件大小也不等同于逻辑限额。
- 每 15 秒发送心跳注释；慢连接写入最多等待 5 秒。反向代理应关闭响应缓冲，并允许长连接。
- 若需要可靠归档，消费者应先持久化消息及游标，再确认本地处理完成。断线可能重放，不能承诺外部平台恰好投递一次。
