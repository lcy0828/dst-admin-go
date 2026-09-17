# DST 开源产品矩阵架构

> 文档状态：实施基线 v1.0  
> 更新时间：2026-08-10  
> 适用仓库：`dst-admin-go`、`dst-admin-vue-v3`、Beacon Archive
> 目标：面向个人玩家、社区服主和商用自建服的本地优先 DST 管理与分析平台

## 1. 产品结论

项目不应继续只按“服务器面板”扩展，也不应把所有能力塞进一个进程。推荐形成一个以 DST Admin 为控制面、以版本化协议连接可选能力的产品矩阵：

```text
                         DST Admin Web
                              |
                       DST Admin API v2
                              |
        +---------------------+---------------------+
        |                     |                     |
  Local / Remote Agent   Map Renderer         Catalog Adapter
  进程、配置、备份、日志    Session 地图与快照     图鉴、关系、攻略数据
        |                     |                     |
        +-----------+---------+----------+----------+
                    |                    |
             World Analyzer       Performance Lab
             世界资源与风险分析      基准、Profiler、VM 回退
                    |                    |
                    +---------+----------+
                              |
                     Guide / Recommendation
                     可解释建议与运维决策
```

核心边界：

- `DST Admin` 始终可以独立完成启停、配置、备份、恢复、日志和更新。
- `dst-map-renderer` 始终可以在没有图鉴服务时生成基础地图。
- Catalog 只做只读增强；不可用、版本不匹配或数据过期时不能阻断管理操作。
- Performance Lab 是隔离实验能力；探针或 LuaJIT 失败时必须回退到已验证的原版 VM。
- 前端只访问 DST Admin API，避免浏览器直连多个服务造成 CORS、鉴权和地址泄露。
- 本机是默认运行目标；远程 Agent、远程 Catalog 和远程性能节点必须分别显式配置。

## 2. 面向用户的七个产品域

| 产品域 | 用户价值 | 当前基础 | 下一阶段 |
| --- | --- | --- | --- |
| 服务器控制台 | 建服、分片、启停、更新、备份、恢复 | 已具备正式 API v2 | 完善部署向导和商用审计 |
| 世界地图 | 查看地形、玩家、地标、资源和 MOD 实体 | Renderer v1 已实现 | 接入图鉴、资源分析和地图差异 |
| 图鉴与攻略 | 查询实体、配方、掉落、世界生成和资源循环 | Beacon Archive 已有数据闭环 | 作为可选 Catalog 接入管理端 |
| MOD 中心 | 安装、配置、更新、依赖和故障定位 | 管理端已有安装配置链路 | 合并 Catalog 变更与兼容证据 |
| 日志与诊断 | 定位崩溃、卡顿、玩家和 MOD 问题 | 结构化日志和规则已具备 | 关联 Build、MOD 和性能报告 |
| 性能实验室 | 找出 Tick、GC、保存和 MOD 热点 | 已有 LuaJIT 技术研究 | 先做可信基准，再做 VM A/B |
| 多节点运营 | 管理多机器、多 Cluster 和容量 | Agent 与运行目标已具备 | 增加舰队概览、SLO 和审计 |

这七个域应共享同一套房间、世界、Build、MOD 和 Job 标识，但不共享可变进程内状态。

## 3. Beacon Archive 中可利用的资产

截至当前实现状态，Beacon Archive 已生成：

| 资产 | 数量或能力 | 在矩阵中的用途 |
| --- | ---: | --- |
| Catalog 实体 | 8,311 | 通过 `prefab` 丰富地图实体和 MOD 详情 |
| 结构化事实 | 9,757 | 属性、数值、配方和攻略依据 |
| 实体关系 | 12,253 | 掉落、制作、烹饪、生命周期和世界生成关系 |
| 制作配方 | 949 | 建造计划与材料缺口分析 |
| 烹饪规则 | 397 | 料理计算和食材利用建议 |
| 公开图鉴图片 | 2,509 | 地图图例和实体详情图标 |
| 世界生成模型 | `WorldgenDistribution` | 地层、地皮、区域、权重和数量关系 |
| 资源生命周期 | `ResourceLifecycle` | 可再生、有限、一次性和条件再生判断 |
| 证据模型 | `evidence/confidence` | 区分静态推断、运行观测和派生建议 |
| 搜索能力 | Meilisearch + 确定性回退 | 中文、英文、Prefab、别名和拼音搜索 |
| P0 实验资产 | macOS/Linux 专服报告 | 平台兼容、容量和采集门禁 |

