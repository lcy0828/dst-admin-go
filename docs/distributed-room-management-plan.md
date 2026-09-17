# 多节点集中管理执行计划

> 状态：Phase 1-9、受管远程配置投放和 cold/hot-consistent 备份已完成；Kubernetes 保持默认关闭的只读实验能力
> 更新日期：2026-08-19
> 范围：多台服务器集中管理、一个房间跨节点运行多个世界分片、主服务/Agent/DST Runtime 独立部署、集中操作与可观测性

当前进度：Phase 1-9 的既定可交付范围已完成。Placement apply 已具备停服预检、跨文件系统传输、目标校验、原子切换、源恢复点和 `appliedTargetId` 提交；首次把本机受管房间规划到一个或多个 Agent 时，可通过固定配置白名单、分块校验、原子发布和持久恢复操作投放 Cluster/Shard 配置，已有远程分片换节点仍必须走 migration。仓库已交付非 root 控制面/Agent/DST Runtime OCI 镜像、Compose、Linux systemd 与 macOS launchd Agent、native/container Runtime Driver、cold-consistent 与带可验证同 snapshot 屏障证据的 hot-consistent 分布式备份、Placement-aware 跨节点 Mod 原子发布与自动重启/加载确认，以及带保护备份、版本矩阵和失败恢复的 DST 多节点版本发布。端口租约、网络作用域、跨节点 Master 端点预检、Linux cgroup v2 与 Docker CPU policy 执行/回读、玩家/日志/诊断聚合均已接入运行链路。2026-08-15 至 2026-08-16 已在全新 Debian 12 上完成容器控制面、裸机 Agent、容器 Agent、分片迁移、SteamCMD 下载、保护备份、110 MB Mod 分块发布以及 Mod 禁用/启用/配置/移除和 Placement 回读实测。Kubernetes 仍只有默认关闭的 status/observe/preflight API 与 UI，没有 Apply、生命周期、Console、Mod 或备份恢复能力，不能视为生产 Driver。

## 1. 目标

DST Admin 需要从“管理当前机器上的 DST”扩展为“本地优先、可选多节点”的集中控制系统：

- 主服务裸机部署时默认继续管理控制器所在本机，不要求部署 Agent。
- 主服务容器化时默认不获取宿主控制权限；管理同宿主或远程裸机 DST 时使用 Agent。
- 用户拥有多台服务器时，可以在每台机器安装 Agent 并接入同一个控制中心。
- 一个节点可以运行多个房间或多个世界分片。
- 一个房间可以把不同世界分片放置在不同节点，以分摊 CPU、内存和磁盘负载。
- 主服务部署、Agent 部署和 DST Runtime 三个维度独立组合；任何一层容器化都不自动推导另外两层也容器化。
- 同一领域模型支持裸机和容器 Runtime，并为未来 Kubernetes 预留明确 Driver；容器化不减少房间、Mod、日志、备份或控制能力。
- 用户可以控制单个节点、单个分片、整个房间或一组房间。
- 玩家、日志、世界状态、Mod、备份和操作结果在控制中心聚合展示。
- 网络分区、节点离线和部分失败时，系统必须避免双启动、错误恢复和静默数据损坏。

## 2. 当前基线

当前代码已提供：

- WebSocket 连接、注册、心跳、节点资源和受信 RuntimeInstallation 清单。
- 房间/世界/进程清单、CPU 容量与数据新鲜度。
- Room/Shard Placement 期望位置、拓扑 revision 和冲突诊断。
- `shard.status/start/stop/restart/save` 类型化远程操作。
- 房间租约、fencing token、幂等结果、操作审计和 Agent 断线语义。
- 单房间和跨房间的启动容量预检、批量 Job、逐 Shard 结果和恢复入口。
- `master_port`、`server_port`、`authentication_port`、`master_server_port` 采集。
- 按网络作用域持久化端口租约，并在导入失败、取消、恢复和过期计划时释放或回收。
- 跨节点 `bind_ip/master_ip`、地址新鲜度和 Master 地址/端口一致性预检。
- Linux native cgroup v2 与 Docker `reserved/exclusive` CPU policy 执行、回读和生命周期释放；macOS 对不支持策略明确拒绝。
- 按 Placement 聚合玩家、日志、世界状态与 Runtime 诊断，并携带来源、新鲜度、stale 和冲突信息。
- 跨节点 Mod 发布后的协调重启与加载日志确认，以及 DST 二进制多节点版本发布、保护备份和原地恢复/重试。
- 首次远程房间配置投放：只读取受管房间固定白名单文件，按计划 Placement 写入受信安装根，逐分片校验 SHA，并在整体提交前失败时回滚。
- `hot-consistent` 分布式备份：Runtime 2.4.0 在所有运行分片准备屏障，由 Master 触发一次 `ms_save`，只有每个分片返回相同 snapshot、相同运行实例身份和 `save_current_callback` 证据后才进入 staging。

当前不足：

- Kubernetes 只提供默认关闭的只读 Provider 状态、资源观察、类型化预检计划、安全内核、RBAC 和 UI；没有 Apply 路由、生产工作负载、lease-aware supervisor、Console、Mod、备份恢复或故障注入结论。
- Podman、macOS 容器以及 Kubernetes/CSI/CNI 生产矩阵仍未实机验收，不能从 Docker/Debian 结果外推兼容性。

## 3. 资料核验原则

任何涉及 DST 跨节点行为的设计，必须先查阅资料或完成实机验证。

来源优先级：

1. Klei 官方 Dedicated Server 文档、官方论坛和官方游戏脚本。
2. Steam 官方 Dedicated Server、SteamCMD 和 Workshop 文档。
3. 可复现的开源工具实现。
4. 社区教程只做交叉验证，不能作为唯一依据。

每项结论记录：

- 结论内容。
- 来源 URL 和访问日期。
- 适用的平台与游戏版本。
- 官方说明、代码推断或实机观察。
- 可信度和仍需验证的问题。

重点核验：

