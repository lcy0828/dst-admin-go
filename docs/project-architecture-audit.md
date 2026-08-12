# DST Admin 现存设计与实现改造审计

> 版本：1.2
> 审计日期：2026-08-12
> 范围：`dst-admin-go`、`dst-admin-vue` 当前 `feature/v2-rebuild` 工作树
> 关联文档：`docs/customcommands.md`、`docs/dst-platform-matrix.md`、前端 `docs/DST_ADMIN_FUNCTION_TRUTH.md`

## 1. 审计目标

本审计回答两个问题：

1. 哪些现存设计已经造成可靠性、功能闭环或使用体验问题，值得改造。
2. 哪些代码虽然不够新或不够统一，但改造收益不足，应当保持不动。

项目不以“技术栈更新”作为改造理由。一个改造项只有同时满足以下条件才进入实施计划：

- 能指出当前源码中的具体问题、功能断点或可复现风险。
- 能说明对用户或维护者的直接收益。
- 能定义可验证的完成标准。
- 能在保留现有存档、配置、API 和回退能力的前提下渐进落地。

尚未取得运行数据的性能猜测只进入测量清单，不直接触发重写。

## 2. 审计基线

本轮检查覆盖：

- Go 生产入口、路由组装、后台调度器、SQLite、Job/SSE、房间操作锁、日志、Mod、存档导入、公告和 OpenAPI。
- Vue 路由、API client、Vue Query 缓存刷新、功能页面、shadcn-vue 使用、测试和构建配置。
- 后端串行全量测试和 `go vet`，前端 OpenAPI 生成、lint、unit 和生产构建均通过。
- OpenAPI 重新生成结果与 `src/api/schema.d.ts` 一致。

这些结果说明当前代码具备可继续演进的基础，但“测试通过”不等于真实 macOS/Linux DST 与代表性 Mod 组合已经完成实机验收。

## 3. 当前架构与主要断点

```text
Vue 3 / Vue Query
  |-- typed query key factory ---------------------+
  |-- 日志 SSE                                    |
  `-- Job SSE -> terminal jobEffects -> 批量刷新   |
                                                    v
Gin Router
  |-- adminserver Bootstrap 组装依赖
  |-- Application 统一管理 scheduler/worker/close
  |-- v2 领域服务 -> SQLite / DST 文件 / tmux / SteamCMD
  `-- 单一生产入口与 signal 驱动的优雅停机

DST 运行时
  |-- 原生日志事件
  |-- customcommands.lua A/B Telemetry/事件/诊断/短动作
  `-- 发送前不可用时的旧控制台 fallback
