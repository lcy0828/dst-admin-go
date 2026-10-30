# 玩家记录与存档 Session

玩家管理默认显示当前存档的记录。切换“历史存档”可以查看重置前保留的玩家档案；
历史记录只读，不表示当前在线，也不能直接对当前游戏执行玩家操作。

- Session 属于每个世界分片。Master 与 Caves 的 Session 不必相同；普通重启不会创建新的存档记录。
- 面板收到可信的新 Session 采集后，会将该分片旧玩家档案归入历史。相同 KU ID 在新存档中重新加入时，年龄、角色和生命等属性重新采集，不沿用旧局数值。
- 从旧版本升级时，没有 Session 依据的已有记录会在首次识别 Session 时归入历史；当前玩家由有效采集恢复。无 Session 的旧采集端仍可使用，但无法提供完整的重置识别能力。
- 已离线或状态过期的玩家，其之前采集的游戏属性不会继续标为实时。首页“状态已过期”包含离线且状态过期的记录；后台无人维护使用的在线人数口径保持保守。
- 未确定 Session 的旧日志不能补入已经确定的新存档。此行为不修改游戏存档、封禁名单或白名单。

房间在线人数按真实连接的 KU ID 去重，包含选角、加载和跨分片迁移中的玩家。世界人数只计算当前分片的本地玩家实体，所以房间在线数可能大于各世界人数之和。没有实体归属时显示“位置确认中”；悬停可查看最后确认的位置，这不表示当前仍在该世界。虚拟 `[Host]` 不计入玩家。

API：`GET /api/v2/rooms/{roomId}/players?scope=current|history`，默认 `current`。
`sessionId` 表示记录对应的分片存档；`historical` 和 `historyId` 标识历史记录。
汇总 `stale` 包含全部状态过期记录；`staleOnline` 只包含最后已知在线且观测过期的记录。

页面与 MCP 查询当前玩家列表或详情时，会按需合并 Runtime 已生成的有效快照，不必等待定时刷新任务。相同房间的并发查询共享结果，五秒内复用；全局最多同时同步两个房间，每次运行节点读取预算为两秒。此过程不发送 Lua、不安装或修复 Runtime，也不启动后台轮询。历史存档查询不触发同步。采集过期、节点离线或同步忙时保留已有记录；列表通过 `warnings` 明确说明本轮未确认的状态，原先离线的记录也会在本次响应中标为状态过期，保留原时间，不能将读取失败当作无人在线。原有主动刷新入口继续用于主动采集与恢复。

## English

Player management shows current-save records by default. Select **Previous saves**
to inspect read-only archived records. These records do not represent current
online players and do not offer current-game actions.

Each shard has its own Session. A validated new Session archives that shard's old
records without changing saves or access lists. Normal restarts preserve the same
Session. Returning KU IDs receive fresh gameplay attributes, including a valid
zero age. Unscoped records from older panel versions are archived when a Session
is first established; current players return through valid observations. Older
collectors without Session evidence cannot fully detect save resets.

Offline or stale gameplay attributes are not marked live. The `scope=current|history`
query selects current or archived records; `sessionId`, `historical` and `historyId`
identify their source. The `stale` total includes offline records with stale presence,
while `staleOnline` retains its conservative online-maintenance meaning.

Room online counts unique real connections, including character selection, loading and shard migration. Per-world counts include only shard-local entities, so their sum can be below room online. Players without a confirmed current shard show a pending location; the tooltip can show the last confirmed world as history. Dedicated `[Host]` entries are excluded.

Current player list and detail reads, including MCP, merge already-published valid Runtime snapshots on demand. Concurrent reads share each room's result for five seconds; at most two rooms synchronize at once, with a two-second runtime-read budget. This never sends Lua, installs or repairs Runtime, or starts idle polling. History reads skip synchronization. Stale snapshots, offline nodes and busy synchronization preserve existing records; list `warnings` explain that current presence could not be confirmed. A failed read is not evidence that nobody is online. Explicit refresh remains available for active collection and recovery.