- Cluster、Master Shard、Secondary Shard 的配置和唯一性。
- `cluster.ini`、`server.ini`、Shard ID、连接密钥、地址和端口。
- 多机器部署的网络要求与 Master 失联行为。
- 分片启动顺序、注册、重连、停止和异常退出语义。
- 玩家跨分片迁移时的状态与存档归属。
- 保存、关服、回档和备份对其他分片的影响范围。
- Cluster 公共文件与各 Shard 私有文件的边界。
- Mod 配置、Workshop 文件和 UGC 缓存在每个节点的要求。
- macOS/Linux 混合节点的 DST、Mod 与存档兼容性。

无法通过资料确定的内容必须进入实机测试矩阵，不能凭经验补全。

## 4. 领域模型

目标层级：

```text
Control Plane（控制中心）
  deployment = native | container | kubernetes
  -> Runtime Provider（local/agent/kubernetes）
      deployment = in_process | native | container | daemonset
      -> Node（物理机、虚拟机或 Kubernetes Worker）
          -> Execution Environment（native/container/kubernetes）
              -> Runtime Installation（DST 安装实例）
                  -> Shard Placement（逻辑世界的生效位置）
                      -> Process Instance（进程/容器/Pod 实例）
```

Room / Cluster 与 Shard / World 是控制面的逻辑对象，不从属于某一 Node；Placement 把 Shard 映射到具体执行环境。

三个 `deployment` 维度分别表示主服务、Agent/Provider 和 DST Runtime，不能合并成一个 `containerized` 布尔值。

### 4.1 核心对象

| 对象 | 职责 |
| --- | --- |
| Control Plane | 保存期望拓扑、编排操作、聚合状态、审计和授权 |
| Runtime Provider | 本机、Agent 或 Kubernetes 集群的连接、凭证和能力边界 |
| Node | 一台运行机器或 Kubernetes Worker，作为容量和故障域 |
| Execution Environment | 稳定的逻辑执行边界，保存网络/CPU/挂载期望和当前实际 Node |
| Runtime Installation | 执行环境中的 DST 二进制、SteamCMD、Workshop、UGC 和路径集合 |
| Room | 一个 DST Cluster，包含共享配置、Token、名单和多个分片 |
| Shard | Master、Caves 或自定义世界，拥有独立目录和运行配置 |
| Placement | 指定 Shard 的 Provider/Environment/Installation，以及固定节点或调度范围 |
| Process Instance | 实际观察到的 PID/container ID/Pod UID、启动时间、版本和退出来源 |
| NetworkProfile | 描述 bind、advertise、internal、published 地址和端口 |
| PortReservation | 在正确网络作用域登记端口所有权和生命周期 |
| CPUAllocation | 描述容量预算、quota/request 或独占 cpuset |
| Artifact | Cluster 公共配置、Shard 存档、Mod 配置或备份分片 |
| Operation Plan | 对一个或多个目标执行的类型化、可审计操作 |

### 4.2 状态所有权

- 控制中心保存期望状态：拓扑、放置、配置版本和操作计划。
- Runtime Provider/Agent adapter 上报观察状态：实际 Node、本地文件、进程、资源和任务进度。
- 观察状态不能自动覆盖期望状态。
- 控制中心不能在 Agent 离线时假设操作已成功。
- 同一 Shard 在任一拓扑版本中只能有一个有效 Placement。
- 进程、容器、Pod、端口和 PVC 等观察状态都不能替代控制面的 Shard lease 与 fencing 所有权。
- Provider 必须声明自己如何部署以及能够控制哪类 Runtime；Agent 容器化不自动获得宿主 native 或 Docker 控制能力。

## 5. 操作范围

系统需要支持以下选择范围：

- 单个分片。
- 一个房间的全部或部分分片。
- 单个节点上的全部受管分片。
- 用户选择的一组节点、房间或分片。

房间级操作不是简单并发发送命令，而是带前置条件和顺序的协调计划。

建议操作：

- 发现和刷新节点能力。
- 分片启动、停止、重启、保存和发送受控控制台动作。
- 房间协调启动、停止、重启和保存。
- DST 版本检查、下载、切换和回滚。
- Mod 预下载、校验、配置发布、安装、修复和回滚。
- 分布式备份、校验、恢复和删除。
- Shard 迁移预检、复制、切换和回滚。

所有操作必须具备：

- 类型化输入，不下发任意 Shell。
- 幂等键和目标拓扑版本。
- 前置条件、超时、取消和结果审计。
- 每个节点/分片独立的执行状态。
- 部分成功的明确结果，不把部分成功包装成整体成功。
- 可恢复操作的恢复入口和不可恢复操作的高风险确认。

### 5.1 CPU 容量约束

同一节点允许运行多个房间和多个世界分片，但产品默认按“每个运行中的世界分片预留 1 个物理核心”计算建议容量：

- 节点上运行中的 Shard 数量不能只看房间数量。
- 同一房间的地表、洞穴和自定义附加世界分别计为一个 Shard；同一节点上的多个房间需要继续累计，不能按 Cluster 合并计算。
- 默认至少为操作系统、Agent、SteamCMD 和备份任务预留 1 个核心或一段可配置余量。
- 容量展示需要区分物理核心与逻辑处理器；在没有可靠物理核心信息时采用更保守的值。
- 启动后超过建议容量时允许用户确认继续，但必须提示可能出现 tick 延迟、网络抖动和卡顿。
- 批量启动前展示每个节点的当前 Shard 数、启动后 Shard 数和建议上限。
- 自动放置默认拒绝把新 Shard 调度到已达到建议上限的节点。
- 实际负载、Mod 复杂度和玩家数量可能使单个 Shard 需要超过一个核心的性能预算，不能把“每核一层”解释为性能保证。
- 容量建议、CPU request/quota 和独占核心分配必须分开存储与展示。
- CPU 策略默认为 `none`；`reserved` 表示份额/上限但不保证固定核心；`exclusive` 必须验证物理核心拓扑并保证 CPU 集合不重叠。
- Linux 裸机优先用 cgroup v2/cpuset，Docker/Podman 用 quota 与 `cpuset-cpus`，Kubernetes 独占模式要求 CPU Manager static、Guaranteed QoS 和整数 CPU request；macOS 不宣称支持可移植的独占绑核。
- 默认系统预留核心不进入独占分配池，SteamCMD、备份与压缩任务使用共享池或独立低优先级配额。
- 同一物理/虚拟 Node 上的 native 与所有 container Shard 合并计算容量，不能按 ExecutionEnvironment 重复计算核心。
- Kubernetes 普通模式以 Worker `allocatable` 和 requests 做调度预检，exclusive 还必须校验 kubelet 返回的实际 cpuset。

