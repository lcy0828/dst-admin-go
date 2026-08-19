# 多节点房间集中管理设计

> 状态：Phase 1-9 已实现；Kubernetes 为默认关闭的只读实验能力
> 更新日期：2026-08-16
> 依赖：`distributed-room-management-plan.md`、`multi-node-dst-research.md`

实现状态说明：仓库已经交付控制面、Agent 和单 Shard DST 的 OCI/Compose 基线，Agent 支持 `native` 与受管 `container` Runtime；容器 Driver 通过 label 识别目标，并在 DST 容器内使用固定 tmux transport。控制面与 Agent 已交付 Placement-aware 生命周期、日志/玩家/诊断聚合、cold-consistent 备份、`runtime.mods.v1` 分块分发与原子发布、发布后重启/加载确认，以及 DST 多节点版本计划、保护备份、精确 build 校验和失败恢复。端口租约已按网络作用域持久化；Linux native cgroup v2 和 Docker CPU policy 已执行并回读，macOS 对不支持的策略明确拒绝。Kubernetes 仅接入默认关闭的 status/observe/preflight API 与实验 UI，固定 `applyAllowed=false`，没有生产 Driver 或 Apply 路由。本文明确标注“未来/目标”的部分不代表已经可用。

## 1. 产品边界

系统默认管理控制器本机。只有用户显式添加并配置 Agent 后，远程节点才进入管理范围。

首期不提供任意远程 Shell，也不把控制器本地路径直接套用到远程节点。所有远程能力都通过带版本的类型化协议完成，并由 Agent 对目标路径和资源再次校验。

支持的拓扑包括：

- 一个节点运行一个房间的一个 Shard。
- 一个节点运行同一房间的多个 Shard。
- 一个节点运行多个房间的多个 Shard。
- 一个房间的不同 Shard 分布在多个节点。

部署必须拆成三个独立维度：

- Control Plane Deployment：主服务运行在裸机、普通容器或未来 Kubernetes 中。
- Agent Deployment：Agent 运行在裸机、普通容器或未来 DaemonSet 中。
- DST Runtime：Shard 运行在裸机进程、普通容器或未来 Pod 中。

主服务/Agent 容器化不代表 DST 必须容器化。目标组合为：

| 主服务 | Agent | DST Runtime | 定位 |
| --- | --- | --- | --- |
| 裸机 | 内置本机/裸机 Agent | 裸机 | 当前基线 |
| 容器 | 裸机 Agent | 裸机 | 推荐主路径；主服务与宿主控制权限分离 |
| 容器 | 容器 Agent | 容器 | 推荐完整容器路径；Agent 通过受限容器 Runtime Driver 管理 Shard |
| 容器 | 容器 Agent | 裸机 | 高权限兼容模式；验证前不作为默认方案 |
| Kubernetes | Kubernetes Provider/外部 Agent/可选 DaemonSet | Pod 或外部裸机 | 未来实验能力 |

同一 Room 仍可把 Shard 放在不同 Node 或 Runtime，但只有各目标版本、网络、存储和 Mod 预检全部通过后才允许启动。

控制器的部署位置和 Shard 的执行形态相互独立。主服务容器默认不取得宿主 PID、tmux socket、DST 目录、Docker Socket 或 Kubernetes 凭证；需要管理同宿主裸机 DST 时，也应通过该宿主注册的 Agent 完成。

同机多 Shard 是合法能力，但默认容量策略为“每个运行中的 Shard 预留一个物理核心预算单位，并给系统至少预留一个核心”。

这里的 Shard 统计跨房间累计：同一台服务器既可以承载一个房间的多层世界，也可以承载多个房间的多个世界。界面必须明确提醒用户“一核心最多规划一层运行中的世界”，超过时提示可能卡顿；该规则是保守容量建议，不是硬限制，也不是性能保证。

## 2. 数据模型

### 2.1 RuntimeProvider

```text
id, kind(local|agent|kubernetes)
displayName, connectionState, credentialsRef
deploymentKind(in_process|native|container|daemonset)
protocolVersion, capabilities, observedAt
```

`RuntimeProvider` 表示控制权和连接边界。当前 `local`/`Agent` target 一对一管理一台 Node；未来一个 Kubernetes Provider 可以发现多台 Worker Node。这样不要求为每个 K8s Worker 安装普通 Agent，也不会把“集群连接”错误建模成一台服务器。

Provider capability 需要分别声明 `nativeRuntimeControl`、`containerRuntimeControl` 和 `kubernetesRuntimeControl`。Agent 自己运行在容器里，不代表自动具有控制宿主 native Runtime 或 Docker daemon 的权限。

### 2.2 Node

```text
id, providerId, providerNodeRef
displayName, hostname, os, arch
agentVersion(optional), capabilities
addresses, connectionState
logicalProcessors, physicalCores, physicalCoreSource
memoryTotalBytes, memoryAvailableBytes
observedAt, stale, staleReason
```

`connectionState=offline` 不等于该节点上的 DST 已停止。连接断开后，所有进程观察状态变为 `unknown/stale`。

### 2.3 ExecutionEnvironment

```text
id, providerId, kind(native|container|kubernetes)
desiredNodeId(optional), desiredNodeSelector(optional)
observedNodeId(optional)
driver, orchestratorRef, namespace, runtimeClass
networkMode, networkNamespaceId
cpuPolicy, cpuSet, cpuRequest, cpuLimit
memoryRequestBytes, memoryLimitBytes
capabilities, observedAt, health
```

`ExecutionEnvironment` 是稳定的 DST Shard 逻辑执行和隔离边界，不描述主服务或 Agent 自身怎么部署；实际 PID、container ID 或 Pod UID 属于 ProcessObservation：

