# 多节点房间集中管理设计

> 状态：Phase 1-4 实现基线 + 后续平台扩展设计
> 更新日期：2026-08-15
> 依赖：`distributed-room-management-plan.md`、`multi-node-dst-research.md`

实现状态说明：当前代码只支持 `native` 执行环境，本机和 Agent 都通过受信路径及 tmux 控制 DST；已经采集四类 DST 端口和 CPU 容量，但尚未实现执行环境、端口租约、CPU 绑核、容器 Driver 或 Kubernetes Driver。本文后续对应能力均为目标设计，不代表已经可用。

## 1. 产品边界

系统默认管理控制器本机。只有用户显式添加并配置 Agent 后，远程节点才进入管理范围。

首期不提供任意远程 Shell，也不把控制器本地路径直接套用到远程节点。所有远程能力都通过带版本的类型化协议完成，并由 Agent 对目标路径和资源再次校验。

支持的拓扑包括：

- 一个节点运行一个房间的一个 Shard。
- 一个节点运行同一房间的多个 Shard。
- 一个节点运行多个房间的多个 Shard。
- 一个房间的不同 Shard 分布在多个节点。

目标部署形态：

- 裸机：Linux/macOS 直接运行 DST，本机或 Agent 使用原生进程 Driver。
- 容器：Linux 节点使用 Docker 或 Podman；推荐一个容器承载一个 Shard，不在容器内运行 tmux。
- Kubernetes：未来由 Kubernetes Driver 管理命名空间内的有状态 Shard 工作负载；推荐一个 Pod 承载一个 Shard。
- 混合拓扑：同一 Room 可以把 Shard 放在不同部署形态，但只有各目标的版本、网络、存储和 Mod 预检全部通过后才允许启动。

控制器的部署位置和 Shard 的执行形态相互独立。控制器可以运行在裸机或容器中，但不得因为自身位于容器中就默认取得宿主机、Docker Socket 或 Kubernetes 集群权限。

同机多 Shard 是合法能力，但默认容量策略为“每个运行中的 Shard 预留一个物理核心预算单位，并给系统至少预留一个核心”。

这里的 Shard 统计跨房间累计：同一台服务器既可以承载一个房间的多层世界，也可以承载多个房间的多个世界。界面必须明确提醒用户“一核心最多规划一层运行中的世界”，超过时提示可能卡顿；该规则是保守容量建议，不是硬限制，也不是性能保证。

## 2. 数据模型

### 2.1 RuntimeProvider

```text
id, kind(local|agent|kubernetes)
displayName, connectionState, credentialsRef
protocolVersion, capabilities, observedAt
```

`RuntimeProvider` 表示控制权和连接边界。当前 `local`/`Agent` target 一对一管理一台 Node；未来一个 Kubernetes Provider 可以发现多台 Worker Node。这样不要求为每个 K8s Worker 安装普通 Agent，也不会把“集群连接”错误建模成一台服务器。

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

`ExecutionEnvironment` 是稳定的逻辑执行和隔离边界；实际 PID、container ID 或 Pod UID 属于 ProcessObservation：

- 裸机节点固定存在一个 `native` 环境。
- Docker/Podman 的一个 Shard 容器对应一个稳定环境，容器重建不改变环境 ID；`host` 网络模式显式共享宿主网络作用域。
- Kubernetes 的一个 Shard 工作负载对应一个稳定环境；调度前 `observedNodeId` 可为空，Pod 重建或迁移只更新实际 Worker/Pod observation。Worker Node 仍是容量和故障域。

### 2.4 RuntimeInstallation

```text
id, nodeId, environmentId, displayName
serverPath, executablePath, saveRoot, backupRoot
steamcmdPath, workshopContentPath, ugcPath
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

后续房间与资源控制能力：

- `room.start`、`room.stop`、`room.restart`、`room.save`
- `backup.stage`、`backup.upload`、`backup.restore`
- `mod.prepare`、`mod.publish`、`mod.verify`

Agent 不接受任意 Shell 字符串。路径必须落入已登记 RuntimeInstallation 的允许根目录。

Agent 主机在自己的 `app.conf` 中登记受信安装，控制中心只能引用 `INSTALLATION_ID`，不能随操作覆盖路径：

```ini
[runtime]
INSTALLATION_ID = default
SAVE_PATH = /srv/dst/.klei/DoNotStarveTogether
SERVER_PATH = /srv/dst/server
UGC_PATH = /srv/dst/server/ugc_mods
SERVER_MODE = 64