DST 的线程利用、物理核心/逻辑核心差异和平台表现需要在资料及实机阶段继续核验；在取得证据前使用上述保守预警规则。

### 5.2 网络与端口约束

系统必须分别管理 Shard 通信 `master_port`、玩家 `server_port`、Steam `authentication_port` 和 `master_server_port`，并区分监听地址、其他 Shard 使用的 Master 地址、容器/Pod 内部端口和对外发布端口。

- 裸机和 host network 按 Node/地址/协议判冲突。
- bridge 网络允许不同容器复用内部端口，但宿主 published UDP 端口必须唯一。
- Kubernetes Pod 端口可以跨 Pod 复用；`hostPort` 按 Worker Node、`NodePort` 按 Cluster 判冲突，独立 Service 可复用相同 Service port。
- 同机裸机 Shard、容器 Shard 和 host-network 容器共享宿主作用域时必须统一判冲突，不能各管一份。
- Master 的监听端口由 Master Placement 占有；Secondary 保存可达 Master Endpoint 引用。跨 Node、容器或 Pod 的 `master_ip` 禁止使用 loopback。
- 自动端口分配先创建带 topology revision 的 PortReservation，再写配置；失败或迁移后按状态释放，不能靠扫描一个数字范围后直接写文件。

启动预检检查租约、实际监听、Master 可达性、Shard ID、Cluster key、对外映射、防火墙/NetworkPolicy 和观察数据新鲜度。由于 UDP 无响应不能证明端口空闲，必须组合租约、系统 socket 清单和进程归属判断。

Docker bridge 默认保持 `publishedPort == server_port`。Kubernetes NodePort/LoadBalancer 若发生端口转换，必须验证 Steam 列表公布端点和真实公网连接；验证前只作为实验 profile。Master 连接优先使用稳定 Service ClusterIP，DNS 是否可写入 `master_ip` 需实机确认。

## 6. Agent 协议方向

现有共享密钥和通用命令协议需要逐步收敛为能力声明与类型化动作：

```text
Agent enroll
  -> 节点身份和能力协商
  -> inventory snapshot
  -> desired operation lease
  -> ack
  -> progress events
  -> terminal result + observed snapshot
```

关键要求：

- Agent 主动向控制中心建立连接，默认不要求公网开放 Agent 端口。
- 每个 Agent 使用独立身份和凭证，不能长期共享一个全局密钥。
- 操作携带唯一 ID、幂等键、拓扑版本和租约/fencing token。
- Agent 对允许的路径、房间、分片和动作再次做本地校验。
- Agent 重连后上报仍在执行、已完成和未知结果，控制中心进行恢复。
- Agent 版本和协议版本不兼容时进入只读或升级要求状态。
- 任意命令执行只保留为受限诊断能力，不作为产品操作主路径。

### 6.1 Runtime Driver 契约

控制面和 Agent 内部先建立一个统一 Driver 边界：

```text
Discover / Inventory / Status
Start / Stop / Restart / Save / SendConsole
ConsoleHealth / ObserveOperation
StreamLogs / ReadArtifacts
StageBackup / RestoreBackup
PrepareMod / PublishConfig / VerifyRuntime
```

Driver 输入只引用已登记的 `environmentId`、`installationId`、Room、Shard、配置版本、租约和 fencing token，不接受任意宿主命令、镜像、路径、挂载或 Kubernetes manifest。只有显式 `rawConsole` capability 和高风险授权可以携带受限 Lua payload；它不能转换为宿主 Shell。Driver 输出保留平台原始身份：native PID/tmux socket/session/pane、container ID、Pod UID 和 restart count。

实现顺序：

1. 建立 Agent 内唯一的 per-Shard console dispatcher；本地与 Agent 都通过它串行发送，停止期间拒绝新命令，后台探针有界合并。
2. 把现有 tmux 分片控制封装为 `native` Driver；按 RuntimeInstallation 隔离 socket，固定并校验 pane 与 DST process instance。
3. 将现有“tmux 已接收”映射为 `sent`，再按操作类型实现完成证据：ready observation、process exit、snapshot barrier、受管文件回执或 probe nonce。原始 Lua 不承诺 `confirmed`。
4. 交付类型化 `StreamLogs/ReadArtifacts`，再把房间操作、命令、自动化、玩家/世界探针、日志、备份、Mod 和 Runtime 诊断全部改为依赖 Driver 契约。
5. 迁移旧 cron/taskbridge、动态日志监控和 tmux 枚举旁路；CI 只允许 native Driver/低层 tmux adapter 直接导入 tmux package。
6. 增加 `container` Driver；首版保留 Shard 容器内 `tmux-compat` console transport，Agent 仍独立运行。
7. 最后增加 `kubernetes` Driver，不在控制器中拼接 kubectl Shell 命令。

每个 Driver 必须声明 capability。缺少 `consoleInput`、`managedRuntimeReceipt`、`snapshotBarrier`、`rawConsole`、`exclusiveCpu`、`volumeSnapshot`、`publishedUdpEndpoint` 等能力时，UI 显示不支持或明确降级选项，不能假定所有平台等价。