- 裸机节点固定存在一个 `native` 环境。
- Docker/Podman 的一个 Shard 容器对应一个稳定环境，容器重建不改变环境 ID；`host` 网络模式显式共享宿主网络作用域。
- Kubernetes 的一个 Shard 工作负载对应一个稳定环境；调度前 `observedNodeId` 可为空，Pod 重建或迁移只更新实际 Worker/Pod observation。Worker Node 仍是容量和故障域。

### 2.4 RuntimeInstallation

```text
id, nodeId, environmentId, displayName
serverPath, executablePath, saveRoot, backupRoot
steamcmdPath, workshopContentPath, ugcPath, modCachePath, modStatePath
platform, serverMode, buildId
observedAt, health
```

路径在所属节点解释。控制器不能把远程路径传入本地房间、备份、Mod 或日志服务。

### 2.5 Room、Shard 与 Placement

```text
Room: id, clusterName, desiredConfigVersion
Shard: id, roomId, directoryName, role, shardId
Placement: shardId, providerId, environmentId, installationId
           desiredNodeId(optional), observedNodeId(optional)
           topologyVersion, desiredState, placementMode
           cpuPolicyRef, networkProfileRef
```

约束：

- 一个拓扑版本中，一个 Shard 只有一个有效 Placement。
- 一个 Room 只有一个 Master role。
- 端口必须在对应网络作用域内唯一，不能简单按 Node 或 Room 判断。
- Shard 的目录名、`server.ini` 中的 `id` 和产品 ID 分开保存，不能互相猜测。
- 裸机/容器 Placement 固定到 Node；Kubernetes Placement 默认可以交给 scheduler，也可通过 node selector/affinity 限定节点池。只有高级模式允许固定 Worker Node。

### 2.6 ProcessObservation

```text
instanceId, nodeId, environmentId, installationId, roomId, shardId
nativePid, containerId, podUid, startedAt, executable, buildId
runtimeState, exitSource, cpuPercent, rssBytes
observedAt, stale
```

native Driver 通过进程参数中的 `-cluster`、`-shard` 和 `-persistent_storage_root` 识别归属；container/kubernetes Driver 还校验受管 label、容器/Pod 身份和挂载。无法唯一归属的实例进入诊断列表，不能自动绑定到房间。

## 3. 状态新鲜度

所有远程观察对象必须返回：

- `observedAt`：Agent 实际采集时间。
- `receivedAt`：控制器收到时间。
- `stale`：是否超过该数据类型的新鲜度窗口。
- `staleReason`：`agent_offline`、`report_expired`、`clock_skew` 或 `unknown`。

建议窗口：

- Agent 心跳：90 秒。
- 进程和节点容量：90 秒。
- 玩家与世界实时状态：45 秒。
- 安装/房间/配置清单：5 分钟。

前端不得把 stale 的最后快照渲染成实时状态；可以保留数值，但必须显示“最后观测于 …”。

## 4. CPU 容量模型

### 4.1 计算

```text
effectivePhysicalCores = reportedPhysicalCores
  or max(1, floor(logicalProcessors / 2)) when unknown

recommendedShardLimit = max(1, effectivePhysicalCores - reservedCores)
projectedShardCount = observedRunningShards + shardsInStartPlan
```

默认 `reservedCores=1`，允许管理员按节点调整。容量状态：

- `available`：`projectedShardCount < recommendedShardLimit`
- `full`：`projectedShardCount == recommendedShardLimit`
- `overcommitted`：`projectedShardCount > recommendedShardLimit`
- `unknown`：CPU 或进程观察数据过期

### 4.2 产品提示

节点页固定展示：

```text
世界进程 2 / 建议 7
8 个物理核心，16 个逻辑处理器，已为系统预留 1 核
```

启动预检提示：

```text
启动后该节点将运行 8 个世界分片，超过建议上限 7。
DST 每层世界是独立进程；一核心承载多层世界可能造成 tick 延迟、网络抖动和卡顿。
```

物理核心为估算值时明确显示“估算”，不伪装成精确硬件信息。

容量提示的验收条件：同一节点放置一个房间的地表与洞穴时显示 2 个计划 Shard；再放置另一个房间的地表时显示 3 个，而不是 2 个房间。超过建议上限仍允许管理员显式确认继续，但不得静默启动，也不得把建议值实现成不可绕过的硬限制。

### 4.3 CPU 执行策略

容量建议与 CPU 绑核是两套能力。默认策略为 `none`，不自动绑核：

| 策略 | 行为 | 适用范围 |
| --- | --- | --- |
| `none` | 只做“一核一层”容量提醒，由系统调度 | 全平台默认 |
| `reserved` | 声明 CPU request/quota，限制或保障份额，但不声称独占核心 | Linux cgroup、Docker、Kubernetes |
| `exclusive` | 将 Shard 分配到不重叠的物理核心集合 | 能证明支持 cpuset 的 Linux、Docker 和特定 K8s 集群 |

规则：