```

已完成的横向改造：

1. HTTP、后台任务和数据库已经由 Application 管理生命周期。
2. SQLite 已启用 WAL、5 秒 busy timeout、foreign keys、保守连接池和迁移版本状态。
3. Job SSE 已具备水位、断线补偿、过期游标 reset、保留策略和声明式前端刷新。
4. Runtime 2.3.0 已取代默认持续长探针，并保留旧控制台 fallback。

当前主要剩余断点是后端已有但前端未交付的存档导入、世界/房间生命周期与 SMTP 测试，以及真实 DST 实机验收和日志事实源收敛。

## 4. 决策总表

| 优先级 | 改造项 | 直接收益 | 成本 | 结论 |
| --- | --- | --- | --- | --- |
| 已完成 | Application 生命周期与单一生产入口 | 可控启动、优雅停机、测试隔离，消除入口行为差异 | L | 保持回归 |
| 已完成 | SQLite bootstrap、WAL、busy timeout 和连接策略 | 降低 `database is locked` 与隐式初始化风险 | M | 保持回归 |
| 已完成 | Job 事件水位、保留策略和声明式缓存影响表 | 修复操作后必须刷新页面，避免全历史回放 | M | 保持回归 |
| P0 | 存档导入前端闭环 | 把已完成的安全导入能力交付给用户 | L | 必须改 |
| P1 | 世界创建/删除、房间删除、SMTP 测试前端闭环 | 补齐本地服完整生命周期和部署验证 | M | 值得改 |
| 已完成，待实机 | Runtime 2.3.0 与字段级新鲜度 | 保留完整指标且消除周期性长 Lua 命令日志 | L | macOS/Linux 验收 |
| P1 | v2 日志事实源收敛 | 避免双链重复采集和状态不一致 | M | 值得改 |
| P1 | 房间操作锁和 Mod 下载锁粒度调整 | 防止跨领域死等和大 Mod 下载阻塞所有读取 | M | 先测后改 |
| P1 | 能力清单与前端 CI | 防止文档和功能再次漂移 | M | 值得改 |
| P2 | 页面级 shadcn-vue 组件收敛 | 提高一致性、可访问性和表单维护性 | M | 随功能改造推进 |
| 已完成 | E2E 领域标签、setup 隔离与关键单元测试 | 缩短定位时间并支持选择性回归 | M | 保持回归 |
| 已完成 | ECharts 路由隔离与可测量包体预算 | 防止世界状态页与入口体积无界增长 | S-M | 持续监测 |
| 冻结 | 远程 Agent 新能力 | 当前不符合本地优先阶段目标 | - | 保留边界，不扩张 |
| 不做 | 微服务、重型消息队列、全量 GORM 迁移、Vue 重写 | 风险和成本高于当前收益 | XL | 明确不建议 |

成本含义：S 为一个小型提交，M 为一个功能批次，L 为需要跨模块迁移和回归的批次，XL 为不适合当前阶段的重构。

## 5. 必须改造

### 5.1 建立 Application 生命周期

> 落地状态：已完成。`internal/adminserver` 提供统一 Bootstrap/Run，生产入口走同一启动路径；Application 管理 scheduler、worker 和 finalizer，启动失败也执行最终清理。以下保留原问题和验收要求，作为回归依据。

#### 现状证据

- `routers/router.go` 的 `InitRouter()` 同时负责配置读取、数据库迁移、约数十个 Store/Service/Handler 的创建、legacy 迁移和路由注册。
- Agent watcher、封禁过期调度、自动化调度和备份调度在路由初始化期间使用 `context.Background()` 启动。
- `cmd/admin-api/main.go` 直接 `ListenAndServe()`，没有 signal 驱动的 `Shutdown()` 和后台任务统一停止过程。
- 根 `main.go` 仍启动旧日志监控、旧解析器和可选 Agent Server，两个可执行入口的生产行为不同。

#### 推荐结构

```text
Bootstrap(config, db)
  -> Application
       |-- HTTPHandler()
       |-- Start(ctx)
       `-- Close(ctx)

main
  -> signal.NotifyContext
  -> app.Start(ctx)
  -> http.Server.Serve
  -> server.Shutdown(deadline)
  -> app.Close(deadline)
```

路由层只注册 HTTP，不启动 goroutine。迁移仍可在 Bootstrap 阶段完成，但必须返回错误，不能在 package `init()` 中隐式退出进程。

#### 收益

- 停服时不再留下未完成写入或无法停止的定时任务。
- 单元测试可只创建需要的组件，不因 import 包而打开数据库或迁移旧表。
- 生产只有一个明确入口，legacy 功能可以隔离而不是与 v2 同时运行。

#### 迁移约束

- 第一阶段保留根入口，标记为 legacy，并通过等价启动测试确认发布脚本实际使用哪个入口。
- 不直接删除旧日志和 Agent 代码；先证明生产 v2 没有引用。
- 每个 scheduler 先补充幂等 `Start(ctx)`/`Stop()` 契约，再移动组装位置。

#### 验收标准

- `SIGTERM` 后 HTTP 停止接收新写请求，所有 scheduler 在截止时间内退出，数据库最后关闭。
- 连续调用两次 `Start` 不产生重复任务，连续调用 `Close` 不 panic。
- 生产构建和文档只指向一个入口；legacy 入口不会被默认构建或启动。

### 5.2 修正 SQLite 使用方式

> 落地状态：已完成。SQLite table prefix 在打开时固化，文件库启用 WAL、5 秒 busy timeout、foreign keys 和保守连接池；迁移版本与数据库运行参数已进入系统状态接口。仍需在生产规模数据上持续观察锁等待。

#### 现状证据

- `models/models.go` 在 package `init()` 中打开数据库并执行多组迁移。
- 普通文件数据库最大连接数设置为 100，未在统一 bootstrap 中显式配置 WAL、busy timeout 和 foreign keys。
- Job、日志、玩家、世界状态、自动化、备份、地图和 Mod 都可能并发写入同一 SQLite 文件。

#### 推荐方案