`SendConsole` 必须写入正在运行的 DST 主进程 stdin。`docker exec` 只会创建新进程，不能直接作为实现；它仅可调用固定的 tmux/console client。每个请求携带 operation ID/kind、目标 instance ID、deadline、lease 和 fencing token，同一 Shard 的所有调用源由一个 dispatcher 串行发送。transport 接受只产生 `sent`；`ObserveOperation` 根据操作类型读取证据。Agent/Engine 重启、pane 输入状态不确定或 instance 变化时返回 `unknown/input_dirty`，危险命令不自动重放。

完成证据固定映射：启动看新 instance ready；停止看目标进程退出；热保存看所有必需 Shard 的 snapshot 屏障；`customcommands.lua` 看 session/shard/request 匹配的持久回执；探针看 nonce 完整批次；原始 Lua 默认只到 `sent`。不能为了统一回执强制包装任意 Lua，从而改变 Mod 或控制台语义。

远程迁移清单必须逐项关闭本地旁路：

| 当前路径 | 目标 |
| --- | --- |
| `routers/router.go` 中共享本机 `tmuxControl/savePath` | 按 Placement 解析 Runtime Driver 与 Artifact provider |
| `pkg/taskbridge`、tmux cron task | 迁移为 Room/Shard ID 的自动化领域动作；无法映射的旧任务禁用并生成报告 |
| `service/logmonitor`、`service/logparser`、server monitor 的 tmux 枚举 | 使用 topology observation 与 `StreamLogs` |
| 玩家/世界 console fallback | 使用统一 dispatcher，后台请求可合并并受生命周期门禁 |
| 备份固定等待 2 秒 | snapshot barrier；不可用时使用 cold-consistent fallback |
| 旧 `/api/tmux` 写操作 | 兼容期转发 typed Driver，禁止直接执行；最终删除 |

## 7. 分布式生命周期

每个房间操作先生成计划：

```text
解析目标拓扑
  -> 检查节点在线、版本、端口、磁盘和配置
  -> 获取房间操作锁与拓扑租约
  -> 下发分片步骤
  -> 收集逐目标结果
  -> 验证最终观察状态
  -> 提交成功或保留部分失败状态
```

需要研究和验证后确定：

- Master 与其他 Shard 的启动、停止顺序。
- Master 不可用时是否允许启动或保留 Secondary。
- 房间停止时如何防止新的分片启动任务插入。
- 批量房间重启的并发上限和分批策略。

无论最终顺序如何，控制协议必须支持：

- 单独停止非 Master 分片。
- 停止 Master 前提示其对房间的影响。
- 一键停止整个房间。
- 节点失联后将分片标记为 `unknown`，而不是直接标记为 stopped。
- 未取得新的 fencing token 前，禁止在另一节点启动同一 Shard。

## 8. 一致性备份与恢复

跨节点备份不能把不同时间点的目录直接拼接。目标是建立“逻辑备份集”：

```text
锁定房间变更
  -> 请求统一保存点或安全停止点
  -> 所有分片确认到达屏障
  -> 各 Agent 创建本地只读快照
  -> 生成文件 manifest、hash、Session 和版本信息
  -> 可选上传集中存储
  -> 控制中心验证完整性
  -> 登记一个包含所有分片的原子备份集
```

备份集应包含：

- Room/Cluster 公共文件。
- 每个 Shard 的目录快照。
- 拓扑、节点、DST 版本、Mod 版本和 Runtime 版本。
- 每个子快照的大小、hash、创建时间和保存点标识。
- 完整、部分、失败、损坏和过期状态。

约束：

- 任一必需分片失败时，不得标记为完整备份。
- 默认只允许整套恢复。
- 单分片恢复必须经过资料和实机验证，并作为高级危险操作。
- 恢复前验证目标拓扑、端口、磁盘、版本和 Mod。
- 上传中断可以断点续传，但不能重复提交不同内容到同一子快照 ID。
- 中央存储不可用时，可保留各节点本地快照和未汇总状态。

## 9. Mod 分发

Mod 管理继续区分：

- 节点级 Workshop 下载缓存。
- 安装级 `dedicated_server_mods_setup.lua`。
- 房间/分片级 `modoverrides.lua`。
- 运行时 UGC 安装与加载状态。

跨节点发布采用预检和分阶段提交：

```text
解析房间 Mod 期望状态
  -> 所有目标节点预下载
  -> 校验 Workshop ID、版本、文件和依赖
  -> 所有节点准备完成
  -> 发布分片配置
  -> 按需要协调重启
  -> 从日志确认加载
```

关键风险：

- 同一房间的节点下载到不同 Mod 版本。
- 某节点磁盘不足或 SteamCMD 失败。
- macOS/Linux 节点的缓存目录和二进制差异。
- 配置已发布但文件未准备完成。
- 部分节点更新后房间无法整体回滚。

系统必须在启动前显示不一致并默认阻止高风险启动，不能只以 SteamCMD 退出码判断成功。

## 10. 玩家与状态聚合

玩家以 KU ID 为房间内唯一键：

- 聚合房间内所有 Shard 报告，跨分片迁移不能重复计数。
- 记录当前 Shard、Node、角色、在线状态和最后观测时间。
- 同一玩家被多个 Shard 同时报为在线时展示冲突诊断。
- 玩家操作路由到最近确认所在的 Shard，发送前重新验证。
- Agent 离线后保留最后状态并标记 stale/unknown。

日志、世界状态和 Runtime 诊断使用相同的来源字段：

- `roomId`
- `worldId/shardId`
- `nodeId`
- `environmentId`
- `installationId`
- `observedAt`
- `freshness/stale`

## 11. 产品信息架构

### 11.1 集中总览

展示：

- 在线/离线/配置异常节点。
- 运行中的房间和分片。
- 在线玩家总数与异常重复报告。
- CPU、内存、磁盘和版本不一致告警。
- 最近失败操作、备份和 Mod 发布。

### 11.2 房间看板

核心是拓扑表，而不是节点卡片堆叠：

| 分片 | 角色 | 执行位置 | 端点 | CPU | 运行状态 | 玩家 | 数据时间 | 操作 |
| --- | --- | --- | --- | --- | --- | ---: | --- | --- |

