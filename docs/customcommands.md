# DST `customcommands.lua` 能力与集成设计

> 文档状态：设计与实现基线 v1.4
> 更新时间：2026-08-15
> 适用仓库：`dst-admin-go`、`dst-admin-vue`
> 目标：明确 `customcommands.lua` 能做什么、适合做什么，以及 DST Admin 应如何安全使用它

## 0. 当前实现状态

截至 2026-08-15，阶段 A 至阶段 E 已完成代码实现和自动化验收：

- Runtime `2.3.1` 以唯一受管块接入每个分片，不覆盖用户已有脚本。
- 安装、升级、状态、卸载、完整备份和校验回滚 API 已接通；卸载和回滚要求分片停止及精确房间名确认。
- `Start/Stop` 幂等，`Reload` 重新读取受管模块；候选加载或启动失败时保留或恢复旧实例。
- 玩家 A/B JSON 快照、健康信标、Session/Shard/sequence/时间校验和损坏槽回退已实现。
- 房间级多分片一次事务刷新、字段级 `live/stale/unavailable`、原生日志补充及旧控制台探针 fallback 已实现。
- 世界状态每 5 秒写入 A/B JSON 快照，完整保留 17 项季节、昼夜、天气、环境与洞穴指标；Go 严格校验版本、Session、Shard、实例、sequence、时间和有限字段，快照有效时不发送控制台 Lua。
- `pause_when_empty=true` 造成模拟暂停时，DST 的普通和 static scheduler 都不会继续周期任务；快照过期后 Go 发送短入口 `DSTAdmin.Refresh()`，等待玩家、世界和 Health sequence 同时前进，再读取一致结果。
- Runtime 未安装、版本过旧、快照缺失、损坏、错 Session/Shard 或过期时，世界状态刷新只执行一次原 nonce 日志探针 fallback；请求取消或超时后不会再触发 fallback。
- 玩家页已展示 Runtime 正常、fallback、降级和不可用状态，并提供安装或修复入口。
- 玩家操作已迁移到固定允许列表短命令，使用 A/B 结构化回执；发送后未收到回执时不会自动重试，避免重复执行危险动作。
- 世界事件使用最多 128 条的 A/B 小批次；Go 按 Runtime 实例和 sequence 合并、去重，并校验当前 Session 与 Shard 数据。
- 世界诊断只开放 `summary`、`prefab`、`performance` 三种档位；实体样本最多 50 条，性能采样 1 至 5 秒且最多 50 个样本，完成或停止时自动取消任务。
- 世界状态页已接入最近事件、最近诊断和手动诊断入口。

真实 Linux 空服验收已在 Debian 12、DST build `747465` 上完成：分片保持 `Sim paused` 25 秒后，玩家和世界快照均会过期；短刷新可同步推进两个快照和 Health sequence，且不需要解除暂停。

当前尚未完成：

- 真实 DST 在 macOS、满员及代表性 Mod 组合上的上线验收。自动化测试与 Debian 空服验收不能替代剩余平台和 Mod 组合验证。

## 1. 结论

`customcommands.lua` 是 DST 在每个分片启动时加载的本地 Lua 扩展入口。它不需要安装 Workshop Mod，可以直接访问当前分片的世界、玩家、网络表、组件、事件和调度器，因此很适合作为 DST Admin 的轻量运行时适配层。

它在项目中的正确定位是：

```text
DST Admin Go 控制面
        |
        | 安装、升级、调用、读取结果
        v
customcommands.lua 受管加载块
        |
        v
dst-admin/*.lua 运行时模块
        |
        +-- 事件通知
        +-- 小型实时快照
        +-- 有边界的管理动作
        +-- 按需诊断采样
```

它不应成为新的业务后端、数据库、规则引擎或长期任务平台。鉴权、审计、保护备份、跨分片编排、持久化、重试、API 和 UI 状态仍由 Go 负责。

## 2. 已验证的加载机制

DST 当前脚本通过以下逻辑加载自定义命令：

```lua
TheSim:GetPersistentString("../customcommands.lua", function(load_success, str)
    if load_success then
        local fn = loadstring(str)
        known_assert(fn ~= nil, "CUSTOM_COMMANDS_ERROR")
        xpcall(fn, debug.traceback)
    end
end)
```