- RuntimeProvider/Agent 必须上报物理核心、逻辑 CPU、SMT sibling、NUMA，以及无法识别时的原因；不能把两个超线程当成两个独立物理核心预算。
- 默认从可分配集合排除至少一个物理核心，留给 OS、Agent、SteamCMD、压缩和备份。
- `exclusive` 分配必须持久化，启动前检查交叠；重启、BIOS 拓扑变化或容器迁移后重新校验，失效时阻止自动启动并要求重新分配。
- Linux 裸机优先使用 cgroup v2/cpuset；单独使用 `taskset` 只能作为能力受限的兼容实现。
- Docker/Podman 的 CPU quota 与 `cpuset-cpus` 分开展示；只有后者且核心拓扑已校验时才显示“独占绑核”。
- macOS 没有可依赖的通用 cpuset 接口，默认只提供 `none` 和容量建议，不能伪装成支持独占绑核。
- Kubernetes 只有在 CPU Manager 使用 `static`、容器属于 Guaranteed Pod 且 CPU request 为整数时，才允许选择 `exclusive`；否则降为 `reserved` 或显示“不支持”，不能静默改变语义。
- 同一 Node 上的 native、host-network 和 bridge container Shard 共同占用宿主 CPU 预算，不能按环境各算一份可用核心。
- Kubernetes 普通调度以 Worker `allocatable` 和 requests 为准；独占模式还必须读取 kubelet 实际 cpuset。产品的“一核一层”仍是额外的 DST 预警，不替代 Kubernetes admission。

当前运行链路在启动和重启前准备 CPU 策略，并在实例就绪后回读实际结果；策略应用失败会停止新实例。停止、回滚或异常恢复只有收到 `stopped` 且 `sessionExists=false` 的明确状态后才释放 cgroup/cpuset，超时、空结果和 `unknown` 都保留约束并记录失败。控制器重启时先恢复本地分配；远程分配等待 Agent 在线且清单新鲜后逐项观测，每项使用独立超时，观测写回通过目标、策略、CPU 集合和更新时间 CAS，不能覆盖并发修改。

CPU 设置需要同时展示“请求/上限”“实际可用 CPU 集合”和“最近校验时间”。绑核不能替代负载监控：大型 Mod 或高玩家 Shard 仍可能需要超过一个核心预算，并可能被 quota 节流。

## 5. 网络与端口模型

### 5.1 DST 端点

每个 Room/Shard 必须显式管理四类 UDP 端口：

| 端点 | 配置来源 | 所有者与用途 |
| --- | --- | --- |
| Shard 通信 | `cluster.ini: master_port` | Master 监听，Secondary 连接 Master |
| 玩家流量 | `server.ini: server_port` | 每个 Shard 的玩家连接 |
| Steam 鉴权 | `server.ini: authentication_port` | 每个 Shard 的 Steam 鉴权流量 |
| Steam 列表 | `server.ini: master_server_port` | 每个 Shard 的 Steam Master Server 流量 |

不能只保存一个“分片 IP 和端口”。目标 `NetworkProfile` 至少包含：

```text
bindAddress, advertiseAddress
internalAddress, internalPort
publishedAddress, publishedPort
protocol(udp), networkScopeType, networkScopeId
allocationMode(manual|automatic), observedAt
```

`bindAddress` 是进程监听地址，`advertiseAddress/publishedAddress` 是其他 Shard 或玩家实际可达地址。跨节点 Secondary 的 `master_ip` 禁止使用 `127.0.0.1`；容器或 Pod 之间也不能把各自的 loopback 当成 Master 地址。

### 5.2 端口租约与冲突范围

端口由 `PortReservation` 管理，状态为 `planned -> active -> releasing -> released`，并记录 Room、Shard、端点类型、地址、协议、网络作用域和拓扑版本。自动分配和手动输入都必须先创建租约，避免两个并发计划选到同一端口。

冲突规则：

- 裸机与容器 `host` 网络：按 `node + bindAddress + port + protocol` 唯一；通配地址与具体地址的重叠也算冲突。
- Docker/Podman bridge：不同容器的内部端口可重复；映射到宿主的 UDP 端口必须在宿主作用域唯一。
- Kubernetes Pod 网络：不同 Pod 的容器端口可重复；`hostPort` 按 Worker Node 唯一，`NodePort` 按 Kubernetes Cluster 唯一，独立 ClusterIP Service 可复用相同 Service port。
- Master 的 `master_port` 只由 Master 环境声明监听租约；Secondary 保存目标引用，不重复占用 Master 的监听端口。

端口转发“配置成功”不等于 DST 可被正确发现。Docker bridge 默认让 `publishedPort == server_port`；Kubernetes NodePort/LoadBalancer 如果改变玩家外部端口，必须通过 Steam 列表与公网连接验证，未验证前标为实验网络配置。Master 端点优先使用稳定 Service ClusterIP；Kubernetes DNS 名是否可直接写入 `master_ip` 也必须实机验证。

存档导入使用自动端口时，apply journal 与端口 lease ID 在一次数据库更新中持久化。发布前或激活失败后的清理顺序固定为文件回滚、端口释放、清 journal；任一步失败都会保留 `applying` 状态和 lease 引用，服务重启后继续恢复，不能先清记录再留下无归属的 active lease。

启动预检必须同时检查：租约冲突、实际 UDP 监听冲突、Master 地址可达性、Cluster key/身份、Shard ID 唯一性、防火墙或 NetworkPolicy、端口观察数据是否过期。UDP “未探测到响应”不能单独证明端口空闲，必须结合操作系统监听表、受管进程和租约登记。

## 6. Agent 协议

协议版本从能力协商开始。每条操作具有：

```text
operationId, idempotencyKey, action
providerId, nodeId, environmentId, installationId, roomId, shardId
topologyVersion, leaseId, fencingToken
preconditions, parameters, deadline
```

首批只读能力：

- `node.inventory.read`
- `runtime.installations.read`
- `runtime.shards.read`
- `runtime.processes.read`
- `runtime.capacity.read`

已实现的单分片控制能力：

- `shard.status`、`shard.start`、`shard.stop`、`shard.restart`、`shard.save`

后续房间级聚合能力：

- `room.start`、`room.stop`、`room.restart`、`room.save`
- 跨 Shard 的完整备份集编排与集中存储策略
- 跨节点 Mod 预检、协调重启和加载日志确认