支持：

- 选择部分分片执行操作。
- 房间整体启动、停止、重启、保存和备份。
- 查看依赖、网络和版本异常。
- 查看期望节点/节点池与实际 Worker、内外 UDP 端点、CPU 策略和能力降级。
- 进入玩家、日志、世界状态、Mod 和备份页面时保留房间上下文。

默认只展示“运行位置、可达端点、容量结论和异常”。bind address、network namespace、cpuset、PVC 等细节进入高级抽屉，避免个人服主必须先理解基础设施；商家和多机用户可保存并复用 Network/CPU/Storage profile。

### 11.3 节点看板

展示：

- CPU、内存、磁盘、网络、系统与 Agent 版本。
- Runtime Provider、执行环境、网络作用域、CPU 分配和端口租约。
- DST 安装和 Workshop 缓存。
- 当前运行的房间分片和资源占用。
- 待执行、运行中和失败任务。
- 能力缺失、路径错误、端口冲突和磁盘预警。

### 11.4 操作中心

展示分布式 Job 的父子关系：

- 父 Job 表示房间或批量操作。
- 子目标表示每个节点/分片步骤。
- 清晰区分等待、执行、成功、失败、取消、跳过和未知。
- 失败后提供重试失败目标、恢复配置或回滚入口。

## 12. 主要风险

| 风险 | 必须具备的防护 |
| --- | --- |
| 网络分区导致双启动 | Placement 唯一性、租约和 fencing token |
| Agent 离线状态误判 | `unknown/stale`，不把离线等价为停止 |
| 两个控制中心同时操作 | 控制面租约、拓扑版本和写入所有权 |
| 分布式备份时间点不一致 | 保存屏障、manifest 和完整性状态 |
| Master 异常退出 | 明确依赖状态、恢复计划和实机故障验证 |
| Mod 或 DST 版本不一致 | 发布前预检、全目标准备和版本锁 |
| 单节点分片数超过 CPU 容量 | 每 Shard 一核的保守预算、系统预留和启动警告 |
| CPU 配额被误认为独占核心 | 三档 CPU policy、cpuset 观察值和能力校验 |
| 绑核重叠或硬件拓扑变化 | 持久化分配、SMT/NUMA 拓扑、启动前重验 |
| 容器内部端口与宿主端口混淆 | NetworkProfile、网络作用域和 PortReservation |
| Docker Socket 导致宿主越权 | 标签/镜像/挂载白名单、最小权限代理、禁止任意容器参数 |
| 把主服务容器化误认为 DST 容器化 | 三层 DeploymentProfile 独立配置和能力展示 |
| 主服务容器直接接管宿主 | 默认禁用 host PID/DST 路径/Docker Socket，改由 Agent 控制 |
| Agent 容器状态卷丢失 | 持久化身份/fencing/幂等状态，丢失后阻止自动接管 |
| Agent 容器越权控制宿主 native DST | 单独 host-integration profile、最小 bind mount 和高风险确认 |
| 容器能启停但不能发送 DST 命令 | Runtime Driver 强制 `consoleInput/operationEvidence` capability 和等价验收 |
| 命令在重启边界发给错误实例 | instance ID、fencing、per-Shard dispatcher 和操作类型证据 |
| 原始 Lua 被包装后与 Mod 行为不同 | 原始 Lua 只报告 sent；可靠产品动作进入受管 Runtime allowlist |
| 多来源控制台输入交叉 | Agent per-Shard dispatcher、有界队列、停止门禁和输入 dirty 状态 |
| 同宿主多个 Installation 的 tmux session 冲突 | 按物理存档根目录派生私有 tmux socket、内核独占 owner lock、稳定 session identity、进程归属预检和固定 pane ID |
| tmux 返回权限/socket 错误却被当成 stopped | ConsoleHealth 细分错误，只有明确 not_found 才是不存在 |
| 服主手动 attach 与自动命令交叉 | 只读 attach；可写 attach 获取 maintenance lease，未知 writer 时阻止 dispatcher |
| 容器用空闲进程保活，DST 已退但容器仍 Running | init + runtime supervisor 作为 PID 1，退出码和 instance observation 绑定 DST |
| Engine restart policy 绕过 fencing 自动拉起旧 Shard | 默认控制器恢复 desired state；禁止默认 always/unless-stopped |
| 主服务读取远程 savePath | 仅允许类型化 ArtifactRef、范围读取、generation 和大小限制 |
| 普通日志泄露完整 Lua/敏感参数 | 审计保存类型、摘要、hash 和证据引用；敏感内容分权与脱敏 |
| 容器可写层或 Pod 消失导致存档丢失 | 显式 volume/PVC、回收策略和恢复前校验 |
| K8s 节点失联后重复调度写同一存档 | Room lease、fencing、Pod UID、PVC 所有权与旧实例终止确认 |
| 部分成功 | 逐目标结果、补偿步骤和显式降级状态 |
| Agent 凭证泄露 | 独立节点身份、轮换、撤销和最小权限 |
| 任意远程执行 | 类型化动作、本地二次校验和审计 |
| 磁盘满或上传中断 | 容量预检、临时文件预算、断点续传和清理 |
| macOS/Linux 差异 | 能力声明、平台适配和跨平台测试矩阵 |
| 时钟偏差 | 控制面序列/版本为准，时间只用于展示和诊断 |

## 13. 实施阶段

### Phase 1：资料与架构基线

- 完成资料核验记录。
- 完成 Agent 与本地服务差距矩阵。
- 固化领域模型、协议版本和状态机。

### Phase 2：只读节点与拓扑（已完成）

- Agent 上报安装、房间、分片、进程、资源和能力。
- 控制中心保存拓扑快照并展示数据新鲜度。
- 前端提供集中总览、房间拓扑和节点详情。

