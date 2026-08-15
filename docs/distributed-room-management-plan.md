# 多节点集中管理执行计划

> 状态：已批准，进入资料核验与实现阶段
> 更新日期：2026-08-15
> 范围：多台服务器集中管理、一个房间跨节点运行多个世界分片、裸机/容器/Kubernetes 执行环境、集中操作与可观测性

当前进度：Phase 1 至 Phase 4 已完成。节点清单、容量、新鲜度、房间拓扑与 Placement 规划已交付；单 Shard 类型化控制、控制面房间租约、Agent fencing/幂等保护和运行审计也已交付。Phase 4 已交付单房间与跨房间启动容量预检、结构化风险确认、房间内全量预检、跨房间批量 Job、集中看板的批量选择和未成功项重试入口。Placement 当前仍只保存期望位置，不会迁移分片；只有后续迁移流程成功写入 `appliedTargetId` 后才能对远程目标执行，失败时不会回落本机。当前 Runtime 仍是裸机/虚拟机上的 tmux 实现；执行环境、端口租约、CPU 绑核、容器和 Kubernetes 均未实现。下一阶段先完成平台抽象与网络/CPU 基线，再继续跨节点一致性备份。

## 1. 目标

DST Admin 需要从“管理当前机器上的 DST”扩展为“本地优先、可选多节点”的集中控制系统：

- 默认继续管理控制器所在本机，不要求部署 Agent。
- 用户拥有多台服务器时，可以在每台机器安装 Agent 并接入同一个控制中心。
- 一个节点可以运行多个房间或多个世界分片。
- 一个房间可以把不同世界分片放置在不同节点，以分摊 CPU、内存和磁盘负载。
- 同一领域模型支持裸机、Docker/Podman 容器，并为未来 Kubernetes 预留明确 Driver；容器化不减少房间、Mod、日志、备份或控制能力。
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

当前不足：

- Runtime 只实现 `native + tmux`，尚无统一 Driver 和 ExecutionEnvironment。
- 四类端口只被读取或在本地导入时重写，尚无网络作用域、端口租约、对外地址和 Master 可达性预检。
- CPU 只提供“一核一层”的建议容量与超配确认，尚无 quota、cpuset、SMT sibling 或 NUMA 分配。
- Placement 仍是规划能力，尚无 Shard 文件迁移、切换和回滚流程。
- 跨节点一致性备份、Mod/DST 发布、玩家/日志聚合尚未交付。
- 容器和 Kubernetes Driver、部署模板、持久卷、Service、RBAC 与 NetworkPolicy 尚未交付。

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
  -> Runtime Provider（local/agent/kubernetes）
      -> Node（物理机、虚拟机或 Kubernetes Worker）
          -> Execution Environment（native/container/kubernetes）
              -> Runtime Installation（DST 安装实例）
                  -> Shard Placement（逻辑世界的生效位置）
                      -> Process Instance（进程/容器/Pod 实例）
```

Room / Cluster 与 Shard / World 是控制面的逻辑对象，不从属于某一 Node；Placement 把 Shard 映射到具体执行环境。

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
Start / Stop / Restart / Save
StreamLogs / ReadArtifacts
StageBackup / RestoreBackup
PrepareMod / PublishConfig / VerifyRuntime
```

Driver 输入只引用已登记的 `environmentId`、`installationId`、Room、Shard、配置版本、租约和 fencing token，不接受任意命令、镜像、路径、挂载或 Kubernetes manifest。Driver 输出保留平台原始身份：native PID/tmux session、container ID、Pod UID 和 restart count。

实现顺序：

1. 把现有 tmux 分片控制封装为 `native` Driver，API 与行为保持完全等价。
2. 房间操作、日志、备份、Mod 和 Runtime 诊断全部改为依赖 Driver 契约。
3. 增加 `container` Driver，容器内不启动 tmux。
4. 最后增加 `kubernetes` Driver，不在控制器中拼接 kubectl Shell 命令。

每个 Driver 必须声明 capability。缺少 `exclusiveCpu`、`volumeSnapshot`、`publishedUdpEndpoint` 等能力时，UI 显示不支持或明确降级选项，不能假定所有平台等价。

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

已交付说明：支持同一节点规划多个房间，也支持同一房间的多个世界分片放在同一节点；容量按该节点运行中的 Shard 总数而非房间数计算，默认每个 Shard 占用 1 个物理核心预算并额外预留 1 核。界面明确提示“一核心最多运行一层世界”，超出建议值只要求用户确认并告警，不构成硬限制或性能保证。

### Phase 3：类型化分片控制（已完成）

- 实现单分片启动、停止、重启和状态确认。
- 增加幂等、租约、fencing、审计和断线恢复。
- 保持本机操作路径兼容。

已交付说明：Agent 2.2.0 支持 `shard.status/start/stop/restart/save` 固定动作，不接受任意 Shell 或请求内路径；Agent 从本地受信安装注册表解析路径，并持久化最高 fencing token、完成结果和中断后的 `unknown` 状态。控制器使用持久化房间租约和单调 token，Job 保留逐世界结果，运行审计记录 target、Agent、operation、lease、fencing 和拓扑 revision。执行器只使用 `appliedTargetId`；节点离线、清单过期、文件缺失或运行冲突时拒绝操作，不自动改在本机执行。

### Phase 4：房间级协调与批量操作（已完成）

- 实现房间整体操作计划和逐目标状态。
- 支持选择多个房间、节点或分片分批执行。
- 建立部分失败和恢复入口。