- 增加唯一的数据库打开函数，显式注入给 Application。
- 对文件数据库启用并校验 `journal_mode=WAL`、`busy_timeout`、`foreign_keys=ON`。
- 先把最大连接数收敛到保守范围，再以并发写压力测试决定最终值；不要沿用 100 作为默认值。
- 为 migration 建立顺序和版本记录，启动失败时返回可理解错误。
- `:memory:` 测试继续限制单连接，避免每个连接看到不同数据库。

#### 收益

- 显著降低多个 Job 同时完成时的锁错误。
- 数据库初始化变得可测试、可诊断，也为优雅停机提供明确所有权。

#### 验收标准

- 并行运行任务、日志刷新、玩家采样和自动备份的压力测试无 `database is locked`。
- 启动状态接口能报告数据库模式和迁移版本，但不暴露文件敏感信息。
- 迁移失败不会继续启动部分可用的 HTTP 服务。

### 5.3 重建 Job 事件到界面状态的刷新链

> 落地状态：已完成。新连接从当前水位开始，同标签页用 `sessionStorage` 游标断线补偿；保留窗口外返回 `job.cursor reset=true`。前端统一 query key 与 `jobEffects`，只在 Job 终态刷新业务数据，并在微任务批次内去重。默认保留 7 天/10,000 条事件和 90 天/5,000 个终态 Job，运行中任务不清理。

#### 原问题证据

- `useJobEvents.ts` 通过字符串前缀手工刷新少量 query key。
- 当前映射遗漏备份、配置/名单/Token、地图/Session、游戏版本、存档导入、命令历史和部分 Mod library 数据。
- 所有新 EventSource 都从 `/jobs/events` 的 ID 0 开始；后端会依次返回 `id > 0` 的历史事件。
- Job event 表持续追加，当前没有明确保留和清理策略。

这会同时造成两个用户可见问题：任务完成后页面仍显示旧数据；运行时间越长，新登录越容易回放大量历史并反复触发刷新。

#### 推荐方案

```text
typed query key factory
  + jobEffects(kind -> affected query keys)
  + mutation accepted -> 写入 pending/optimistic 状态
  + Job terminal -> 批量 invalidate
  + SSE reconnect -> 从最近 event ID 补偿
```

- 后端增加“当前事件水位”语义。新会话从当前水位订阅，只有同一浏览器断线重连才补历史。
- 前端持有最近处理的 event ID，并对同一批事件产生的 query keys 去重后刷新。
- 为 Job、target 和 event 建立按时间/数量的保留策略；仍在运行或用于审计窗口内的数据不得清理。
- 每个新增 Job kind 必须在同一提交中声明其 UI 影响；未知 kind 至少刷新 Job 本身并记录开发期警告。

#### 收益

- 系统性修复“点按钮后要手动刷新”，不再在每个页面打补丁。
- 长期运行时 SSE 连接成本保持有界。

#### 验收标准

- 对每一种 Job kind 都有测试证明终态会刷新正确资源。
- 使用含十万条历史事件的数据库建立新连接，不回放旧历史；断线期间事件仍能补齐。
- 多个目标事件在一个短窗口内只触发一次相同 query key 的失效。

### 5.4 交付后端已有的存档导入能力

#### 现状证据

后端和 OpenAPI 已实现 `/save-imports` 完整流程，包括上传、归档安全扫描、兼容报告、分析、应用、删除，以及新建/克隆/替换、Token/端口/Mod 策略和保护备份。当前 Vue 前端没有对应 API client、路由、页面或 E2E。

#### 推荐交互

```text
上传归档
  -> 安全扫描与结构识别
  -> 兼容性报告
  -> 选择：创建新房间 / 克隆 / 替换现有房间
  -> 冲突解决：目录、端口、Token、Mod
  -> 最终确认
  -> Job 进度
  -> 导入结果与保护备份入口
```

高风险的“替换”必须显示目标房间、运行状态、保护备份和恢复路径。不能把上传成功等同于可导入。

#### 收益

这是自建服迁移、灾难恢复和从其他面板迁入的核心能力，产品收益明显高于继续增加视觉组件。

#### 验收标准

- ZIP、TAR、TAR.GZ 的成功路径和恶意归档拒绝均有浏览器测试。
- macOS/Linux 来源存档、Master/Caves、单世界、自定义分片和缺失 Mod 分别展示明确兼容结论。
- 替换失败后原房间和保护备份可用，不留下半发布目录。

