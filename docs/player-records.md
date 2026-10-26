# 玩家记录与存档 Session

玩家管理默认显示当前存档的记录。切换“历史存档”可以查看重置前保留的玩家档案；
历史记录只读，不表示当前在线，也不能直接对当前游戏执行玩家操作。

- Session 属于每个世界分片。Master 与 Caves 的 Session 不必相同；普通重启不会创建新的存档记录。
- 面板收到可信的新 Session 采集后，会将该分片旧玩家档案归入历史。相同 KU ID 在新存档中重新加入时，年龄、角色和生命等属性重新采集，不沿用旧局数值。
- 从旧版本升级时，没有 Session 依据的已有记录会在首次识别 Session 时归入历史；当前玩家由有效采集恢复。无 Session 的旧采集端仍可使用，但无法提供完整的重置识别能力。
- 已离线或状态过期的玩家，其之前采集的游戏属性不会继续标为实时。首页“状态已过期”包含离线且状态过期的记录；后台无人维护使用的在线人数口径保持保守。
- 未确定 Session 的旧日志不能补入已经确定的新存档。此行为不修改游戏存档、封禁名单或白名单。

API：`GET /api/v2/rooms/{roomId}/players?scope=current|history`，默认 `current`。
`sessionId` 表示记录对应的分片存档；`historical` 和 `historyId` 标识历史记录。
汇总 `stale` 包含全部状态过期记录；`staleOnline` 只包含最后已知在线且观测过期的记录。

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