因此文件是分片级的，而不是 Cluster 共享一份：

```text
<DST_SAVE_PATH>/<Cluster>/Master/customcommands.lua
<DST_SAVE_PATH>/<Cluster>/Caves/customcommands.lua
<DST_SAVE_PATH>/<Cluster>/<OtherShard>/customcommands.lua
```

重要语义：

- 地面、洞穴和其他分片分别加载自己的文件。
- 在地面执行的函数默认只能访问地面世界实体；洞穴同理。
- 文件在 `Start()` 阶段异步加载，此时 `TheWorld` 可能尚未创建。
- 文件语法错误可能触发 `CUSTOM_COMMANDS_ERROR`，因此更新前必须离线校验。
- 修改文件不会自动让运行中的分片重新执行；需要重启分片，或通过受控热加载入口只加载 DST Admin 模块。
- 它与普通 Mod 可以同时存在，但共享同一个 Lua 运行时和全局命名空间。

参考资料：

- [DST `consolecommands.lua` 镜像](https://github.com/taichunmin/dont-starve-together-game-scripts/blob/master/consolecommands.lua)
- [自定义饥荒服务器后台命令](https://peppernotes.top/2019/11/dstcustomcmd/)
- 本机当前 DST `scripts/mainfunctions.lua` 与 `scripts/consolecommands.lua`

博客最后更新于 2021 年，GitHub 镜像中的目标文件最后更新于 2024 年。实现和验收应以目标机器当前安装的 DST 脚本为事实源。

## 3. 可用的核心运行时能力

### 3.1 当前分片玩家

两个数据源用途不同：

| 数据源 | 适合读取 | 限制 |
| --- | --- | --- |
| `AllPlayers` | 玩家实体、角色和服务端组件 | 只包含当前分片，不直接提供完整网络信息 |
| `TheNet:GetClientTable()` | `userid`、名称、角色、管理员、`netid`、`netscore`、`playerage` | 包含专服虚拟宿主，需要正确过滤 |

官方 `c_listplayers()` 使用以下规则过滤 Dedicated Server 虚拟宿主：

```lua
local isdedicated = not TheNet:GetServerIsClientHosted()
for _, client in ipairs(TheNet:GetClientTable() or {}) do
    if not isdedicated or client.performance == nil then
        -- real player
    end
end
```

不能依赖“跳过第一项”或固定数组下标。玩家身份应以非空、格式有效的 `userid` 关联，实体与网络表按 `userid` 合并。

可以安全读取的玩家信息包括：

- Klei User ID、显示名称、角色 Prefab、管理员状态和平台 NetID。
- 当前分片、在线状态、玩家存活天数和网络质量等级。
- 生命、饥饿、理智、温度、潮湿度等存在的服务端组件。
- 幽灵、睡眠、战斗等可由标签或组件稳定表达的当前状态。
- 位置、装备和物品栏摘要，但必须单独配置采集等级并限制数据量。

组件不是稳定数据库字段。Mod 可能替换、删除或扩展组件，因此每个字段都必须独立判断并通过 `pcall` 隔离。

### 3.2 世界状态

`TheWorld.state` 和当前世界组件可提供运行时状态，例如：

- 世界天数、时间阶段、季节及季节进度。
- 降雨、积雪、湿度、环境温度、月相。
- 洞穴噩梦周期等分片特有状态。
- 世界标签、当前 Shard ID、Session ID 和连接分片信息。
- 部分 Boss、裂隙、事件和世界组件的当前状态。

用途包括仪表盘、状态时间线、告警和世界状态快照。读取时必须区分：

- 官方公开状态字段。
- 当前版本可观测但未承诺稳定的内部组件。
- Mod 自定义字段。

后两类需要携带 `unsupported` 或 `unknown`，不能因为字段不存在就判定事件没有发生。

### 3.3 世界实体

可以通过 `Ents` 或 `TheSim:FindEntities()` 查询当前分片已加载的实体，支持：

- 按 Prefab、标签、位置或范围计数。
- 获取基地周边玩家、建筑、资源和危险源摘要。
- 定位特定 Boss、传送点、入口和异常实体。
- 检测实体数量突增、重复 GUID 或可疑刷物。
- 为按需诊断返回有限的 Prefab/坐标/GUID 列表。

但它不是完整存档地图解析器：

- 休眠、未加载或不在当前模拟范围内的数据未必等同于完整 Session 内容。
- 遍历全部 `Ents` 会占用主模拟线程，不能高频执行。
- 大范围空间分析和全世界资源分布应由 `dst-map-renderer` 读取 Session 快照完成。

### 3.4 事件监听

在 `TheWorld` 或玩家实体上使用 `ListenForEvent`，可以将轮询改造成事件触发。适合关注：

- 玩家进入、离开、生成、死亡、复活和跨分片迁移。
- 角色实体建立后绑定生命、制作、战斗等有限事件。
- 天数、阶段、季节、降雨和其他世界状态变化。
- 世界保存、回档、重置和关闭前后的生命周期信号。
- Vote、公告以及明确有稳定事件名的管理状态。

事件监听器必须可以成组解绑。热升级时先卸载旧监听器，再安装新监听器，不能不断叠加回调。

### 3.5 调度与延迟执行

可使用：

- `scheduler:ExecuteInTime()`：世界创建前进行有限次数的就绪检查。
- `TheWorld:DoTaskInTime()`：世界就绪后延迟执行一次。
- `TheWorld:DoPeriodicTask()`：执行小型周期采集。
- Task 的 `Cancel()`：停止任务和完成热升级。

周期任务只适合低成本、固定上限的采集。所有任务必须有句柄、命名空间和 `Stop()`，不得生成无法取消的匿名永久任务。

### 3.6 本地持久化

可使用 `TheSim:SetPersistentString()` 将结构化数据写入当前分片的持久化目录。适合保存：

- 玩家实时快照。
- 采集器健康与版本信息。
- 小型事件增量批次。
- 短时间性能采样结果。
- 管理动作的游戏内执行回执。

建议目录：

```text
<Shard>/save/mod_config_data/dst-admin/
```

写入使用 JSON，不再使用 Lua `return` 文件。单个文件必须有大小上限，异步写入未完成时不能启动下一次写入。

## 4. 适合建设的产品能力

### 4.1 P0：玩家 Telemetry

这是当前最值得恢复的能力：

- 原生日志即时维护加入、离开和基础身份。
- Lua 每 5 秒采集当前分片玩家和生存指标。
- A/B JSON 快照防止写入中断造成整份数据不可用。
- Go 聚合房间内所有分片，处理地面与洞穴迁移。
- API 明确返回 `live`、`delayed`、`degraded` 或 `unavailable`。

该能力只读，不修改游戏状态，风险最低。

### 4.2 P0：运行时健康信标

每个分片输出一个很小的健康记录：

- 模块版本、协议版本和启动实例 ID。
- 当前 Shard、Session 和游戏 Build。
- 是否已经启动，周期任务是否存在。
- 最近采集时间、最近成功写入时间和最近错误。
- 当前玩家数、采集耗时和连续失败次数。

Go 可以据此区分“服务器没有玩家”和“采集器已经死亡”。

### 4.3 P1：世界状态事件流

在现有世界状态日志解析之外，可以补充：

- 天数和昼夜阶段变化。
- 季节切换、降雨开始/结束、月相变化。
- 洞穴噩梦周期等分片状态。
- 服务器保存、回档和重置生命周期。

这些信息适合生成运维时间线，但不能把事件文件当作唯一真相。服务重启后仍需通过当前状态快照对账。

### 4.4 P1：受控管理命令

可以将现有的长内联 Lua 重构成参数化函数：

```lua
DSTAdminCommands.Kick(userid)
DSTAdminCommands.Announce(message)
DSTAdminCommands.Resurrect(userid)
DSTAdminCommands.SetGodMode(userid, enabled)
DSTAdminCommands.SetCreativeMode(userid, enabled)
DSTAdminCommands.ChangeCharacter(userid)
```

收益：

- Go 只发送短调用，日志不会记录整段 Lua。
- 参数校验和实体查找集中在一个版本化模块。
- 每个动作可以返回统一的 `code/message/details` 回执。
- 可以在游戏更新后对模块做集中兼容测试。

这不能代替 Go 层权限检查。Go 仍需负责管理员鉴权、参数限制、房间确认、风险分级、Job 和审计。

### 4.5 P1：按需诊断包

由用户明确触发，在数秒内采集后自动停止：

- 当前玩家、分片、世界状态和连接分片摘要。
- 目标 Prefab/标签的数量和有限样本。
- 周期 Tick 延迟的短时采样。
- 当前加载 Mod 的运行时可见信息。
- 关键世界组件是否存在及简短调试字符串。

诊断结果必须有限制：采样时长、实体数、递归深度、字符串长度和输出文件大小。默认只读，不调用 `c_dump()` 等无界日志命令。

### 4.6 P2：性能采样探针

可以在原版 VM 中做低侵入采样：

- Telemetry 自身单次采集耗时和最大耗时。
- 主线程短时 Tick 间隔分布。
- 指定模块或管理函数的调用耗时。
- 实体总量及高频 Prefab 数量变化。
- 保存前后耗时和短期卡顿关联证据。

这只能用于趋势和异常提示，不能单靠 Lua 探针得出完整 CPU、内存或 GC 根因。进程 CPU/RSS、系统磁盘和网络仍由 Go Agent 获取；LuaJIT A/B 仍属于隔离的 Performance Lab。

### 4.7 P2：地图与资源分析辅助

`customcommands.lua` 可以补充地图渲染器无法从单个静态快照直接获得的运行时信息：

- 在线玩家的实时位置和所在分片。
- 当前加载范围内的动态 Boss 或事件实体。
- 用户选定基地半径内的资源与危险源摘要。
- 某 Prefab 当前实例数量的轻量复核。

完整地形、全部资源、未知 Mod 实体和快照差异仍由 Renderer 负责。Lua 输出是“运行观测”，不能覆盖 Session 解析结果。

### 4.8 P2：服务器事件与运营辅助

在管理员明确配置后，可以支持：

- 定时或事件触发的游戏内公告。
- 开服欢迎、关闭倒计时和维护提醒。
- 玩家加入后发送简短规则提示。
- 根据在线人数或世界阶段触发只读通知。
- 将重要游戏事件写入运维时间线。

公告调度策略、文案、多语言、静默时段和审计由 Go 管理；Lua 只执行当前分片的最终动作。

## 5. 可以做，但默认不应自动做

以下能力技术上可行，但会修改游戏状态，必须由显式操作触发：

| 能力 | 风险 | 必要保护 |
| --- | --- | --- |
| 踢出、封禁、解封 | 玩家访问状态变化 | 鉴权、目标确认、审计 |
| 复活、死亡、换角色 | 玩家状态变化 | 在线分片确认、可见反馈 |
| 发放物品、修改属性 | 破坏公平性和存档状态 | 高风险权限、参数白名单 |
| 传送玩家或跨分片迁移 | 可能卡住玩家或破坏流程 | 目标校验、兼容测试 |
| 生成或删除实体 | 可能永久改变存档 | 保护备份、预览、数量上限 |
| 修改时间、季节和天气 | 全局世界变化 | 保护备份、全服确认 |
| 保存、回档和重置 | 数据丢失风险 | 专用 API、二次确认、Job |
| 启用维持生命等周期效果 | 任务残留和玩法改变 | 明确 Stop、重启恢复策略 |

禁止把这些函数加入自动恢复流程。Telemetry 失败只能恢复采集，不能顺带修改玩家或世界。

## 6. 不应交给 `customcommands.lua` 的能力

### 6.1 身份、鉴权和审计

Lua 无法可靠确认 Web 请求者身份，也不应保存后台 Token。以下内容必须留在 Go：

- 登录、Session、角色权限和 API Token。
- 危险动作确认、审批和审计记录。
- 请求限流、幂等键和 Job 生命周期。
- 多用户、多商户和远程节点边界。

### 6.2 文件系统管理

Mod 下载、配置编辑、存档导入、备份、恢复和服务更新必须由 Go 完成。Lua 只应写入固定持久化文件名，不允许接受来自 API 的任意文件路径。

### 6.3 完整地图与离线存档分析

运行时实体不是完整 Session 的替代品。地图渲染、历史快照、资源分布和离线存档兼容仍由独立 Renderer 完成。

### 6.4 长期数据库和复杂业务规则

不要在 Lua 中长期累积玩家历史、封禁理由、运营配置或推荐规则。DST 崩溃、回档和分片重启都会破坏这类状态的一致性。

### 6.5 无边界遍历和持续 Profiling

以下操作不能成为默认周期任务：

- 高频遍历全部 `Ents`。
- 输出完整实体、组件或 Lua 堆。
- 持续调用 `c_dump()`、`c_dumpentities()` 或调试渲染。
- 在每个 Tick 做 JSON 编码或磁盘写入。
- 执行用户提交的任意 Lua 表达式。

## 7. 推荐模块结构

每个分片保留很小的加载入口：

```text
customcommands.lua
dst-admin/
├── bootstrap.lua
├── telemetry.lua
├── worldstate.lua
├── commands.lua
├── events.lua
└── diagnostics.lua
```

建议命名空间：

```lua
_G.DSTAdmin = {
    version = "2.3.1",
    protocolVersion = 2,
    Telemetry = {},
    WorldState = {},
    Commands = {},
    Events = {},
    Diagnostics = {},
}
```

模块公共接口固定为：

```lua
DSTAdmin.Start()
DSTAdmin.Stop()
DSTAdmin.Status()
DSTAdmin.Reload()
DSTAdmin.Refresh()

DSTAdmin.Telemetry.EmitOnce()
DSTAdmin.WorldState.EmitOnce()
DSTAdmin.Commands.Execute(request_json)
DSTAdmin.Diagnostics.Capture(request_json)
```

外部只能调用这组受控入口。内部函数保持 `local`，不要新增 `res()`、`list()`、`count()` 等可能与用户脚本或 Mod 冲突的短全局名称。

## 8. 启动、停止与热升级

### 8.1 启动

`customcommands.lua` 加载早于 `TheWorld` 就绪。受管加载块应：

1. 异步加载 DST Admin `bootstrap.lua`。
2. 同步检查 `TheWorld.ismastersim`、`TheNet` 和持久化能力；已经就绪时直接启动，避免空服暂停后 scheduler 永远不回调。
3. 尚未就绪时使用 `scheduler:ExecuteInTime()` 做有限次数的重试。
4. 调用幂等的 `DSTAdmin.Start()`。
5. 超过等待上限后只记录一次结构化错误，不无限重试。

### 8.2 停止

`Stop()` 必须：

- 取消全部一次性和周期任务。
- 移除全部事件监听器。
- 等待或放弃正在进行的异步写入。
- 写入最终健康状态，但不得阻塞服务器关闭。
- 多次调用仍返回成功。

### 8.3 热升级

不能重新执行用户完整的 `customcommands.lua`。推荐两阶段切换：

1. 将新模块加载到临时命名空间并校验接口、协议和版本。
2. 校验成功后停止旧模块，切换 `_G.DSTAdmin` 并启动新模块。
3. 新模块启动失败时尝试恢复旧实例。
4. Go 记录升级结果；无法安全恢复时提示重启目标分片。

### 8.4 空服暂停与主动刷新

周期任务只用于服务器未暂停时降低控制台调用频率，不能作为读取当前状态的可靠前提。玩家或世界快照过期时：

1. Go 校验目标进程、Runtime 安装状态、当前 Session 和 Shard 身份，但不要求旧 Health 新鲜。
2. Go 只发送 `DSTAdmin.Refresh()`；Lua 先完成世界快照，再完成玩家快照并发布包含两个最新 sequence 的 Health。
3. Go 只有在玩家、世界和 Health 的 Session/Shard 匹配且两个 sequence 都前进后才返回成功。
4. 主动刷新失败或 Runtime 不兼容时，玩家和世界采样继续使用原有长 Lua nonce 探针 fallback。

单分片房间在 `cluster.ini [SHARD] shard_enabled=false` 时，DST 的 `TheShard:GetShardId()` 返回 `0`，即使 `server.ini` 仍保留 `id=1`；Go 按运行时真实身份 `0` 校验。启用分片时才使用各 `server.ini [SHARD].id`。

## 9. 文件协议

### 9.1 玩家 A/B 快照

```json
{
  "schemaVersion": 2,
  "producerVersion": "2.3.1",
  "producerInstanceId": "runtime-random-id",
  "sessionId": "dst-session-id",
  "shardId": "1",
  "sequence": 42,
  "capturedAtUnix": 1786500000,
  "complete": true,
  "players": []
}
```

文件名固定为：

```text
players-a.json
players-b.json
```

同一 `producerInstanceId` 选择最大 `sequence`。不同实例优先选择 `capturedAtUnix` 更新且 Session 匹配的快照。空数组只有在 `complete=true` 且快照新鲜时，才能证明当前分片无人在线。

### 9.2 世界状态 A/B 快照

`worldstate-a.json` 与 `worldstate-b.json` 在模拟未暂停时每 5 秒轮换一次；暂停期间由读取请求触发主动刷新。信封字段与玩家快照一致，并固定承载以下 17 项业务字段：

```text
season, phase, cycles
elapsedDaysInSeason, remainingDaysInSeason, seasonProgress
dayProgress, phaseProgress, precipitation, moonPhase
temperature, wetness, moisture, moistureCeil, precipitationRate
nightmarePhase, nightmareProgress
```

Lua 优先读取 `TheWorld.state`；季节进度、噩梦阶段和噩梦进度在对应字段缺失时，分别尝试 `seasonmanager` 与 `nightmareclock` 的只读方法。单项不存在时省略数值或返回空文本，不伪装为零。采集不遍历 `Ents`，只做有限字段读取、JSON 编码和一次异步持久化。

Go 只接受 Runtime `2.3.1`、协议 2、当前 Session 和当前运行时 Shard 身份的新鲜完整快照；拒绝未知 JSON 字段、尾随 JSON、NaN/Inf、负计数、超长文本与未来时间。A/B 中一槽损坏时读取另一槽；快照缺失或过期时先调用短刷新，短刷新不可用时才调用旧 nonce 控制台探针，保证旧 Runtime、特殊环境和代表性 Mod 仍有恢复路径。

### 9.3 事件批次

事件不要每条写一个文件。使用有上限的批次或双槽文件：

```json
{
  "schemaVersion": 1,
  "producerInstanceId": "runtime-random-id",
  "firstSequence": 101,
  "lastSequence": 108,
  "events": []
}
```

事件必须允许重复投递。Go 使用 `producerInstanceId + sequence` 去重，不能假设文件通知只发生一次。

Runtime 在停止或热升级时会取消尚未发布的内存批次并解绑监听器，避免旧实例在新实例启动后继续发布事件。已经开始的异步持久化无法由 DST API 取消，因此 Go 选择事件发生时间更新的 Runtime 实例，并只合并同一实例的 A/B 槽。

### 9.4 管理动作回执

请求由 Go 生成不可预测 ID，Lua 只接受固定动作及结构化参数：

```json
{
  "requestId": "uuid",
  "action": "player.resurrect",
  "arguments": { "userId": "KU_xxx" }
}
```

回执：

```json
{
  "requestId": "uuid",
  "ok": true,
  "code": "PLAYER_RESURRECTED",
  "message": "",
  "completedAtUnix": 1786500000
}
```

Lua 不能根据请求中的字符串动态调用任意全局函数。

命令采用至多一次发送语义：Go 在确认 Runtime 已安装、健康且命令模块空闲后只发送一次。发送后即使回执超时，也不能自动使用同一命令或旧控制台命令重试，因为动作可能已经在游戏内成功执行。只有在发送前确认 Runtime 未安装或未就绪时，玩家服务才允许使用旧内联控制台实现作为 fallback。

### 9.5 诊断报告

诊断请求由 Go 生成请求 ID，只允许以下三种固定档位：

| 档位 | 内容 | 硬限制 |
| --- | --- | --- |
| `summary` | 玩家数、实体数、天数、时段、季节和降雨 | 单次、只读 |
| `prefab` | 指定 Prefab 总数及有限 GUID/坐标样本 | Prefab 仅小写字母、数字和下划线；最多 50 个样本 |
| `performance` | 主模拟线程周期回调间隔摘要 | 1 至 5 秒；最多 50 个样本；自动停止 |

报告使用 `diagnostic-a.json` 与 `diagnostic-b.json`，携带 Session、Shard、Runtime 实例、sequence、完成时间和请求 ID。Go 不接受任意诊断名称、任意 Lua、无界实体输出或持续 profiling。

## 10. 安装器与用户文件保护

Go 安装器必须遵守：

- 只处理已经被 DST Admin 接管的房间。
- 对每个分片独立部署。
- 首次修改前创建带时间和哈希的备份。
- 使用唯一受管标记块，不覆盖用户其他内容。
- 标记块重复、嵌套或损坏时停止并报告，不能猜测性重写。
- 写入前执行 Lua 语法检查；没有外部 Lua 时至少做受管模板完整性检查。
- 使用同目录临时文件、`fsync` 和原子重命名发布。
- 符号链接、非普通文件、越界路径和过大文件一律拒绝。
- 创建房间、接管房间、导入存档、新增分片和启动前检查均执行幂等部署。
- 卸载时只移除自己的标记块和受管模块；用户文件其余内容原样保留。

建议标记：

```lua
-- DST-ADMIN MANAGED BLOCK BEGIN protocol=2
-- loader only
-- DST-ADMIN MANAGED BLOCK END
```

## 11. 与 Mod 的兼容策略

为了尽可能兼容所有 Mod：

- 不修改官方 `scripts.zip`，不要求安装 Workshop Mod。
- 不覆盖用户的 `customcommands.lua`。
- 所有新增全局收敛到 `_G.DSTAdmin`。
- 不修改官方 `AllPlayers`、`Ents`、`TheNet` 或组件方法。
- 对所有组件、字段和事件 payload 做存在性检查。
- 单个玩家或字段失败不应让整批快照失败。
- 采集回调使用 `pcall/xpcall`，错误限频写入健康记录。
- 不依赖 Mod 加载顺序和 Mod 环境专属的 `AddSimPostInit`。
- 不把未知组件或未知 Prefab 当成错误。
- 与目标 DST Build 做回归；游戏更新后重新验证关键 API。

不能承诺未来任意恶意 Mod 的数学意义 100% 兼容。合理的发布标准是：不侵入 Mod、字段级隔离、失败可见、保留 fallback，并持续把真实失败样本加入回归集。

## 12. 安全与风险分级

| 等级 | 类型 | 示例 | 默认策略 |
| --- | --- | --- | --- |
| L0 | 只读、小成本 | 玩家快照、健康信标、世界状态 | 可自动运行 |
| L1 | 只读、有成本 | 有限实体计数、短时诊断 | 用户触发、限频 |
| L2 | 可逆状态变化 | 公告、踢出、临时玩家效果 | 权限和审计 |
| L3 | 持久状态变化 | 封禁、生成/删除实体、修改世界 | 保护备份和确认 |
| L4 | 数据破坏风险 | 回档、重置、清空世界、强制崩溃 | 独立高危流程，不能进入通用命令 |

官方存在 `c_forcecrash()`、`c_emptyworld()`、`c_removeall()` 等调试命令，不代表管理系统应暴露它们。命令目录必须采用允许列表，而不是从 `consolecommands.lua` 自动把所有函数开放给用户。

## 13. Go 与 Lua 的职责边界

| 职责 | Go | Lua |
| --- | --- | --- |
| 用户鉴权和权限 | 是 | 否 |
| Job、审计和幂等 | 是 | 仅执行回执 |
| 分片生命周期和跨分片编排 | 是 | 当前分片动作 |
| 玩家实时组件读取 | 读取快照 | 是 |
| 原生日志事件 | 是 | 可补充 |
| 数据库存储和查询 | 是 | 否 |
| 备份、导入、Mod 和更新 | 是 | 否 |
| 世界内实体和组件操作 | 发起受控请求 | 是 |
| 完整 Session 地图解析 | Renderer | 否 |
| 系统 CPU、内存和磁盘 | Agent | 否 |

原则是“Go 决定是否做、在哪个分片做、如何审计；Lua 只在当前游戏世界读取或执行”。

## 14. 产品实施顺序

### 阶段 A：可靠运行时基础（代码已完成）

1. 定义加载块、模块目录和版本握手。
2. 实现分片级安装、备份、卸载和启动前检查。
3. 实现 `Start/Stop/Status/Reload` 及健康信标。
4. 已覆盖操作系统无关路径、安全文件操作和不同分片名称；真实 macOS/Linux DST 进程验收待部署时执行。

### 阶段 B：Telemetry V2（代码已完成）

1. 实现玩家字段级安全采集。
2. 实现 A/B JSON 快照和写入回调状态。
3. Go 实现文件校验、房间级多分片合并和数据新鲜度。
4. 保留原生日志与内联探针 fallback。

### 阶段 C：命令模块（代码已完成）

1. 把玩家操作迁移到版本化短函数。
2. 引入固定动作允许列表和结构化回执。
3. 完成动作的房间、分片、权限、确认和 Job 审计闭环。
4. 回执超时后禁止自动重试；仅在发送前确认 Runtime 不可用时启用旧控制台 fallback。

### 阶段 D：事件与诊断（代码已完成）

1. 增加稳定事件的小型批次协议。
2. 增加有限、按需、自动停止的诊断采样。
3. 世界状态页展示最近事件、历史诊断与结构化诊断结果。
4. 后续与地图和专门性能页面关联属于产品增强，不阻塞 Runtime 2.3.1 发布。

### 阶段 E：世界状态无日志采集（代码已完成）

1. 增加独立 `worldstate.lua`，每 5 秒发布完整 17 项 A/B 快照。
2. Go 严格校验版本、Session、Shard、实例、sequence、时间、字段边界和 JSON 完整性。
3. Runtime 快照正常时世界状态刷新不再向控制台发送 Lua，也不再产生周期性 `RemoteCommandInput`。
4. Runtime 缺失、旧版、损坏或过期时保留原 nonce 日志探针；上下文取消时禁止额外 fallback。

## 15. 验收清单

基础兼容：

- 用户已有空文件、有自定义函数和有注释的 `customcommands.lua` 均能无损安装。
- 地面、洞穴和自定义分片分别安装且互不覆盖。
- 文件不存在时可创建，损坏或含重复标记时明确失败。
- DST 无 Mod、多 Mod、服务器 Mod 和客户端必装 Mod 场景均能启动。

运行时：

- 初始加载时 `TheWorld` 不可用不会产生持续错误。
- 多次 `Start()` 不产生重复任务，多次 `Stop()` 不报错。
- 热升级失败后旧任务仍工作，成功后旧监听器全部解绑。
- DST 重启、回档、换 Session 后不会采用旧实例高序列快照。
- A/B 任一文件损坏时仍能读取另一份。

玩家与分片：

- Dedicated Server 虚拟宿主不会被当成真实玩家。
- 玩家从地面进入洞穴时只有一个在线记录，世界归属正确。
- 玩家断线、服务器停服和采集器故障是三个不同状态。
- 组件缺失只影响对应指标，旧有效值标记过期而不被清空。

性能与安全：

- 空服和满员情况下记录采集 P50/P95/max 耗时。
- 默认周期任务不遍历全部 `Ents`。
- 所有文件有大小和路径边界，所有字符串有长度限制。
- 任意 Lua、任意路径和官方危险调试命令不会通过普通 API 暴露。
- 高风险动作具有权限、确认、保护备份和审计证据。

当前自动化验收已覆盖安装器文件安全、Lua 语法与生命周期、命令允许列表、回执超时不重发、A/B 损坏回退、事件合并去重、诊断自动停止和 HTTP 契约。发布前仍需在可维护窗口完成真实 macOS/Linux DST、空服/满员和代表性 Mod 组合的分片重启验收；未执行该项前不得宣称生产环境兼容性已完全验证。

## 16. 最终定位

`customcommands.lua` 最有价值的不是增加更多控制台快捷命令，而是在不安装 Mod、不修改官方脚本的前提下，为 DST Admin 提供一个靠近游戏真实状态的、可降级的运行时桥梁。

推荐长期保留四类能力：

1. 玩家与世界的轻量实时观测。
2. 稳定游戏事件向管理端的结构化通知。
3. 已鉴权、已审计的短管理动作。
4. 用户主动触发、自动停止的诊断采样。

其他复杂能力继续留在 Go、Agent、Renderer 和 Catalog 中，避免让游戏主线程承担控制面的职责。