## 6. 值得改造

### 6.1 补齐本地服务器生命周期操作

后端已有房间删除、世界创建/删除和 SMTP 连接测试，前端当前没有调用入口。建议按以下顺序补齐：

1. 世界创建/删除：直接影响一个房间内 Master、Caves 和自定义分片的完整管理。
2. 房间删除：沿用后端已实现的 `.dst-admin-trash` 可恢复移动，前端必须展示恢复位置、运行中保护和精确确认；不要新增默认永久删除。
3. SMTP 测试：设置保存前后都可执行，并展示 DNS、连接、TLS 和认证失败阶段。

公告 CRUD 虽然也已存在，但当前只是数据库中的系统公告记录，不会向 DST 玩家广播，也不是系统事件通知。应先定义产品语义，再决定放入后台通知中心、游戏广播还是通知规则，不能只因为后端有接口就增加菜单。

### 6.2 实施 Telemetry V2，而不是恢复持续长探针

> 落地状态：Runtime 2.3.0 已完成自动化实现，包含 A/B Telemetry、固定允许列表短动作与结构化回执、世界事件、有界诊断、热加载清理、世界状态快照和旧控制台 fallback。真实 macOS/Linux DST、全部短动作及代表性 Mod 组合仍是发布前门禁。

`docs/customcommands.md` 已给出完整方案。推荐的数据链为：

```text
原生日志即时事件
  + 每分片 customcommands.lua A/B JSON 快照
  -> 房间级多分片合并
  -> 字段级持久化
  -> API 返回来源、observedAt 和 live/stale/unavailable
```

关键改造点：

- 身份、上下线等原生日志信息与网络/生存指标不能共享一个统一刷新时间。
- 新快照字段缺失时只把对应字段标为过期，不把旧值伪装成新值，也不清空仍有诊断价值的旧值。
- Go 是权限、任务、审计和持久化事实源；Lua 只读取当前游戏世界或执行短动作。
- 保留受控控制台探针作为安装失败、脚本不兼容和紧急诊断 fallback，但默认不持续写入长 `RemoteCommandInput`。

这项改造直接解决日志噪声与指标完整性的冲突，且保留全部 Mod 场景下的降级路径。

### 6.3 收敛日志事实源

生产 v2 已有 `internal/logstream -> internal/structuredlogs`，而 legacy 根入口仍启动 `service/logmonitor`、`service/logparser` 和旧数据库日志模型。

推荐：

- v2 生产只以 `logstream + structuredlogs` 为当前事实源。
- legacy 链先变为显式的迁移/导入工具，确认发布脚本和路由无调用后再移除。
- Telemetry 从受管快照读取；结构化日志保留原生日志分析职责，不承担所有实时状态。
- 日志规则提供内置规则版本、用户覆盖层、导入导出和恢复默认，避免升级时规则数量意外缩减。

验收时应对同一日志文件证明只有一个采集游标和一套可见统计，不产生重复记录。

### 6.4 统一房间破坏性操作协调器

现有 `internal/roomops` 是正确边界，但 backups、mods、players 等领域仍存在自己的房间锁。建议定义清楚：

- 修改存档、配置、分片生命周期的破坏性操作统一经过 `roomops`。
- 领域内部 mutex 只保护该服务的内存状态，不包围外部命令和长文件 I/O。
- `Acquire` 使用请求或 Job context，取消请求后不得无限等待。
- 锁顺序写入包文档，并增加跨备份/Mod/配置/存档导入的并发测试。

不能为了减少锁而牺牲存档一致性。锁属于必要基础设施，目标是收敛所有权和缩短范围。

### 6.5 缩小 Mod 全局锁范围

当前 library 写锁覆盖 SteamCMD 下载、验证和回滚全过程。大 Mod 下载期间可能阻塞其他房间的 Mod library 读取。

推荐先增加基准和并发测试，再执行：

- 按 Workshop ID 做单航班锁，同一 Mod 只下载一次，不同 Mod 可受控并发。
- 下载到 staging 和校验期间不持有全局 library 写锁。
- 原子发布缓存目录、更新 managed setup 清单时短暂持锁。
- 房间启用与配置仍通过 `roomops` 串行。

如果测量证明当前用户规模下没有可见阻塞，可把实现降为 P2，避免为理论并发增加复杂度。

### 6.6 建立可验证的 Capability Manifest