已交付说明：支持同一节点规划多个房间，也支持同一房间的多个世界分片放在同一节点；容量按该节点运行中的 Shard 总数而非房间数计算。2 个及以下有效 CPU 不额外预留整核，保证常见 2C4G 主机可以运行 Master+Caves；3 个及以上有效 CPU 默认预留 1 核。界面明确提示“一有效 CPU 最多运行一层世界”，超出建议值只要求用户确认并告警，不构成硬限制或性能保证。

### Phase 3：类型化分片控制（已完成）

- 实现单分片启动、停止、重启和状态确认。
- 增加幂等、租约、fencing、审计和断线恢复。
- 保持本机操作路径兼容。

已交付说明：Agent 2.5.1 支持 `shard.status/start/stop/restart/save` 固定动作，不接受任意 Shell 或请求内路径；Agent 从本地受信安装注册表解析路径，并持久化最高 fencing token、完成结果和中断后的 `unknown` 状态。控制器使用持久化房间租约和单调 token，Job 保留逐世界结果，运行审计记录 target、Agent、operation、lease、fencing 和拓扑 revision。执行器只使用 `appliedTargetId`；节点离线、清单过期、文件缺失或运行冲突时拒绝操作，不自动改在本机执行。

### Phase 4：房间级协调与批量操作（已完成）

- 实现房间整体操作计划和逐目标状态。
- 支持选择多个房间、节点或分片分批执行。
- 建立部分失败和恢复入口。

已交付说明：`POST /api/v2/rooms/:roomId/actions/:action` 在启动和重启前按 `appliedTargetId` 计算启动后容量，首次超配或容量未知时返回 `CAPACITY_RISK_CONFIRMATION_REQUIRED`，确认后允许继续但不提供性能保证。整房间在任一世界预检失败时不会先操作其他世界。`POST /api/v2/rooms/actions/:action` 支持多个房间合并预览容量，并用 `roomId:worldId` 作为 Job 目标标识；不同房间使用独立租约，单个房间失败不阻塞其他房间。前端已统一所有单房间、单世界和跨房间启动、重启入口的 shadcn-vue 风险确认；批量界面按房间选择世界，显示每层世界的当前生效节点和运行状态，完整展示部分或全部失败的逐世界结果，并在重新读取拓扑与状态后只重试未成功项。同一台服务器可以承载同一房间或不同房间的多层世界；2 个及以下有效 CPU 不预留整核，3 个及以上默认预留 1 核，超出动态建议值才需要确认。这是可确认绕过的保守告警，不是硬限制。

### Phase 5：主服务容器 + 裸机 Agent + native 基线（已完成）

- 新增 ControlPlaneDeployment、ProviderDeployment 和 RuntimeExecution 三个独立 profile，禁止用一个 `containerized` 字段代替。
- 交付主服务非 root OCI 镜像与 Compose：持久化数据库/WAL、配置和密钥引用；默认不挂 host PID、DST 路径或 Docker Socket。
- 交付 Linux systemd 与 macOS launchd 的 Agent 安装/升级/卸载流程；验证 Agent 通过主服务容器暴露的 WebSocket 主动连接。
- 新增 ExecutionEnvironment、Runtime Driver capability 和统一观察身份。
- 新增 per-Shard console dispatcher；命令、自动化、备份、玩家/世界探针和 Runtime Bridge 共用同一串行与生命周期门禁。
- 把现有 tmux 实现迁入 `native` Driver：每个 Installation 私有 socket、固定 pane、literal 输入、instance 校验、输入 dirty 和细分 ConsoleHealth。
- 交付 Agent 本地 console attach CLI：默认只读；可写模式需要超时 maintenance lease，Web UI 不提供宿主 Shell。
- 建立操作类型化证据；受管 Runtime 继续使用结构化文件回执，原始 Lua UI 明确显示“已发送，未确认执行”。
- 交付 `StreamLogs/ReadArtifacts`，ArtifactRef 不包含任意路径并支持 generation、offset、大小上限和内容校验。
- 迁移 cron/taskbridge、动态日志监控、日志解析和旧 tmux API 旁路，并增加直接 tmux import 的架构回归检查。
- 新增 NetworkProfile、PortReservation、四类 UDP 端点和作用域冲突预检。
- 新增 CPU policy；先交付 `none`、拓扑观察和能力展示，再在 Linux 交付 `reserved/exclusive`。
- 生命周期只在明确停服后释放 CPU 约束；控制器重启后本地立即回读、远程等待可用清单再逐项恢复，并用 CAS 防止旧观测覆盖新配置。
- 前端拓扑和执行确认展示环境、内外端点、CPU 策略、实际 cpuset 和最近校验时间。
- 把当前 local/Agent target 回填为 RuntimeProvider + Node + `native/default` 环境；旧 API 在兼容期从新模型投影返回。
- 把存档导入的全局端口重写改为基于目标 NetworkScope 的分配器；native 单节点行为保持不变。
- 存档导入原子持久化 apply journal 与端口 lease；回滚或释放失败保留 journal，重启后继续清理。

完成标准：本机和现有 Agent 的所有已交付功能无回归；旧配置自动映射为 `native/default` 环境；未配置高级策略时运行行为不变。主服务容器 + 同宿主/远程裸机 Agent + 裸机 DST 可以完成发现、启动、停止、保存、命令、自动化、玩家/世界探针、Runtime 激活、日志、Artifact 回执和审计，且主服务容器没有宿主控制权限或远程路径读取旁路。

### Phase 6：容器 Agent 与可选容器 Runtime（Docker 基线已完成）