### 3.1 推荐复用层级

| 目录 | 建议 | 原因 |
| --- | --- | --- |
| `packages/contracts` | 提炼为 JSON Schema/OpenAPI | TypeScript 类型不能直接成为 Go 与第三方客户端契约 |
| `packages/catalog` | 发布内容哈希的只读 Release | 适合按 Build 查询，不应让管理端读取工作区文件 |
| `packages/domain` | 借鉴关系标签和确定性表达 | 可移植产品语义，不应跨仓库复制实现 |
| `packages/parser` | 保持独立构建链 | 解析失败不能进入服务器控制进程 |
| `packages/assets` | 仅消费发布后的映射清单 | 原始 TEX/XML 不进入管理端发布包 |
| `packages/database` | 借鉴不可变 Release 模型 | 管理端 SQLite 与知识库 PostgreSQL 不应合库 |
| `packages/search` | 通过 Catalog API 使用 | 管理端不直接依赖 Meilisearch |
| `p0` | 作为采集和实验事实源 | 不部署进日常控制面进程 |

### 3.2 当前禁止直接合并

`/Users/lcy/dst`、`dst-admin-go` 和 `dst-admin-vue-v3` 根目录当前都没有明确许可证文件。这是 GitHub 公开发布前的 P0 阻断项：

- 在来源和许可证确认前，不跨仓库复制代码。
- `dst-admin-go` 若继承了 GPL-3.0 项目代码，分发义务必须按实际代码来源审计，不能靠换仓库消除。
- Klei 图标和衍生 WebP 在公开分发前需要单独完成素材使用边界审核。
- 不公开官方 Lua 源码、原始 Session、用户存档、Token 或第三方 MOD 源码。
- 当前可以复用架构、字段思想、自有生成报告和经审计后允许发布的数据产物。

## 4. 核心数据连接

所有能力围绕五个稳定标识关联：

```text
runtimeTargetId -> roomId -> worldId -> sessionSha256
                         \-> gameBuildId
                         \-> modSetHash

features.json.prefab
  -> Catalog namespace + prefab/alias
  -> localized name + entity type + icon
  -> facts + relations
  -> WorldgenDistribution
  -> ResourceLifecycle
  -> evidence + confidence
```

### 4.1 为什么以 `prefab` 为主键连接

- Session 和 MOD 实体天然暴露 Prefab，映射成本最低。
- 未知 MOD Prefab 可以保留原值，不需要伪造图鉴数据。
- Catalog 可以维护别名、变体和 namespace，不污染 Renderer 协议。
- 地图生成不依赖 Catalog；Catalog 补全失败只影响展示信息。

不能只用中文名或图标名连接。它们会随本地化、皮肤、变体和 MOD 改动而变化。

### 4.2 四层事实必须分开

| 层级 | 示例 | 证据等级 |
| --- | --- | --- |
| 静态规则 | 桦栗树可由落叶林房间生成 | 官方脚本静态解析 |
| 当前世界 | 本 Session 中有 327 棵桦栗树 | Session 快照观测 |
| 运行趋势 | 过去 7 天减少 18%，再生速度偏低 | 多快照或运行采集 |
| 派生建议 | 建议保留某区域并补种资源 | 可解释规则输出 |

UI 必须标明这是“规则”“当前快照”“趋势”还是“建议”。静态权重不能伪装成真实出现概率，单次 Session 数量也不能伪装成长期资源趋势。

## 5. Catalog 接入方案

### 5.1 部署模式

Catalog 是可选只读集成，支持三种模式：

1. `disabled`：默认值，管理和地图完全独立运行。
2. `local`：显式配置本机 Catalog URL，适合完整自建。
3. `remote`：显式配置 HTTPS Catalog URL，适合使用公共知识服务。