> 落地状态：已完成。前端仓库 `docs/capability-manifest.yaml` 当前登记 25 个产品能力域，并显式区分 `closed`、`backend-only`、`experimental` 和 `frozen`。校验器使用 TypeScript AST 检查生成契约中的 operationId、API client 方法、Vue 路由和前端测试；相邻后端仓库存在时继续核对 Go 测试函数。CI 已将该校验放在 lint、unit 和 build 之前，并有负向测试证明伪造 operationId、API、路由、测试或 backend-only 前端入口会失败。

当前功能事实表曾出现“后端无公告/存档导入未记录”等漂移。机器可读清单按以下关系表达：

```text
capability
  -> backend route
  -> OpenAPI operationId
  -> frontend client method
  -> route/view
  -> unit/E2E evidence
  -> release status: closed | backend-only | experimental | frozen
```

CI 检查：

- 后端 `go test -race ./...`、`go vet ./...` 和构建。
- 前端 lint、unit、build 和按领域选择的 E2E。
- 重新生成 OpenAPI 类型后工作树无差异。
- 标记为 `closed` 的能力必须具有前后端引用和验收证据。

清单不能仅按字符串猜测路由，允许少量显式映射，以减少误报。

### 6.7 UI 按功能页面渐进收敛

当前 Vue 3 + Vite + shadcn-vue `new-york` 基线无需重写。剩余问题主要是原始 `label/select/checkbox/file input`、手工空状态/错误状态、直接使用 `reka-ui` primitive 和不一致的语义色。

推荐在每个功能闭环批次内顺手改造对应页面：

- 表单统一 `Field`/`FieldGroup`、Label、Select、Checkbox 和可访问错误关联。
- Dialog、Sheet 和侧栏按任务复杂度选择，不机械地全部居中或全部右侧弹出。
- 危险操作、进行中、成功、过期和不可用使用统一语义 token。
- 共享 Loading、Empty、Error、Confirm 和 Job accepted 状态，避免页面各自实现。

不再单独发起一轮“全站视觉重写”。功能可用性、真实数据和响应反馈优先。

### 6.8 调整测试结构与前端性能门槛

> 落地状态：Playwright 已使用 setup project 创建管理员和种子房间，共享有状态测试后端固定为单 worker；12 条用例按 `auth`、`rooms`、`mods`、`players`、`logs`、`world-state`、`automation`、`agents` 和 `system` 标签可独立回归。玩家、日志等用例自行产生前置事件，不再依赖用例执行顺序。

前端当前有 48 条单元测试。生产构建对应用入口、全局 CSS、世界状态路由和 Select 共享块同时检查原始与 gzip 体积，包含正向与负向门禁测试。预算超限或目标产物消失时构建失败，不以删除功能换取体积。

### 6.9 让界面设置成为真实运行时配置

> 落地状态：系统名称、时区、日期格式和主题色已经闭环。认证 Session 公开这四项非敏感偏好，Vue 启动、登录和设置保存后统一应用；系统名称同步登录页、侧栏和文档标题，日期显示统一使用配置时区和格式，主色按相对亮度选择可读前景色。

仍应保持以下边界：

- `ui.language` 在完整语言资源和 fallback 策略完成前只读，不能只翻译类型或少数页面就宣称支持多语言。
- `ui.adminEmail` 在出现明确的通知、支持或账号恢复消费者前不开放编辑。
- `backup.*` 全局默认值与 `notification.*` 事件开关必须由真实调度器和投递链消费后才能开放。
- Session 只返回展示所需的非敏感偏好，不返回 SMTP、路径、安全策略或其他系统配置。

## 7. 冻结和暂不改造

### 7.1 远程节点

当前产品阶段明确本地优先，远程节点单独配置且暂缓。保留 `RuntimeTarget` 安全边界和已有 Agent 代码，因为它能防止远程请求错误回落到本机；但：

- 不新增远程领域操作。
- 默认导航不突出远程入口。
- 不让本地房间操作依赖 Agent 在线。
- 文档和 API 将 remote 标为 `experimental/frozen`。

### 7.2 暂不更换的技术