已交付说明：`POST /api/v2/rooms/:roomId/actions/:action` 在启动和重启前按 `appliedTargetId` 计算启动后容量，首次超配或容量未知时返回 `CAPACITY_RISK_CONFIRMATION_REQUIRED`，确认后允许继续但不提供性能保证。整房间在任一世界预检失败时不会先操作其他世界。`POST /api/v2/rooms/actions/:action` 支持多个房间合并预览容量，并用 `roomId:worldId` 作为 Job 目标标识；不同房间使用独立租约，单个房间失败不阻塞其他房间。前端已统一所有单房间、单世界和跨房间启动、重启入口的 shadcn-vue 风险确认；批量界面按房间选择世界，显示每层世界的当前生效节点和运行状态，完整展示部分或全部失败的逐世界结果，并在重新读取拓扑与状态后只重试未成功项。同一台服务器可以承载同一房间或不同房间的多层世界，但界面固定提醒“一颗物理核心最多运行一层世界，并额外预留 1 核”；这是可确认绕过的保守告警，不是硬限制。

### Phase 5：执行环境、网络与 CPU 基线

- 新增 ExecutionEnvironment、Runtime Driver capability 和统一观察身份。
- 把现有 tmux 实现迁入 `native` Driver，并用现有 API/测试证明行为等价。
- 新增 NetworkProfile、PortReservation、四类 UDP 端点和作用域冲突预检。
- 新增 CPU policy；先交付 `none`、拓扑观察和能力展示，再在 Linux 交付 `reserved/exclusive`。
- 前端拓扑和执行确认展示环境、内外端点、CPU 策略、实际 cpuset 和最近校验时间。
- 把当前 local/Agent target 回填为 RuntimeProvider + Node + `native/default` 环境；旧 API 在兼容期从新模型投影返回。
- 把存档导入的全局端口重写改为基于目标 NetworkScope 的分配器；native 单节点行为保持不变。

完成标准：本机和现有 Agent 的所有已交付功能无回归；旧配置自动映射为 `native/default` 环境；未配置高级策略时运行行为不变。

### Phase 6：Docker/Podman 执行环境

- 交付一个 Shard 一个容器的 Runtime profile、非 root 镜像和持久 volume 契约。
- 支持 bridge/host 网络、UDP published endpoint、优雅停止和容器退出审计。
- 支持 CPU quota 与 cpuset，明确区分限制份额和独占核心。
- 同一宿主的 native/container 资源合并预检，容器 OOM kill 单独审计；默认不设置未经用户确认的低内存硬上限。
- Agent 只管理有受信标签、镜像和挂载的容器；控制器容器默认不挂 Docker Socket。
- 完成裸机/容器混合 Room，以及同一宿主同时运行 native 与 container Shard 的端口/容量合并预检。

### Phase 7：一致性备份与恢复

- 实现保存屏障、分片快照、manifest 和逻辑备份集。
- 完成整套恢复、失败恢复和完整性校验。
- native 与 container Driver 使用同一备份协议；容器存档只从受管 volume staging，不从可写层提取。

### Phase 8：Mod 与版本发布

- 实现跨节点预下载、校验、配置发布、重启和加载确认。
- 实现 DST 版本一致性检查和安全更新计划。
- 明确受控可变 Installation 和不可变版本镜像两种 profile，同一发布计划不混用。

### Phase 9：玩家、日志与诊断聚合

- 按房间聚合玩家、日志、世界状态和 Runtime 诊断。
- 实现跨分片迁移去重、stale 和冲突展示。

### Phase 10：Kubernetes 实验能力

- Kubernetes Driver 使用受限 ServiceAccount 管理指定 namespace/label 范围。
- 一个 Shard 一个副本为 1 的有状态工作负载，独立 PVC、稳定 Master Service ClusterIP 和显式 UDP 暴露策略。
- Secret 保存 Token/cluster key，ConfigMap 只保存非敏感生成配置。
- Readiness 以 DST 世界加载和 Shard 注册为准；节点失联后结合 lease、fencing、Pod UID 和 PVC 所有权决定是否可重调度。
- 支持普通 CPU request/limit；只有集群满足 CPU Manager static 等前提时开放 exclusive。
- 支持保存屏障后的 CSI snapshot adapter，并保持 manifest/上传备份作为通用 fallback。
- Placement 默认交给 scheduler 在允许节点池内选择，固定 Worker 只在高级模式开放；按 Worker 批量停止只作用于实际位于该节点的受管 Shard，不等同于 drain 或迁移。

完成故障注入和至少两个 Kubernetes/CSI 组合验证前，界面固定标记“实验能力”，不宣称生产可用。

## 14. 验收矩阵

至少覆盖：

- 本机单节点、单房间双分片。
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
- 一致性备份成功、部分失败、上传中断和整套恢复。
- Mod 下载失败、版本不一致、配置发布失败和回滚。
- 磁盘不足、端口冲突和 DST 版本不一致。
- Linux/Linux 与 macOS/Linux 组合。
- Kubernetes Master Service、玩家 UDP 暴露、NodePort 冲突、NetworkPolicy 阻断和 Endpoint 过期。
- Kubernetes CPU Manager static 可用/不可用、PVC Retain、Worker NotReady、旧 Pod 未终止和 CSI snapshot 部分失败。

每个场景必须验证：观察状态、用户提示、Job 结果、审计、数据安全和恢复路径。

## 15. 交付文档

- `docs/distributed-room-management-plan.md`：本执行计划。
- `docs/multi-node-dst-research.md`：外部资料和实机结论。
- `docs/distributed-room-management.md`：最终产品与技术设计。
- OpenAPI 与 Agent 协议契约。
- 数据迁移、部署、回滚和故障演练说明。

在资料和故障语义未确认前，不直接开放跨节点破坏性操作；先交付只读拓扑，再逐层开放控制、备份和 Mod 发布。