不能因为发现到某个远程地址就自动启用。配置变更应做连通性、协议版本、Release 和 Build 握手，并明确显示数据来自本地还是远程。

### 5.2 建议协议

现有 Beacon API 已提供 Release、实体和搜索。接入前增加或固定机器可读能力文档：

```json
{
  "protocolVersion": "1",
  "service": "dst-catalog",
  "releaseId": "dst-740477-13002b3793f6",
  "gameBuildId": "740477",
  "locales": ["zh-CN", "en-US"],
  "capabilities": ["entities", "search", "relations", "assets", "worldgen"]
}
```

管理端建议新增以下只读代理接口，名称在实施时写入 OpenAPI：

```text
GET /api/v2/integrations/catalog/status
GET /api/v2/catalog/entities/{namespace}/{prefab}
GET /api/v2/catalog/search?q=...
GET /api/v2/maps/{mapId}/analysis
```

安全要求：

- Catalog URL 由管理员配置，不接受请求参数传入任意 URL。
- 远程模式只允许 `https`；本机模式仅允许 loopback 或 Unix socket。
- 禁止自动跟随到私网或非白名单重定向，防止 SSRF。
- Catalog Token 只保存在后端，永不发给浏览器和 Agent。
- 响应设置大小、时间、实体数和递归深度上限。
- 使用 `releaseId + prefab` 缓存；Release 变化后自然失效。
- Catalog 不可用时返回明确的 `enrichmentUnavailable`，基础地图仍返回成功。

### 5.3 Build 一致性

地图 Manifest 应继续记录 Session SHA 和 Renderer 版本。分析结果再额外记录：

```json
{
  "analysisProtocolVersion": "1",
  "mapId": "...",
  "sessionSha256": "...",
  "gameBuildId": "740477",
  "catalogReleaseId": "dst-740477-13002b3793f6",
  "catalogBuildMatch": true,
  "generatedAt": "2026-08-10T00:00:00Z"
}
```

Build 不一致时可以展示名称和图标，但数值、生成规则、资源缺口和攻略建议必须标为过期或停止生成，不能静默套用其他版本。

## 6. 世界分析能力

### 6.1 第一批应实现

1. **实体图鉴补全**：地图点击实体后显示本地化名称、类型、图标、关键事实和图鉴入口。
2. **资源分布概览**：按地表/洞穴、Prefab、区域和地皮统计实际数量与密度。
3. **资源生命周期**：区分可再生、需要施肥、仅移植后生长、有限和一次性资源。
4. **世界规则对照**：将实际实体与 `WorldgenDistribution` 对照，展示“可能分布区域”和“本图实际位置”。
5. **MOD 可见性**：未知 Prefab 始终可搜索；Catalog 能识别时再补全 MOD namespace 和版本。
6. **快照差异**：以 Session SHA 为版本比较资源新增、消失、迁移和世界状态变化。

### 6.2 可形成的高价值场景

- 桦栗树、石果灌木丛、芦苇、仙人掌、月树等资源的实际分布与地形关系。
- 基地周边一定半径内的食物、燃料、复活、虫洞和危险源覆盖。
- 地表与洞穴出口、远古区、月岛相关资源的跨层路线规划。
- 有限资源消耗预警，以及可再生资源的保护和补种建议。
- 世界生成后快速判断地图是否满足新手、生存、速通或商用长期服目标。
- 多次快照对比异常资源暴增，辅助发现 MOD 失控、刷物或存档污染。

### 6.3 暂时不承诺

- 从单次 Session 推断精确刷新概率。
- 对动态生成、事件、洞穴蠕虫等仅靠静态地图给出必然结论。
- 为未知 MOD 自动生成看似准确的中文说明。
- 在没有路径网格和碰撞证据时承诺最短可行路线。

## 7. 攻略与推荐引擎

攻略不应成为自由文本生成器，而应是带证据的决策视图：

```text
用户目标 + 当前世界快照 + Catalog 规则 + 服务器运行状态
                           |
                      确定性计算
                           |
               结论 + 原因 + 数据版本 + 风险
```