- 保留 Gin：现有 API、middleware 和测试均建立在其上，替换无产品收益。
- 保留 SQLite：单机本地优先与 SQLite 匹配，应先修正连接和事务方式。
- 保留 Vue 3：现有页面、类型和 shadcn-vue 基线可渐进修复。
- 保留 Go 主解析 + 外部 Lua fallback：这是 Mod 兼容性的必要双路径。
- 保留 tmux 控制边界：在没有更可靠且跨 Linux/macOS 的替代证据前不改。

## 8. 明确不建议的重构

- 不把单体拆成微服务。
- 不引入 Kafka、RabbitMQ 等重型消息队列；Job 与 SSE 的规模不需要它们。
- 不为了统一 ORM 而全量迁移 GORM v2。当前主链仍广泛使用 jinzhu/gorm；只有 legacy `routers/mod` 使用 `gorm.io/gorm`，应在隔离 legacy 后单独评估移除依赖。
- 不从零重写前端或后端。
- 不删除 Lua fallback，也不把 customcommands 做成侵入式服务端 Mod。
- 不把公告 CRUD 直接包装成“游戏公告”或“邮件通知”，因为当前没有对应发送语义。

## 9. 分批实施与提交边界

每一批按功能独立提交，不把 UI、数据库和无关格式化混入同一提交。

### 批次 1：运行可靠性

1. `refactor(app): introduce managed application lifecycle`
2. `fix(storage): configure sqlite concurrency and explicit bootstrap`
3. `fix(jobs): bound event replay and retention`
4. `fix(web): centralize job effects and query keys`

完成门槛：优雅停机、SQLite 并发测试、SSE 水位/重连测试和所有 Job kind 刷新映射通过。

### 批次 2：本地核心功能闭环

1. `feat(imports): add save import analysis and apply workflow`
2. `feat(worlds): add create and recoverable delete controls`
3. `feat(rooms): expose recoverable room deletion workflow`
4. `feat(settings): add smtp connection test`
5. `docs(capabilities): correct feature truth and product semantics`

完成门槛：真实数据、失败恢复、Job 终态刷新和关键 E2E 齐全。

### 批次 3：Telemetry V2

1. `feat(runtime): manage versioned customcommands block`
2. `feat(telemetry): publish atomic shard snapshots`
3. `feat(players): merge shard observations with field freshness`
4. `feat(console): retain audited telemetry fallback`

完成门槛以 `docs/customcommands.md` 第 15 节为准。

### 批次 4：日志与 Mod

1. `refactor(logs): isolate legacy ingestion from v2 runtime`
2. `feat(logs): version builtin rules and user overrides`
3. `perf(mods): narrow library publication locks`

完成门槛：单一日志事实源、规则升级不丢失、不同 Mod 并发下载和同一 Mod 单航班测试通过。

### 批次 5：发布质量

1. `ci(web): verify lint unit build e2e and generated schema`
2. `test(web): split e2e by capability`
3. `docs(capabilities): add verifiable capability manifest`
4. `refactor(ui): normalize shared interaction states by page`
5. `perf(web): enforce measured bundle budgets`

## 10. 验收总则

每项改造合并前必须满足：

- 不改变未列入该批次的 API 行为。
- 不覆盖未知 Lua 字段、用户 `customcommands.lua` 内容或已有 Mod 配置。
- 破坏性文件操作具备保护备份、原子发布和失败恢复。
- 后台任务可取消或有明确不可取消阶段，状态能通过 Job/SSE 到达界面。
- macOS 与 Linux 的路径、可执行文件和大小写差异进入测试矩阵。
- 文档、OpenAPI、前端 client、页面和测试对功能状态的描述一致。

## 11. 最终判断

项目无需从零重造。现有领域模块、OpenAPI、安全边界、Job、备份、Mod 双解析和 Vue 3 页面都值得保留。真正需要重建的是跨领域的“运行和反馈骨架”：统一进程生命周期、正确使用 SQLite、让 Job 结果可靠地传播到页面，并把已经存在的后端能力做成真实前端闭环。

批次 1、本地存档导入与房间/世界生命周期、SMTP 分阶段诊断，以及 Runtime 2.3.0 玩家/事件/诊断/世界状态链路都已实现；Runtime 继续保留受审计的控制台 fallback，Capability Manifest、领域标签 E2E 和可测量 bundle 门禁也已进入 CI。当前下一阶段应补真实 macOS/Linux DST、代表性 Mod、存档导入和 SMTP 提供商验收；日志与 Mod 锁优化仍必须先测量，视觉收敛必须跟随真实功能页面。