[runtime.secondary]
SAVE_PATH = /srv/dst-secondary/.klei/DoNotStarveTogether
SERVER_PATH = /srv/dst-secondary/server
SERVER_MODE = 64
```

`[runtime]` 默认 ID 为 `default`，额外安装使用 `[runtime.<id>]`。Agent 在启动、停止、重启或保存前重新检查 Cluster 与 Shard 名称、真实路径、`cluster.ini`、`server.ini` 和服务端路径；符号链接不能越出对应的受信 Cluster。Agent 将最高 fencing token 和最近的幂等结果持久化到私有状态文件。若进程在接收操作后、记录结果前中断，重复请求返回 `unknown`，不会盲目再次执行。

### 6.1 Runtime Driver

房间服务只调用统一的类型化 Runtime Driver，不直接依赖 tmux、Docker CLI 或 Kubernetes API：

```text
discover, status, start, stop, restart, save
inventory, logs, backupStage, modPrepare, health
```

- `native` Driver：当前 tmux 实现先迁入该边界，未来可增加 systemd/launchd profile。
- `container` Driver：通过 Docker/Podman API 管理带受管标签的容器，不向请求开放任意镜像、挂载或命令。
- `kubernetes` Driver：以独立 RuntimeProvider 身份，通过受限 ServiceAccount 管理指定 namespace 和 label 范围内的工作负载、Service、PVC、Job 与 Secret 引用。

容器和 Pod 内不运行 tmux。Driver 返回统一状态，但必须保留原始来源，如 PID、container ID、Pod UID、restart count 和退出原因，避免把不同平台的故障压扁成一个 `stopped`。

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

恢复顺序：停止整房间、验证目标拓扑和容量、创建恢复前保护备份、分发全部子快照、校验、启动并确认 Shard 注册。单分片恢复只作为高级实验能力。

存储边界按部署形态实现，但备份 manifest 保持一致：

- 裸机：受信 `saveRoot` 内建立只读 staging，再上传或复制。
- 容器：Shard 存档、备份 staging 和运行安装使用显式 volume；禁止依赖容器可写层保存数据。
- Kubernetes：每个 Shard 使用独立 PVC 或经过验证的等价持久卷；默认 `Retain` 数据语义。完整备份仍需游戏保存屏障，CSI VolumeSnapshot 只能替代复制步骤，不能替代一致性协调。

不得让两个 Shard 容器/Pod 无约束地同时写同一个 Cluster 根目录。Cluster 公共配置生成后分别下发，Shard 私有存档独立挂载，再由备份集在逻辑上合并。

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

### 11.1 裸机

- Linux 推荐 Agent 由 systemd 托管，macOS 推荐 launchd；现有 tmux 继续作为 Shard Runtime Driver，不承担 Agent 保活。
- 路径、用户、文件权限和端口均在 Agent 本机预注册，控制中心只引用 ID。
- CPU `exclusive` 仅 Linux 能力探测通过时开放；macOS 保持建议模式。
- 内存默认只预检和告警，不自动施加可能杀死 DST 的硬限制。

### 11.2 Docker/Podman

- 推荐一个 Shard 一个容器，容器使用非 root 用户、只读基础镜像、明确的存档/Mod/日志 volume 和 graceful stop timeout。
- bridge 与 host 网络都支持，但创建前必须展示实际对外 UDP 映射；跨机器 Master 必须发布可路由地址。
- Docker Socket 等同宿主高权限。Agent 只能操作带项目标签、镜像白名单和挂载白名单的资源；优先支持 rootless Podman 或受限 Socket Proxy。
- DST 二进制采用“受控可变安装卷”或“不可变版本镜像”二选一的 Installation profile，同一发布计划不得静默混用。
- 内存限制是高级设置；Driver 必须把 OOM kill 与普通退出分开审计。配置备份 staging 时还要预留压缩和临时文件空间。

### 11.3 Kubernetes（未来）

- 一个 Shard 一个有状态工作负载，副本数固定为 1，使用稳定身份和独立 PVC；不把多个 Shard 塞入同一个 Pod。
- Master Shard 通信优先通过稳定 Service ClusterIP；DNS 写入 `master_ip` 需验证后开放。玩家 UDP、Steam 端口使用明确的 hostPort、NodePort 或 LoadBalancer profile，不能由前端猜测可达地址，也不能假设端口转换后 Steam 会公布正确外部端点。
- Token、cluster key 放 Secret；非敏感生成配置放 ConfigMap；存档和 Workshop 内容放 PVC/受管缓存，不放 ConfigMap 或容器可写层。
- Readiness 检查 DST 已完成世界加载和 Shard 注册；仅检测进程存在不足以判定可用。Liveness 自动重启默认保守，避免加载慢或保存期间被误杀。
- K8s 自动重调度前必须验证 Room lease、fencing token、旧 Pod UID 终止和存储所有权。不能只因节点 `NotReady` 就在另一节点复制启动同一 Shard。
- 最小权限 RBAC 仅覆盖指定 namespace/label；NetworkPolicy 只放行控制、Master/Secondary、玩家 UDP 和必要 Steam 出站。
- 一致性备份使用保存屏障后协调各 PVC snapshot/上传；单个 PVC 快照不能宣称为完整 Room 备份。
- K8s Placement 默认由 scheduler 在允许节点池中选择，页面显示期望节点池和实际 Worker；“停止某台服务器上的分片”只操作当前实际位于该 Worker 的受管 Shard，不等同于 drain、关机或迁移。

### 11.4 配置体验

默认流程面向个人服主，不要求理解容器网络或 Kubernetes：

1. 新增运行目标时选择“本机”“远程服务器”“容器主机”或“Kubernetes 集群（实验）”。
2. 创建/放置 Shard 时先选运行目标；系统自动生成不冲突的推荐端口和 `none` CPU 策略。
3. “网络高级设置”才展示 bind、对外地址、端口映射和网络作用域；跨节点时必须确认 Master 实际可达地址。
4. “资源高级设置”展示建议容量、CPU request/quota 和独占绑核；独占能力不可用时直接解释原因。
5. 执行前预览按 Node 汇总启动后的所有 Shard，包括其他 Room 和不同执行环境，并显示会操作的具体范围。

商家或多机用户可以保存 Network/CPU/Storage profile 批量复用，但 profile 只保存期望参数，落到目标环境后仍需重新预检，不能把一台机器验证过的端口或 cpuset 原样套到另一台机器。

## 12. 迁移顺序

1. 保留本机默认路径，增加只读 Node/Inventory/Capacity API。
2. 让 Agent 上报类型化 Runtime inventory，控制器持久化观察快照。
3. 建立 Placement 和拓扑版本，但暂不迁移现有本地 Room ID。
4. 把单 Shard 本地控制适配到统一 typed operation，再接入远程 Agent。
5. 增加房间租约、幂等和 fencing 后开放房间级远程操作。
6. 引入 `ExecutionEnvironment`、Runtime Driver、NetworkProfile、PortReservation 和 CPU policy；先让 `native` Driver 完全等价现状。
7. 建立备份集与 Mod 发布协议，所有文件操作从 Runtime Driver 进入。
8. 迁移玩家、日志、世界状态和诊断到带来源的新鲜度模型。
9. 交付 Docker/Podman Driver 和部署模板，再完成裸机/容器混合房间验证。
10. 在独立实验能力中交付 Kubernetes Driver，通过存储、网络、CPU Manager 和故障注入矩阵后再标记生产可用。

任何阶段都不得让远程选择回退到本地执行。旧 Agent 缺少能力时保持只读或显示升级要求。