Agent 的 Mod Runtime capability 为 `runtime.mods.v1`，包括缓存检查、cache bundle 分块上传、release plan 分块上传、`prepare/publish/rollback/complete/state` 以及 `modoverrides.lua` 分块读取。单条数据块上限 256 KiB；所有 mutation 继续经过 lease、fencing 和持久化幂等。

控制面已经在该执行边界上完成跨节点编排：一次预览使用同一不可变快照读取全部 Placement 与配置；发布前再次计算 `planHash` 和拓扑版本；共享同一 Installation 的其他房间会被纳入目标并先创建 cold-consistent 保护备份。所有节点 Prepare 成功后才进入 Publish，全部 Publish 成功后先持久化 commit decision 再清理；commit 前失败逆序回滚，commit 后中断进入 `recovery_required` 并由持久恢复任务继续完成。需要重启时按策略协调运行中分片，等待新实例日志确认加载；失败保留可恢复状态和逐目标证据。

Agent 不接受任意 Shell 字符串。路径必须落入已登记 RuntimeInstallation 的允许根目录。

Agent 主机在自己的 `app.conf` 中登记受信安装，控制中心只能引用 `INSTALLATION_ID`，不能随操作覆盖路径：

```ini
[runtime]
INSTALLATION_ID = default
SAVE_PATH = /srv/dst/.klei/DoNotStarveTogether
SERVER_PATH = /srv/dst/server
UGC_PATH = /srv/dst/server/ugc_mods
WORKSHOP_CONTENT_PATH = /srv/dst/server/ugc_mods/content/322330
MOD_CACHE_PATH = /var/lib/dst-admin-agent/mod-cache
MOD_STATE_PATH = /var/lib/dst-admin-agent/mod-state
SERVER_MODE = 64

[runtime.secondary]
SAVE_PATH = /srv/dst-secondary/.klei/DoNotStarveTogether
SERVER_PATH = /srv/dst-secondary/server
WORKSHOP_CONTENT_PATH = /srv/dst-secondary/workshop
MOD_CACHE_PATH = /var/lib/dst-admin-agent/secondary-mod-cache
MOD_STATE_PATH = /var/lib/dst-admin-agent/secondary-mod-state
SERVER_MODE = 64
```

`[runtime]` 默认 ID 为 `default`，额外安装使用 `[runtime.<id>]`。`WORKSHOP_CONTENT_PATH` 是受信 Workshop 内容根；`MOD_CACHE_PATH` 保存不可变 tree-SHA cache；`MOD_STATE_PATH` 保存上传断点、发布计划和 journal。cache/state 必须绝对、持久、由 Agent 私有写入，且不能相同或互相嵌套。Agent 在启动、停止、重启或保存前重新检查 Cluster 与 Shard 名称、真实路径、`cluster.ini`、`server.ini` 和服务端路径；符号链接不能越出对应的受信 Cluster。Agent 将最高 fencing token 和最近的幂等结果持久化到私有状态文件。若进程在接收操作后、记录结果前中断，重复请求返回 `unknown`，不会盲目再次执行。

### 6.1 Runtime Driver

房间服务只调用统一的类型化 Runtime Driver，不直接依赖 tmux、Docker CLI 或 Kubernetes API：

```text
discover, status, start, stop, restart, save
sendConsole, consoleHealth, observeOperation
inventory, streamLogs, readArtifacts
backupStage, health
```

现有 `runtimedriver.Driver` 保持生命周期、控制台、迁移和备份契约；Mod 分发没有强迫所有 Provider 扩张该接口，而是使用独立 `runtimedriver.ModDriver`。Agent adapter 同时实现两者，Mod Driver 暴露 cache inspect/upload、release plan upload、prepare/publish/rollback/complete/state 和 overrides continuation。Kubernetes 实验 Provider 尚未实现 `ModDriver`。

- `native` Driver：当前 tmux 实现先迁入该边界，未来可增加 systemd/launchd profile。
- `container` Driver：通过 Docker/Podman API 管理带受管标签的容器和固定控制台 transport，不向请求开放任意镜像、挂载或宿主命令。
- `kubernetes` 实验 Provider：当前只通过受限 ServiceAccount 读取指定 namespace/label 范围内的 StatefulSet、Pod、PVC、Service 和 NetworkPolicy，并生成类型化预检计划；不提供 Apply、生命周期或 Console。

`sendConsole` 是必需能力，不是附加功能。保存、优雅停止、玩家管理、命令目录、自动化和 `customcommands.lua` 激活/诊断都依赖它。请求至少包含 `operationId`、`operationKind`、目标 Shard instance ID、受限 payload、deadline、lease 和 fencing token。Driver 返回的阶段统一为 `accepted/sent/confirmed/failed/unknown`，但 `confirmed` 必须引用与操作类型匹配的完成证据，不能只表示 tmux 或容器 API 调用成功。

| 操作类型 | 完成证据 | 超时语义 |
| --- | --- | --- |
| 启动 | 新 instance 的世界加载与 Shard 注册 ready observation | `failed` 或 `unknown` |
| 停止 | 目标 instance 进程退出；marker 只作辅助 | `unknown`，随后才允许强制终止 |
| 保存/热备份 | 全部必需 Shard 到达同一可验证 snapshot 屏障 | 不能生成 complete 备份 |
| `customcommands.lua` 受管动作 | session、shard、request ID 匹配的结构化文件回执 | `unknown`，危险动作不重放 |
| 玩家/世界状态探针 | nonce 匹配且完整的结果批次 | 失败并保留旧数据为 stale |
| 任意原始 Lua | 默认最高只到 `sent` | 显示“已发送，执行结果未确认” |