第一批模板：

| 模板 | 输入 | 输出 |
| --- | --- | --- |
| 建家评估 | 基地坐标、资源半径、地标 | 食物/燃料/交通/危险评分及明细 |
| 第一年前准备 | 世界天数、季节、库存和地图 | 缺失物资、可获取位置和制作路径 |
| 资源可持续性 | 数量、生命周期、多快照变化 | 有限资源和补种/施肥建议 |
| BOSS 准备 | 玩家数、角色、装备和配方 | 材料缺口与可制作链 |
| 洞穴探索 | 出入口、关键资源和地层 | 分段目标及风险点 |
| 商用服健康 | Tick、备份、磁盘、MOD 更新 | 运维优先级和回退建议 |

每条建议必须能展开查看引用的实体、关系、世界位置、Build 和证据置信度。没有证据时显示“不足以判断”，而不是补写推测。

## 8. MOD 智能中心

管理端已经知道“当前安装了什么、配置是什么、是否需要更新”，Beacon Archive 可以补充“这个 MOD 定义或改变了什么”。两者合并后形成：

```text
当前房间 modoverrides.lua
  + Workshop 元数据与依赖
  + Catalog MOD delta
  + 日志崩溃签名
  + Performance Profile
  = 房间级 MOD 健康报告
```

报告至少区分：

- `verified`：固定 Build、MOD 版本和配置经过真实运行验证。
- `smoke-tested`：只完成启动、保存和退出冒烟。
- `observed`：从用户实例观测到，但未在隔离环境复现。
- `static-only`：只有静态解析结果。
- `unknown`：没有足够证据。

不能把两个受控 Fixture 的成功外推成“兼容全部 Workshop”。加密 MOD、客户端 UI MOD 和动态下载代码需要单独边界。

## 9. Performance Lab 与 LuaJIT

`/Users/lcy/dst/p0/reports` 中的 LuaJIT 研究是路线依据。当前管理端已经具备按次启动选择 Lua 运行时的安全闭环：

- `serverMode` 只表示 32/64 位服务端架构，不再把 `luajit` 伪装成另一个可执行位数。
- 本机 Runtime 与新版 Agent 安装会只读检测 `DontStarveLuaJIT2` 的注入壳、原始程序、Injector、VM、配套 Mod、签名版本、Klei 游戏版本和二进制摘要。
- 检测状态固定为 `not_installed`、`detected_unverified`、`incompatible`、`ready`；只有 `ready` 可以进入后续启用流程。
- 固定签名包只有在签名版本与当前 Klei 游戏版本完全一致时才可启用；不一致时显示 `incompatible` 并阻止启用。
- `2026-08-22` 发布的 `Preview v3.0.0 (fa2b27c)` 改为插件化布局：游戏目录只保留注入 Stub，`data/unsafedata/ds_luajit_injector.path` 指向 Mod 根目录的真实 Injector，VM 位于 `plugins/plugin_core_vm`，依赖位于 `deps`。
- v3 由 `plugin_core_vm` 按当前 `version.txt` 使用 Function Relocation/Nucleus 生成签名。Linux 自动签名修复已合并到 DontStarveLuaJIT2 v3 的 `master`，Debian 12 完成构建、48 项测试、冷/热签名、存档载入和双分片基础验证。
- 稳定版 v3 直接使用上游的 VM 选项，不要求私有契约文件。安装前在目标节点检查系统依赖；官方包与 Debian 12 兼容构建在页面明确区分。
- Controller 对远程报告采用 fail-closed：`ready` 必须由 Agent 明确声明可启用，固定签名版本匹配当前游戏版本或包声明经过验证的自动签名能力，支持 Game Lua 和 LuaJIT，且不存在未知问题码。
- 启动选择是进程级参数，不写入 `cluster.ini`、`server.ini`、`modoverrides.lua`。同一次房间启动的全部所选世界使用同一模式；默认始终为原版 Game Lua。
- 只有本机或声明 `shard.runtime-mode.v1` 能力的 Agent，且目标安装报告存在共同支持的 LuaJIT 配置时，前端才允许选择 LuaJIT。实验性分代 GC 收在高级设置中，JIT 行为沿用上游配置；任何未验证或旧版节点都只能选择 Game Lua，不允许静默使用 LuaJIT。
- 启动弹窗单独展示运行时包版本：`V2.9.2 Preview` 作为旧版兼容路线可见但不声明支持进程级模式，`V3.0.0` 稳定版在检测通过的安装上可选。客户端提交所选版本，Controller 再次核对每个 Placement 的实际安装版本，禁止页面选择与实际启动版本不一致。
- 默认由 Agent / 本机 Runtime 管理包与安装，用户主动检查上游、下载或安装时才执行；不在后台自动更新，也不会注入运行中的世界。切换模式必须通过启动或重启创建新进程。