- 交付 Agent 非 root OCI 镜像，并把 Agent ID、密钥、Runtime 注册表、最高 fencing token 和幂等结果放入持久状态卷。
- Agent capability 分成 `container-runtime` 和 `native-host-integration`；前者为推荐容器模式，后者在 Linux 实机验证前保持高风险实验状态。
- `native-host-integration` 明确要求同 UID/GID、最小受信路径、tmux socket/运行目录和宿主进程可见性；缺少任一能力时不得报告 native control 可用。
- 交付一个 Shard 一个容器的 Runtime profile、非 root 镜像和持久 volume 契约；Shard 容器首版保留 `tmux-compat`，Agent 不与 DST 合并。
- Shard 镜像使用轻量 init + 固定 runtime supervisor 作为 PID 1；supervisor 创建临时 tmux socket/pane、等待 DST、传播退出原因并处理 SIGTERM，不允许 `sleep infinity` 保活。
- container Driver 交付 `SendConsole/ConsoleHealth/ObserveOperation`；固定 tmux client 动作是兼容基线，Docker attach/stdin 通过专项验证后才可设为默认。
- tmux socket 与控制临时状态位于 `/run` tmpfs；存档、配置、Mod、日志和 staging 使用显式持久 volume，并校验 UID/GID。
- 默认禁用 `always/unless-stopped` 一类脱离 lease 的 Engine 自动恢复；由 Agent 根据持久 desired state 和 fencing 协调恢复。
- 支持 bridge/host 网络、UDP published endpoint、优雅停止和容器退出审计。
- 支持 CPU quota 与 cpuset，明确区分限制份额和独占核心。
- 同一宿主的 native/container 资源合并预检，容器 OOM kill 单独审计；默认不设置未经用户确认的低内存硬上限。
- Agent 只管理有受信标签、镜像和挂载的容器；控制器容器默认不挂 Docker Socket。
- 完成裸机/容器混合 Room，以及同一宿主同时运行 native 与 container Shard 的端口/容量合并预检。

### Phase 7：一致性备份与恢复（cold-consistent 与 hot-consistent 已完成）

- 先实现 `cold-consistent`：协调停止、确认无写入、分片快照、manifest、逻辑备份集和可选恢复原运行状态。
- `hot-consistent` 已通过 Runtime 2.4.0 保存屏障开放：所有运行分片先固定 session/shard/producer instance 与 snapshot，Master 统一触发 `ms_save`；每个分片必须由 `ShardGameIndex.SaveCurrent` 回调证明 snapshot 前进且最终 snapshot 完全相同。证据不足、超时、拓扑或 Runtime 实例变化均失败，不降级成普通在线打包。
- 完成整套恢复、失败恢复和完整性校验。
- 持久化创建/恢复操作阶段、保护备份与失败原因；后台自动恢复中断操作，并提供操作历史和幂等人工重试入口。
- native 与 container Driver 使用同一备份协议；容器存档只从受管 volume staging，不从可写层提取。

### Phase 8：Mod 与版本发布（已完成）

- 已实现跨节点预下载、校验、配置原子发布、协调重启、加载日志确认和故障恢复。
- 已实现 DST 版本一致性预览和安全更新计划：保护备份，Secondary 到 Master 停服，逐 Installation 更新并精确校验 build，Master 到 Secondary 恢复，最后确认新实例加载日志。
- 发布使用 `planHash` 防止陈旧确认；原发布记录原地恢复/重试，已更新目标不会重复执行 SteamCMD，逐 Installation 与逐 Shard 保留证据。
- 受控可变 Installation 已进入可执行发布链路；不可变版本镜像保持独立 profile，同一发布计划禁止静默混用。

### Phase 9：玩家、日志与诊断聚合（已完成）

- 按房间聚合玩家、日志、世界状态和 Runtime 诊断。
- 实现跨分片迁移去重、stale 和冲突展示。
- 日志读取按当前 Placement 路由，房间快照按世界保留 continuation、截断/轮转与部分失败；不会回退读取控制器本机的旧目录。
- 玩家 presence 按用户与分片观测合并，跨分片迁移去重；状态返回来源、观察时间和过期语义，节点离线或数据陈旧不会伪装成实时结果。

### Phase 10：Kubernetes 实验能力（只读观察与预检已完成）

已交付并默认关闭：Provider 状态、只读 Kubernetes REST observation、类型化 mutation preview、启动安全门禁、OpenAPI、最小 namespace RBAC 和前端实验状态页。`preflight` 固定返回 `applyAllowed=false`，不存在 Apply 路由。

以下内容仍是生产化前置条件，不属于当前可用能力：

- 主服务以单副本 Deployment + PVC 运行；SQLite 阶段不宣称多副本 HA。
- Kubernetes Driver 使用受限 ServiceAccount 管理指定 namespace/label 范围。
- Agent DaemonSet 只在需要宿主清单或 native Runtime control 时可选安装，默认不授予 privileged/hostPath/hostPID。
- 一个 Shard 一个副本为 1 的有状态工作负载，独立 PVC、稳定 Master Service ClusterIP 和显式 UDP 暴露策略。
- Secret 保存 Token/cluster key，ConfigMap 只保存非敏感生成配置。
- Readiness 以 DST 世界加载和 Shard 注册为准；节点失联后结合 lease、fencing、Pod UID 和 PVC 所有权决定是否可重调度。
- 支持普通 CPU request/limit；只有集群满足 CPU Manager static 等前提时开放 exclusive。
- 支持保存屏障后的 CSI snapshot adapter，并保持 manifest/上传备份作为通用 fallback。
- Placement 默认交给 scheduler 在允许节点池内选择，固定 Worker 只在高级模式开放；按 Worker 批量停止只作用于实际位于该节点的受管 Shard，不等同于 drain 或迁移。
- Pod Runtime 提供与 native/container 相同的 `SendConsole/ConsoleHealth/ObserveOperation`，控制台不可用时不允许热保存屏障或危险命令。

完成上述实现、故障注入和至少两个 Kubernetes/CSI/CNI 组合验证前，界面固定标记“实验能力”，默认关闭且不宣称生产可用。

## 14. 验收矩阵

至少覆盖：