原始 Lua 不强制包装进 `loadstring/xpcall`，因为包装会改变语法环境、错误传播、`return` 和部分 Mod 命令的行为，与完整兼容目标冲突。需要可靠执行结果的产品功能必须进入 `customcommands.lua` 受管 allowlist；原始 Lua 是高风险兼容通道，不自动重放，也不伪装成已执行。

控制台仍只完成了部分操作证据：原始 Lua 写入 transport 后最高为 `sent`，不能当作游戏逻辑已执行。类型化 Shard migration、backup 与 Mod Runtime 已有独立协议和持久化步骤，但房间级一致性仍必须由上层协调，不能把一个通用 `ConfirmCommand` 当成所有操作的完成条件。

Agent 内建立唯一的 per-Shard console dispatcher。命令页、自动化、备份、玩家/世界探针、Runtime Bridge 和生命周期都必须使用同一个 dispatcher，禁止各模块自建互不相知的锁。dispatcher 以稳定的 `provider/environment/installation/room/shard` 为键，每个队列项再绑定 instance ID，避免重启边界出现两套并发 dispatcher。它提供有界排队、过期丢弃和生命周期门禁：停止开始后拒绝普通命令和探针；低优先级重复探针可以合并；已经写入 transport 后即使调用方取消也返回 `sent/unknown`，不能假装撤销。每次发送前后重验 instance 与 fencing，变化时不得把 payload 送到新进程。Agent 在写入前持久化 in-flight 状态，重启后未完成项恢复为 `unknown/input_dirty` 而不是重新发送。

控制台 transport：

- native Runtime 继续使用 tmux，但每个 RuntimeInstallation 使用 Agent 私有 `0700` 短路径运行目录中的哈希命名独立 tmux socket；session identity 同时绑定 environment、installation、Room 和 Shard，避免同宿主同名 Cluster 冲突和 Unix socket 路径超长。启动时记录固定 pane ID，发送前校验 pane 未 dead、当前进程属于目标 DST instance，不能只向当前活动 pane 发送。
- container Runtime 初始使用 `tmux-compat`：tmux 位于 DST Shard 容器内，仅作为 DST stdin/会话代理；Agent 与 DST 仍是不同容器。Agent 通过 container Driver 执行固定 tmux 客户端动作，不开放任意 `docker exec`。
- Docker Engine attach/stdin 是后续候选：容器必须配置 `OpenStdin=true`、`StdinOnce=false`、`Tty=false`，Driver 只 attach stdin。它通过重连、并发、Engine/Agent 重启、高日志量和命令 marker 验证后才能成为默认 transport。
- `docker exec <lua>` 不能直接替代控制台输入，因为 exec 创建的是新进程，不会把 Lua 写入正在运行的 DST 主进程。exec 只有在调用受管 tmux 客户端或未来固定 console client 时才合法。
- Kubernetes 也必须提供等价 console transport；不能把 Pod Running 或 `kubectl exec` 可用误认为 DST 控制台可用。

tmux transport 必须使用 literal payload、固定 pane、明确最大字节数和单行策略，不能把 Lua 拼进宿主 Shell。payload 已写入但 Enter 未确认时将控制台标为 `input_dirty`，在显式清理或重建 instance 前阻止后续命令，避免两条 Lua 被意外拼接。`consoleHealth` 区分 `ready/disabled/not_found/socket_unavailable/permission_denied/pane_dead/process_mismatch/input_dirty`；`cluster.ini` 关闭控制台时不得报告 ready。

保留服主本机排障能力，但不开放任意远程 Shell：只读 tmux attach 可以与 dispatcher 共存；可写 attach 必须通过 Agent CLI 取得有超时的 `console-maintenance` 租约，暂停自动命令和探针并在 UI 展示操作者/开始时间。发现同 UID 用户绕过 CLI 建立未知可写 client 时，console health 进入 `external_writer` 并阻止自动发送。维护结束后重新校验 pane、instance 和输入为空，才能恢复 dispatcher。

`streamLogs/readArtifacts` 接受受信的 artifact 类型、offset/generation 和大小限制，不接受控制器传入任意远程路径。日志、`customcommands.lua` 回执、诊断和 snapshot manifest 都由所在 Runtime 的 Driver/Agent 读取；主服务不能继续拿全局 `savePath` 去读取远程 Shard。

Agent 与 DST 不打进同一容器。`tmux-compat` session/socket 属于 Shard Runtime；容器 Driver 通过固定 `docker exec ... tmux` 参数访问它，不能把 Agent 镜像里是否存在 tmux 客户端误解为两者共享会话。Compose 只把独立 `dst-mods` volume 以 Agent 读写、DST 只读方式挂到各自安装路径的 `mods` 子目录，服务端安装卷的其余部分对两者保持只读；Agent identity/cache/state 只在 `agent-data`。Driver 返回统一状态，但保留 tmux socket/session/pane、PID/start identity、container ID、Pod UID、restart count、console transport 和退出原因，避免把不同平台故障压扁成一个 `stopped`。审计默认保存操作类型、模板/参数摘要、payload hash、长度和证据引用；敏感参数与完整原始 Lua 不写普通服务日志。Web UI 只提供结构化命令和日志流，不提供宿主终端。

## 7. 生命周期协调

房间级操作生成父计划和逐 Shard 子步骤：

```text
resolve topology -> acquire room lease -> preflight all nodes
-> dispatch typed shard steps -> wait for observations
-> classify complete/partial/unknown -> release lease
```

安全规则：

- 网络断开时不把目标标为 stopped。
- 未取得更高 fencing token，不能在另一节点启动同一 Shard。
- Master 操作必须显示影响范围。
- 单 Shard 操作和房间操作使用同一个房间租约，避免交叉执行。
- 重复请求返回原 operation，而不是再次执行。