后续正确实施顺序是：

### P0：可信性能事实

- 固定 DST Build、专服哈希、MOD 集合与配置哈希。
- 采集 Tick P50/P95/P99/max，而不是只看进程 CPU 百分比。
- 区分主模拟线程 CPU、进程总 CPU、GC、保存和 Shard RPC。
- 同一工作负载至少三次运行，保存离散程度和正确性哈希。
- 建立无 MOD、代表性 MOD 包、实体压力和连续保存工作负载。

### P1：可交付的 VM 选择

- 对同一工作负载比较 Game Lua、LuaJIT 默认配置、LuaJIT 关闭 JIT 和实验性分代 GC。
- 策略绑定 `gameBuildId + modSetHash + configHash`。
- 未识别 Build 默认不注入；启动失败或正确性门禁失败自动回退。
- 管理端展示“当前 VM、为何选择、证据时间、回退入口”。

### P2：Profiler 和 MOD 归属

- 按 `../mods/<mod>/...lua` 聚合 CPU、分配、GC 和 trace abort。
- 输出文件、函数、行号和调用占比。
- 将日志尖峰、Tick 尖峰和保存事件放在同一时间轴。
- 只提出可解释建议，不自动删除实体、取消任务或改写 MOD 逻辑。

### P3：已验证优化

- Linux Fork Save 先做保存停顿、总时长和 Copy-on-Write 峰值实验。
- Concurrent GC 作为独立实验变体，不与上游分代 GC 混称。
- 显式 Worker API 只接受版本化纯数据，不传 `lua_State`、Entity、Lua table 或 userdata。
- 共享内存仅在测出重复数据成本后实施，禁止两个 Master 写同一 Shard。

产品对外可以承诺“测量、分析、选择和回退”，不能承诺“所有 MOD 都会加速”或“单个世界逻辑自动多核化”。

## 10. 商用自建服能力

商用用户需要的不是更花哨的首页，而是可追责和可恢复：

- 多节点/多 Cluster 总览，默认仍优先本机目标。
- 房间级操作权限、二次确认和审计日志。
- 备份成功率、最近可恢复点、恢复演练和磁盘余量 SLO。
- 更新前保护备份、MOD 变化 Diff、分批重启和失败回滚。
- 玩家封禁、公告、任务、日志和崩溃证据统一时间线。
- 资源/性能异常告警，但告警不得自动执行高风险修复。
- 导出不含 Token 和原始存档的脱敏支持包。

多租户计费、支付和托管平台不应进入当前开源核心。先把单组织自建和多节点管理做好，再通过公开 API 或插件边界扩展商业服务。

## 11. 部署组合

| 组合 | 进程 | 适用人群 |
| --- | --- | --- |
| Core | Admin API + Vue + Renderer | 普通个人服主 |
| Knowledge | Core + Catalog | 需要图鉴、地图分析和攻略 |
| Lab | Knowledge + Performance Probe | 高负载和 MOD 调优 |
| Fleet | 中央 Admin + 多 Agent + 可选 Catalog/Lab | 社区和商用自建 |

每个组合都必须有部署检查页。可选组件缺失显示 warning，只有专服路径、存档路径、权限和核心安全配置失败才阻断控制面就绪。

## 12. 分阶段实施

### M0：开源与协议门禁

