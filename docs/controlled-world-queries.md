# 受控只读游戏查询与 AstrBot 接入

此功能提供固定白名单查询，适合由 AstrBot 按工具名称向普通用户开放。后端不接受 Lua、脚本片段、函数名或自定义命令 ID。管理员原有控制台和通用命令执行功能继续保留。

## 开放哪些工具

普通用户可开放：

- `api_queryRoomWorld`：查询世界信息、指定分片在线玩家或实体数量。
- `api_getRoomWorldQueryRun`：读取查询结果，必要时补读原请求的迟到回执。

需要自行选择目标时，可另外开放经过确认的房间、世界列表读取工具，或在机器人中预设目标。房间和世界 ID 使用列表返回的标识，不能填写存档目录。

不要同时向普通用户开放 `call_api`、`api_executeRoomCommand`、`api_executeRawRoomCommand`、`world_console` 或自定义命令编辑工具。通用调度工具能间接调用其他管理接口。

接入密钥保存在机器人服务端。现有接入密钥仍代表关联的管理员身份；`access=read`、目录筛选和 MCP 注解不缩小密钥本身的权限。该功能通过固定后端实现和 AstrBot 的工具白名单开放查询，不创建新的审批流程。机器人共用一把密钥时，面板识别的是同一接入身份，不能区分机器人内部聊天用户。

## 查询类型

| queryId | 参数 | 内容 |
| --- | --- | --- |
| `world_info` | 无 | 天数计数 `cycles`、季节、昼夜、温度、降雨和降雪；缺少的字段为 null |
| `online_players` | 无 | 能确认位于指定分片的在线玩家：游戏用户 ID、名称、角色 Prefab |
| `entity_count` | 必需 `prefab` | 指定分片当前注册实体中，Prefab 精确匹配的数量 |

`cycles` 是游戏原始计数，显示第几天时通常使用 `cycles + 1`。在线玩家查询排除虚拟服务器 Host、去重，只返回有分片归属证据的玩家；尚未生成本地角色的连接不一定包含在结果中，不适合作为连接总数或选角人数。它不返回 Steam ID、IP、玩家坐标、历史记录或管理员配置。

`prefab` 长度为 1–80，只允许 ASCII 字母、数字与下划线，区分大小写。符合规则的模组 Prefab 也可查询，不要求先收录进资源词库。数量包含已注册但休眠的实体，不包含存档中尚未实例化的对象。查询不会加载模组、调用 Prefab 工厂或生成实体。不同分片分别查询，结果不是跨分片同一时刻的原子快照。

未知查询、缺失参数、额外字段、重复 HTTP 参数及请求体都会被拒绝。`readOnly`、`risk`、`category`、`isBuiltin` 等字段不能改变权限；修改或创建同名自定义命令不会改变查询实现。

## 输入示例

下面所有 ID、数值均为虚构示例。使用工具列表或 `find_api` 返回的 Schema，传入 `path` / `query` 对象：

```json
{
  "path": {
    "roomId": "RXhhbXBsZQ",
    "worldId": "TWFzdGVy",
    "queryId": "entity_count"
  },
  "query": {
    "prefab": "spiderden"
  }
}
```

查询世界信息：

```json
{
  "path": {
    "roomId": "RXhhbXBsZQ",
    "worldId": "TWFzdGVy",
    "queryId": "world_info"
  }
}
```

对应 REST API：

```text
GET /api/v2/rooms/{roomId}/worlds/{worldId}/queries/{queryId}?prefab=spiderden
GET /api/v2/rooms/{roomId}/worlds/{worldId}/query-runs/{runId}?refresh=true
```

接口继续使用现有会话或 Bearer 认证、来源/IP 策略和身份可访问范围。查询目标由房间、世界实际 Placement 决定，本机和 Agent 使用相同结果结构；远程节点离线时不会改到控制端执行。HEAD、OPTIONS 不执行查询。

## 如何解释输出

HTTP/MCP 外层成功只表示获得了业务结果。必须检查业务 `status`、`complete`、`source` 和 `observedAt`。例如下面是业务 data 的一部分：

```json
{
  "queryId": "entity_count",
  "status": "succeeded",
  "complete": true,
  "source": "runtime",
  "cached": false,
  "transportOutcome": "sent",
  "executionOutcome": "confirmed",
  "resultState": "output",
  "data": { "prefab": "spiderden", "count": 42 },
  "error": null
}
```