## 8. 分布式备份集

完整备份集由 Cluster 公共快照和每个必需 Shard 的子快照组成：

```text
save barrier -> every shard acknowledges snapshot
-> local immutable staging -> manifest/hash
-> optional central upload -> verify -> commit backup set
```

只有所有必需子快照具备相同 Cluster 保存点语义并通过校验时，备份集才是 `complete`。部分成功只能标为 `partial`，不得用于默认一键恢复。

备份提供两种明确模式：

- `cold-consistent`：协调停止整个 Room、确认所有 instance 不再写盘、校验 Session/snapshot 后 staging；这是无法证明热保存屏障时的安全 fallback，可按用户选择在备份后恢复原运行状态。
- `hot-consistent`：Runtime 2.4.0 在每个运行分片准备 snapshot 屏障并记录 session、Shard、producer instance 和初始 snapshot；随后只由 Master 触发一次 `ms_save`。每个分片必须从实际 `ShardGameIndex.SaveCurrent` 回调返回前进后的相同 snapshot，控制面再次校验拓扑、目标和 Runtime 实例未变化后才允许 staging。证据不足、超时、拓扑变化或 Runtime 重启均使整套备份失败，不会降级为普通在线打包。屏障持有期间到达的额外保存或关服保存会延迟到释放后执行。

恢复顺序：停止整房间、验证目标拓扑和容量、创建恢复前保护备份、分发全部子快照、校验、启动并确认 Shard 注册。单分片恢复只作为高级实验能力。

每次备份创建和存档恢复都写入持久操作记录，包含阶段、拓扑 revision、lease/fencing、原运行世界、保护备份和失败原因。控制器启动后及运行期间会恢复 `running/recovery_required` 操作；正式 API 同时提供按房间读取操作历史和对单个操作立即重试。恢复已发布的存档时只继续完成清理和原运行状态恢复，发布前中断则回滚；重复恢复终态操作不会再次发布存档。前端把“Job 已结束”和“持久操作已完成”分开显示，无法读取操作记录时不会误报恢复成功。

存储边界按部署形态实现，但备份 manifest 保持一致：

- 裸机：受信 `saveRoot` 内建立只读 staging，再上传或复制。
- 容器：Shard 存档、备份 staging 和运行安装使用显式 volume；禁止依赖容器可写层保存数据。
- Kubernetes：每个 Shard 使用独立 PVC 或经过验证的等价持久卷；默认 `Retain` 数据语义。完整备份仍需游戏保存屏障，CSI VolumeSnapshot 只能替代复制步骤，不能替代一致性协调。

Agent 平台状态与房间数据分开备份：runtime state、Agent ID、fencing/幂等记录和 `MOD_STATE_PATH` 属于节点控制状态；Shard saves 属于房间数据；`MOD_CACHE_PATH`、Workshop 内容和 DST 安装可重建，但精确离线回滚依赖保留的 cache。容器部署不得把任何一类状态留在容器可写层。

不得让两个 Shard 容器/Pod 无约束地同时写同一个 Cluster 根目录。Cluster 公共配置生成后分别下发，Shard 私有存档独立挂载，再由备份集在逻辑上合并。

首次把本机受管房间规划到远程 Agent 时，控制面可以投放创建远程 Cluster/Shard 配置。该流程不是任意文件同步：共享文件仅允许 `cluster.ini`、`cluster_token.txt`、三类名单，分片文件仅允许 `server.ini`、世界生成/覆盖、Mod 覆盖和受管 `customcommands.lua`；目标路径只能由 Agent 受信 RuntimeInstallation 和 Cluster/Shard 标识解析。投放使用分块 SHA 校验、目标原子发布、持久 commit decision 与整体回滚；步骤以 `not_started`、`planned`、`uploading` 区分确定未派发、Begin 结果不确定和 Begin 已确认，恢复器只对可能已触达远端的步骤调用 Agent 回滚；已有远程分片换节点仍走带源恢复点的 Placement migration。

## 9. Mod 发布

同一个 Mod 可以在不同房间和不同 Shard 使用不同配置；Workshop 文件则需要存在于每个承载相关 Shard 的节点。

发布步骤：

```text
resolve desired mod lock
-> prepare exact item revision on every target node
-> verify file/version/hash
-> publish each shard modoverrides.lua
-> coordinated restart when required
-> verify load logs
```

任何目标准备失败时，默认不发布配置。UI 分开展示“已下载到节点”和“已在 Shard 启用”。

当前 Agent 执行器把每个 installation 的发布作为一个 fencing/idempotency 域：cache bundle 与 release plan 只通过 256 KiB 分块传输，每个 begin、write offset 和 commit 使用独立幂等身份但共享同一有效租约与 fencing token。Agent 落盘并验证 SHA、tree/manifest、UTF-8/单 JSON 值、tar 路径/文件类型和磁盘空间后才导入。控制面已实现“全部目标 prepare 后再 publish、commit decision 持久化、commit 前失败逆序 rollback、commit 后只向前恢复”的房间级协调器，发布依次进入 `planned -> prepared -> published -> committed`，中断 journal 在 Manager 初始化时恢复或回滚。运行中的目标在策略允许时自动协调重启，并以新实例加载日志确认 Mod 生效；任一确认失败都会保留可诊断、可恢复的 Publication 状态，而不是只返回一个布尔提示。

## 10. 看板与控制范围

集中总览以表格和异常队列为主，支持按 Node、Room、Shard 三个维度筛选。

房间看板展示每个 Shard 的节点、执行环境、内外端点、CPU 策略、进程、玩家、Mod、版本、资源和数据时间。节点看板展示硬件容量、运行分片、执行环境、安装实例、端口租约、磁盘和任务。