- 本机单节点、单房间双分片。
- 主服务容器 + 同宿主裸机 Agent + 裸机 DST，主服务不挂宿主控制资源。
- 主服务容器 + 远程裸机 Agent + 裸机 DST，反向代理/WebSocket 重连和错误 localhost 配置提示。
- 主服务容器 + 容器 Agent + 容器 DST，Agent 状态卷保留和重建后幂等恢复。
- 容器 Agent 的 native host-integration 在缺少 PID/tmux/path 能力时拒绝控制，状态卷丢失时拒绝接管。
- container Runtime 分别验证保存、优雅停止、玩家命令、命令目录、自动化和 `customcommands.lua` 热激活。
- tmux-compat 在 Agent 重启后继续发送命令；attach/stdin 在反复 attach、Docker 重启、高日志量和并发请求下不关闭 DST stdin。
- 命令发送后 Shard 恰好重启时返回 `unknown`，不会把危险命令重放给新实例。
- 同一宿主不同存档根目录中的相同 Cluster/Shard 名落到不同 tmux socket/pane；指向同一存档根目录的 Installation 不能同时配置，第二套 Controller/Agent 无法取得 owner lock，且重复 DST 进程会被拒绝。
- 多来源并发发送时严格串行；停止开始后拒绝探针，队列满/过期可观察，partial send 进入 input_dirty 后不会拼接下一条命令。
- raw Lua 的分号、引号、UTF-8、最大长度、换行拒绝和 Mod 自定义全局函数不因 transport 包装改变；UI 只显示 sent。
- ConsoleHealth 分别验证 disabled、not_found、socket_unavailable、permission_denied、pane_dead、process_mismatch 和 input_dirty。
- tmux 只读 attach 不影响自动命令；可写 maintenance attach 暂停 dispatcher，未知外部 writer 触发 external_writer 且不会交叉发送。
- PID 1 supervisor 验证正常退出码、SIGTERM 优雅关服、超时 SIGKILL、OOM、DST 崩溃而 tmux/容器尚存以及 Agent 离线时的状态。
- Docker daemon/节点重启后，Shard 不会绕过 desired state、lease 和 fencing 自动形成双实例。
- 远程命令回执、日志 follow、offset 续传、日志截断/轮转、Artifact generation 变化和大小限制。
- 旧 cron/taskbridge 和日志监控不再直接访问 tmux；无法映射的旧任务被禁用并可人工修复。
- 两节点、一个房间、Master 与 Caves 分离。
- 三节点、一个房间、三个自定义分片。
- 一个节点运行多个房间。
- 单节点在建议核心容量以内和超过容量时的启动预检。
- Linux native 的 `none/reserved/exclusive`，SMT sibling 不重复分配，以及重启后 cpuset 重验。
- macOS 明确不提供 exclusive，选择后返回能力不支持而不是假成功。
- Docker bridge 下容器内部端口复用、宿主 UDP 映射唯一和跨宿主 Master 可达。
- Docker host network 与同机 native Shard 的端口、CPU 和进程冲突合并判断。
- 容器强制退出、优雅停止超时、volume 缺失、镜像版本不一致和 Agent 重启后的归属恢复。
- Master 节点异常退出和恢复。
- Secondary 节点异常退出和恢复。
- Agent 断线、网络分区和控制中心重启。
- 操作提交后重复请求和 Agent 重连。
- cold-consistent 与 hot-consistent 备份成功、保存屏障超时、部分失败、上传中断和整套恢复；热证据不足时不得标记 complete。
- Mod 下载失败、版本不一致、配置发布失败和回滚。
- 磁盘不足、端口冲突和 DST 版本不一致。
- Linux/Linux 与 macOS/Linux 组合。
- Kubernetes Master Service、玩家 UDP 暴露、NodePort 冲突、NetworkPolicy 阻断和 Endpoint 过期。
- Kubernetes CPU Manager static 可用/不可用、PVC Retain、Worker NotReady、旧 Pod 未终止和 CSI snapshot 部分失败。
- Kubernetes 主服务不部署 DaemonSet 也能管理 Pod Runtime；安装 DaemonSet 后 RBAC/host capability 与声明一致。

每个场景必须验证：观察状态、用户提示、Job 结果、审计、数据安全和恢复路径。

### 14.1 Debian 12 真实双分片验收（2026-08-19）

已在全新 Debian 12 的隔离 Compose 项目中使用真实 DST 运行 Master 与 Caves，两个分片由容器 Agent 识别为同一房间的两个 `aligned` Placement。运行期间通过正式 API 创建 `hot-consistent` 备份集，结果为 `verified`：两个分片均从 snapshot 8 前进到相同的 snapshot 9，回执均为 `proof=save_current_callback`，producer instance 互不相同；两个 part 的 ZIP CRC、64 位 SHA256、共享文件 SHA256 和 `shared/`、`shard/` 内容边界全部通过校验，分片在备份期间没有停止或重启。

本次实机验收同时暴露并修复了三个仅靠模拟测试未覆盖的兼容问题：Compose 的 `dst-saves` 卷根已经是 Cluster 父目录，必须使用 `DST_CONF_DIR=.`；远程配置投放必须显式保留每个分片的 `save/mod_config_data/dst-admin/` 空目录；真实 tmux 会把格式字符串中的 Tab 渲染为下划线，控制台进程与客户端探针改用受约束的 `|` 分隔。另将 DST `SetPersistentString` 写出的 `KLEI     1 ` JSON 头纳入统一严格解析，否则 snapshot 屏障已准备成功但控制面会持续判定回执不可读。

验收结束后已删除两个临时 DST 容器和临时 Cluster Token，按原始 SHA256 恢复 `cluster.ini`，清理认证 Session；旧 E2E 栈和既有真实 DST 容器启动时间未变化。隔离控制面保留已验证备份集作为验收证据，Agent inventory 已刷新为零临时进程。

## 15. 交付文档

- `docs/distributed-room-management-plan.md`：本执行计划。
- `docs/multi-node-dst-research.md`：外部资料和实机结论。
- `docs/distributed-room-management.md`：最终产品与技术设计。
- OpenAPI 与 Agent 协议契约。
- 数据迁移、部署、回滚和故障演练说明。

在资料和故障语义未确认前，不直接开放跨节点破坏性操作；先交付只读拓扑，再逐层开放控制、备份和 Mod 发布。