| status | 解释 |
| --- | --- |
| `succeeded` | 当前实例的有效快照，或有匹配执行回执且输出完整 |
| `partial` | 扫描或输出达到预算；结果不完整 |
| `failed` | 收到明确的 Runtime 执行失败回执 |
| `timed_out` | 投递前已超时，未执行 |
| `uncertain` | 可能已发送或执行，缺少可信的完成证据 |
| `unavailable` | 世界停止、节点离线、Runtime 未就绪，或输出无法使用 |

只有完整确认并明确返回 `count: 0` 才能说数量为零。`sent` 不是成功，空输出不是零。实体扫描不完整时返回 `count: null`、`lowerBound` 和原因，例如至少找到 12 个；不能把下界当作总数。

快照查询 `source=snapshot`，执行字段为 `not_required`；不会伪造 Lua 执行回执。最多使用 10 秒内、符合当前进程身份的快照。短缓存明确返回 `cached=true`，保留原观察时间。查询不自动启动世界、安装或修复 Runtime。

## 结果不确定时

使用返回的 `queryRunId` 调用 `api_getRoomWorldQueryRun`：

```json
{
  "path": {
    "roomId": "RXhhbXBsZQ",
    "worldId": "TWFzdGVy",
    "runId": "00000000-0000-4000-8000-000000000001"
  },
  "query": { "refresh": true }
}
```

默认只读已保存的结果。`refresh=true` 在原请求提交后的两分钟内，进行一次最多四秒的原回执补读。它不会重发 Lua，也不会执行恢复探针。`receiptCheck` 指出 pending、found、not_found、unavailable 或 expired。

原查询未确认期间，同一分片的新执行会合并或被拒绝，更换 Prefab 不能绕过。该记录在控制端重启后仍保留。补读窗口结束后，旧查询保持 uncertain；用户之后主动发起的是一次新观测。不要配置针对 uncertain 的自动重试。

查询记录绑定最初的接入身份、房间和世界；此接口不能读取其他身份的查询或管理员的通用命令记录。日志中的无关 print 不会拼入结果。

## 开销与限制

- 查询执行全局最多 2 个，每分片 1 个；底层执行通道忙时立即返回，不建立长队列。管理员与查询仍共享底层分片通道，查询等待回执也有时间上限。
- 每接入身份按每秒 1 个令牌补充，突发最多 5 个；约每分钟 60 次，命中缓存也计入限制。HTTP 429 返回 `Retry-After`。
- 每分片实体扫描至少间隔 5 秒。相同身份、实例和参数的并发查询合并执行。
- 缓存保留 3 秒，最多 128 项、2 MiB；重启或迁移后不沿用旧实例缓存。
- 单次请求最多等待 8 秒。实体扫描最多 100000 项，每 128 项检查 5ms CPU 预算。CPU 预算是协作检查，不能强制中止 Lua，可能超过一个检查批次的时间。
- 在线玩家最多返回 64 条；业务输出最多 8 KiB，查询记录投影最多 16 KiB。
- 查询运行记录最多 2000 条、保留最多 7 天，新增记录时清理；管理员命令历史不受此清理影响。

不增加闲时轮询、后台实体扫描、常驻玩家 Hook 或独立本机 Agent。

## 验证

验证覆盖正常查询、本机与 Placement 适配、未知查询、参数注入、伪造标记、同名自定义命令、资源限制、空输出、部分计数、错误身份以及迟到回执不重发。可运行：

```bash
go test -race ./internal/worldquery ./internal/dstruntime ./internal/httpapi ./internal/apicontract ./internal/aicontrol ./internal/runtimedriver ./routers
go run ./cmd/openapi-manifest -check
go test ./internal/dstruntime -run '^$' -bench '^BenchmarkWorldQueryEntityScan$' -benchtime=200ms -cpu=2
```

验证结果：上述 7 个 Go 包的完整 race 回归、相关包及 `cmd/admin-api` 的 `go vet`、OpenAPI 映射校验均通过；前端 OpenAPI 客户端的 2 项测试通过，前后端生成映射一致。测试没有连接或重启生产世界。

合成扫描基准使用 Gopher Lua、Darwin arm64 / Apple M4 Pro、Go `-cpu=2`：一次运行中 1 万实体完整扫描约 2.81ms；5 万、10 万及超限集合约 5.10–5.12ms 返回部分结果。确定性测试另外验证 100000 项硬上限和 CPU 预算中断。`-cpu=2` 仅限制 Go 调度并行度，不等于 2C4G 机器或实际 DST LuaJIT 性能；这些数据用于验证预算行为，不作为实际游戏帧耗时保证。