操作范围必须显式：

- 当前 Shard
- 当前 Room 全部分片
- 当前 Node 上选中的分片
- 用户勾选的多个 Room/Shard

执行确认页展示每个节点启动前/后的 Shard 数和 CPU 建议上限。

跨房间批量操作已在房间拓扑页交付：用户选择启动、停止、重启或保存后，可按房间勾选任意世界分片，并看到每层世界当前生效的本机或 Agent 节点。后端按节点合并统计所有房间的运行中和待启动 Shard；同机多层世界合法，但超过“一颗物理核心最多一层世界并额外预留 1 核”的建议值时必须再次确认。批量 Job 即使全部失败也保留逐世界结果，恢复入口会刷新拓扑和运行状态，并只重试仍可操作的未成功项。

## 11. 部署形态

### 11.1 主服务部署

主服务提供 Web/API、拓扑、Job、审计和数据库，不直接等同于运行节点：

- 裸机模式保持现有本地优先行为，可以使用内置 `local` Provider。
- 容器模式使用非 root OCI 镜像，只挂载主服务配置、密钥引用和持久数据卷；SQLite 阶段固定单副本并持久化数据库/WAL，不能把数据库放在容器可写层。
- 主服务容器通过明确的 HTTP/WebSocket 地址对 Agent 提供连接。Agent 从宿主或其他容器主动连接，不能把各自的 `127.0.0.1` 当成对方地址。
- 主服务容器默认关闭内置宿主 Runtime 管理。若要管理同一宿主的裸机 DST，推荐在宿主安装 Agent，而不是给主服务容器增加 host PID、DST 路径或 Docker Socket。
- 反向代理必须支持 WebSocket、请求 ID、真实来源地址和长任务状态连接；健康检查区分进程存活、数据库可写和迁移完成。

### 11.2 Agent 部署

裸机 Agent 是管理裸机 DST 的推荐方式：

- Linux 由 systemd 托管，macOS 由 launchd 托管；现有 tmux 只作为 Shard Runtime Driver，不承担 Agent 保活。
- 路径、用户、文件权限和端口均在 Agent 本机预注册，主服务只能引用 ID。
- Agent 主动建立 WebSocket，不要求对公网开放 Agent 入站端口。

容器 Agent 必须持久化身份、配置、密钥和 operation/fencing state，并根据目标 Runtime 选择权限 profile：

- `container-runtime`：管理容器化 Shard。推荐使用 rootless Podman 或受限 Docker Socket Proxy；只允许受管 label、镜像、网络和挂载，不把原始 Docker Socket 暴露给主服务。
- `native-host-integration`：仅是目标设计，仓库当前没有该容器 profile。管理宿主裸机 Shard 需要同 UID/GID、受信路径 bind mount、tmux socket/运行目录、宿主进程可见性和持久 Agent state，相当于授予较高宿主权限；完成 Linux 实机验证前统一使用裸机 Agent，macOS Docker Desktop 不作为该模式的目标。
- capability 必须来自实测环境。未取得宿主进程/tmux 能力时，容器 Agent 可保持只读或只管理容器 Runtime，不能报告 native 控制可用。

Agent 容器重建后必须保留同一 Agent ID 和最高 fencing token；状态卷丢失时进入 `identity_lost/operation_state_unknown`，禁止直接接管原 Shard。

### 11.3 DST Runtime 部署

- `native`：DST 直接运行在 Linux/macOS，继续由 native/tmux Driver 控制。CPU `exclusive` 仅 Linux 探测通过时开放；macOS 保持建议模式。
- `container`：推荐一个 Shard 一个容器，使用非 root 用户、只读基础镜像、明确的存档/Mod/日志 volume 和 graceful stop timeout。首版在 Shard 容器内保留 `tmux-compat` 控制台代理；这不改变 Agent 独立部署，也不允许一个容器承载多个 Shard。
- Shard 容器以轻量 init 加固定 runtime supervisor 作为 PID 1。supervisor 创建安装/分片专用 tmux socket 与 pane、等待 DST 退出、传播真实退出原因并处理 SIGTERM；不得用 `sleep infinity` 充当生命周期。tmux socket 和临时控制状态放 `/run` tmpfs，不进入持久卷。
- container bridge 与 host 网络都可作为 profile，但必须展示真实 UDP 对外映射；跨机器 Master 使用可路由地址。
- DST 二进制采用“受控可变安装卷”或“不可变版本镜像”二选一，同一发布计划不得静默混用。
- 内存默认只预检和告警。启用硬限制后，Driver 必须把 OOM kill 与普通退出分开审计，并为备份压缩和临时文件保留资源。
- Shard 容器默认由 Agent/控制器恢复期望状态，不使用会绕过 lease/fencing 的 `always` 或 `unless-stopped`。未来可选的节点本地恢复模式只允许无共享存储的单节点 Room，并要求持久化 desired state、instance identity 和 fencing 后单独验收。

CPU 绑核和 DST 四类 UDP 端口都附着在 Shard Runtime/Node 上，不附着在主服务或 Agent 容器上。主服务和 Agent 只计入系统预留资源。

容器停止顺序必须是：`sendConsole(c_shutdown(true)) -> 等待目标进程退出 -> container stop/supervisor exit -> 超时后强制终止`。marker 只作诊断，进程退出才是停止完成证据。若 Agent 未能先协调，PID 1 supervisor 收到 SIGTERM 且 console health 为 ready 时执行同一有界优雅关服；控制台 dirty/unavailable 时进入信号 fallback 并明确记录未保存风险，不能把 Lua 拼到未知输入后。SIGKILL 始终记为未保存风险。热备份先验证所有目标 `consoleHealth=ready`；任一 Shard 控制台或保存证据不可用时改用 cold fallback 或生成非 complete 结果。