- 审计三个仓库的来源和许可证，补齐根许可证、NOTICE 和第三方清单。
- 固定 Catalog capability、分析结果和性能报告 JSON Schema。
- 给所有产物增加 `protocolVersion`、Release、Build、平台、时间和内容哈希。
- 保持本地默认、远程显式配置和失败关闭的安全策略。

验收：仓库可合法公开；协议有兼容测试；没有工作区路径耦合。

### M1：地图图鉴增强

- 新增 Catalog 配置、握手、状态与受限 HTTP client。
- 提供 Prefab 批量查询，避免 19,000 个实体逐条请求。
- 地图点击详情展示名称、图标、类型、关键事实和证据。
- 未知 MOD、Catalog 离线和 Build 不匹配有完整降级状态。

验收：Catalog 断开后地图、启停、备份不受影响；相同 Release 结果可重复。

### M2：世界分析 v1

- 从 `features.json` 生成不可变 `analysis.json`。
- 完成资源数量、密度、地层、地皮、区域和生命周期统计。
- 增加地图资源筛选、聚合、详情和快照差异。
- 对真实 Session 建立固定回归样本和性能上限。

验收：每项分析能追溯到 Session SHA、Catalog Release 和 Prefab。

### M3：攻略与房间健康

- 实现建家、季节、资源可持续和商用服健康模板。
- 关联地图位置、配方依赖、日志和运行状态。
- 输出“不足以判断”和冲突证据，禁止无依据补全。

验收：建议可解释、可重复，Catalog/快照变化会使旧结果失效。

### M4：MOD 情报

- 合并安装状态、Workshop 版本、Catalog delta、日志和兼容等级。
- 建立房间 `modSetHash`，用于更新 Diff、性能报告和回退。
- 提供兼容报告导出和最小复现信息。

验收：任何兼容结论都带 Build、MOD 版本、配置和证据等级。

### M5：Performance Lab

- 先交付固定工作负载、指标协议和基准报告查看器。
- 再接入 GameLua/JIT-off/JIT-on A/B 与自动回退。
- Fork Save、Concurrent GC、Worker 和共享内存按实测收益逐项门禁。

验收：优化可关闭、可回退、不会破坏保存恢复，并有至少三次真实运行证据。

### M6：Fleet 与运营

- 多节点容量、备份 SLO、更新编排和审计时间线。
- 保持 Agent 动作白名单，不开放任意 shell。
- 为商用自建提供脱敏诊断包和恢复演练。

验收：单节点故障不影响其他节点，所有高风险动作可追溯。

## 13. 建议立即执行的下一批

按收益、依赖和风险排序：

1. **许可证与来源审计**：这是公开 GitHub 前不可绕过的阻断项。
2. **Catalog capability 与批量 Prefab 查询协议**：先稳定边界，再做 UI。
3. **地图图鉴增强**：已有 Renderer v1 和交互地图，最短路径形成用户可见价值。
4. **`analysis.json` v1**：只做资源统计、生命周期和世界生成对照，不先做自由文本攻略。
5. **地图快照差异**：把一次性查看升级成世界变化与异常分析。
6. **性能报告协议与基准页面**：先可观测，再讨论 LuaJIT 自动优化。

不建议下一批直接做 Concurrent GC、共享内存、实时语音、多租户计费或自动改 MOD。这些工作依赖尚未完成的基准、许可、协议和正确性门禁。

## 14. 总体验收标准

- 管理核心在所有可选组件离线时仍能安全运行。
- 本地目标默认启用；每个远程能力单独配置、单独鉴权、单独显示状态。
- 所有地图和分析结果可追溯到 Session SHA、Build、Renderer 和 Catalog Release。
- 未知 Prefab、Tile、MOD 字段和外部协议新增字段不会导致数据丢失。
- 静态事实、运行观测、趋势和建议在 API 与 UI 中明确区分。
- 性能结论包含 P50/P95/P99/max、重复次数、工作负载和正确性结果。
- 每项优化都有关闭开关、回退路径和故障注入测试。
- 开源发布具备清晰许可证、第三方清单、素材边界和隐私说明。
- 个人玩家可以只安装 Core；商用自建可以逐步扩展到 Knowledge、Lab 和 Fleet。