### 11.4 Kubernetes（只读实验能力）

当前控制面始终注册 Provider 状态、observe 和 preflight 接口，功能默认关闭；启用并提供受信配置后也只能观察和生成类型化计划，`applyAllowed=false`，前端固定展示“实验能力”。以下内容仍是生产 Driver 的目标设计：

- 一个 Shard 一个有状态工作负载，副本数固定为 1，使用稳定身份和独立 PVC；不把多个 Shard 塞入同一个 Pod。
- Master Shard 通信优先通过稳定 Service ClusterIP；DNS 写入 `master_ip` 需验证后开放。玩家 UDP、Steam 端口使用明确的 hostPort、NodePort 或 LoadBalancer profile，不能由前端猜测可达地址，也不能假设端口转换后 Steam 会公布正确外部端点。
- Token、cluster key 放 Secret；非敏感生成配置放 ConfigMap；存档和 Workshop 内容放 PVC/受管缓存，不放 ConfigMap 或容器可写层。
- Readiness 检查 DST 已完成世界加载和 Shard 注册；仅检测进程存在不足以判定可用。Liveness 自动重启默认保守，避免加载慢或保存期间被误杀。
- K8s 自动重调度前必须验证 Room lease、fencing token、旧 Pod UID 终止和存储所有权。不能只因节点 `NotReady` 就在另一节点复制启动同一 Shard。
- 最小权限 RBAC 仅覆盖指定 namespace/label；NetworkPolicy 只放行控制、Master/Secondary、玩家 UDP 和必要 Steam 出站。
- 一致性备份使用保存屏障后协调各 PVC snapshot/上传；单个 PVC 快照不能宣称为完整 Room 备份。
- K8s Placement 默认由 scheduler 在允许节点池中选择，页面显示期望节点池和实际 Worker；“停止某台服务器上的分片”只操作当前实际位于该 Worker 的受管 Shard，不等同于 drain、关机或迁移。
- 主服务可以作为单副本 Deployment + PVC 运行；需要控制 Pod Shard 时由 Kubernetes Provider 使用受限 ServiceAccount。只有需要宿主级清单或 native Runtime 控制时才部署 Agent DaemonSet，不能默认给 DaemonSet 特权。
- Pod Runtime 必须声明 console transport；初期可沿用 Shard 容器内 `tmux-compat`，或在 Kubernetes attach/stdin 通过同一等价矩阵后切换。Pod Running 但 console 不健康时，保存、备份和命令操作必须阻止。

### 11.5 配置体验

默认流程面向个人服主，不要求理解容器网络或 Kubernetes：

1. 安装向导先选择主服务部署方式，再登记 Agent；不会把“主服务使用 Docker”自动推导为“DST 使用 Docker”。
2. 新增运行目标时选择“本机”“远程服务器”“容器 Runtime”或“Kubernetes 集群（实验）”，并显示 Agent 的实际 capability。
3. 创建/放置 Shard 时选择 DST Runtime；系统自动生成不冲突的推荐端口和 `none` CPU 策略。
4. “网络高级设置”才展示 bind、对外地址、端口映射和网络作用域；跨节点时必须确认 Master 实际可达地址。
5. “资源高级设置”展示建议容量、CPU request/quota 和独占绑核；独占能力不可用时直接解释原因。
6. 执行前预览按 Node 汇总启动后的所有 Shard，包括其他 Room 和不同执行环境，并显示会操作的具体范围。

商家或多机用户可以保存 Network/CPU/Storage profile 批量复用，但 profile 只保存期望参数，落到目标环境后仍需重新预检，不能把一台机器验证过的端口或 cpuset 原样套到另一台机器。

## 12. 迁移顺序

1. 保留本机默认路径，增加只读 Node/Inventory/Capacity API。
2. 让 Agent 上报类型化 Runtime inventory，控制器持久化观察快照。
3. 建立 Placement 和拓扑版本，但暂不迁移现有本地 Room ID。
4. 把单 Shard 本地控制适配到统一 typed operation，再接入远程 Agent。
5. 增加房间租约、幂等和 fencing 后开放房间级远程操作。
6. 拆分主服务、Provider/Agent 和 DST Runtime 三层 deployment profile；引入 `ExecutionEnvironment`、Runtime Driver、NetworkProfile、PortReservation 和 CPU policy。
7. 先建立统一 console dispatcher、按操作完成证据、安装级 tmux socket/pane identity 和类型化 Artifact API；迁移所有直接 tmux/savePath 旁路。
8. 已交付“主服务容器 + 裸机 Agent + native DST”的部署基线；完整功能等价矩阵继续回归。
9. 已交付容器 Agent 持久状态和带 PID 1 supervisor 的 Docker Shard Runtime 基线；Podman 与 native host-integration 尚未完成实机验证。
10. 已交付 cold-consistent 跨 Shard 备份集、跨节点 Mod 原子发布、自动协调重启和加载日志确认；hot backup 仍需可证明的跨分片保存屏障。
11. 已交付玩家、日志、世界状态和诊断的 Placement 聚合与来源/新鲜度模型。
12. 已交付独立 Kubernetes 类型化安全内核、只读 REST adapter、status/observe/preflight API、OpenAPI、RBAC 和实验 UI；仍需 Apply、lease-aware supervisor、Console、Mod、备份恢复，并通过存储、网络、CPU Manager 和故障注入矩阵后才能标记生产可用。

任何阶段都不得让远程选择回退到本地执行。旧 Agent 缺少能力时保持只读或显示升级要求。
