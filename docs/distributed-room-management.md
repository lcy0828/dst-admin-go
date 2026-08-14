# 多节点房间集中管理设计

> 状态：实现基线
> 更新日期：2026-08-15
> 依赖：`distributed-room-management-plan.md`、`multi-node-dst-research.md`

## 1. 产品边界

系统默认管理控制器本机。只有用户显式添加并配置 Agent 后，远程节点才进入管理范围。

首期不提供任意远程 Shell，也不把控制器本地路径直接套用到远程节点。所有远程能力都通过带版本的类型化协议完成，并由 Agent 对目标路径和资源再次校验。

支持的拓扑包括：

- 一个节点运行一个房间的一个 Shard。
- 一个节点运行同一房间的多个 Shard。
- 一个节点运行多个房间的多个 Shard。
- 一个房间的不同 Shard 分布在多个节点。

同机多 Shard 是合法能力，但默认容量策略为“每个运行中的 Shard 预留一个物理核心预算单位，并给系统至少预留一个核心”。

这里的 Shard 统计跨房间累计：同一台服务器既可以承载一个房间的多层世界，也可以承载多个房间的多个世界。界面必须明确提醒用户“一核心最多规划一层运行中的世界”，超过时提示可能卡顿；该规则是保守容量建议，不是硬限制，也不是性能保证。

## 2. 数据模型

### 2.1 Node

```text
id, displayName, hostname, os, arch
agentVersion, protocolVersion, capabilities
addresses, connectionState
logicalProcessors, physicalCores, physicalCoreSource
memoryTotalBytes, memoryAvailableBytes
observedAt, stale, staleReason
```

`connectionState=offline` 不等于该节点上的 DST 已停止。连接断开后，所有进程观察状态变为 `unknown/stale`。

### 2.2 RuntimeInstallation

```text
id, nodeId, displayName
serverPath, executablePath, saveRoot, backupRoot
steamcmdPath, workshopContentPath, ugcPath
platform, serverMode, buildId
observedAt, health
```

路径在所属节点解释。控制器不能把远程路径传入本地房间、备份、Mod 或日志服务。

### 2.3 Room、Shard 与 Placement

```text
Room: id, clusterName, desiredConfigVersion
Shard: id, roomId, directoryName, role, shardId
Placement: shardId, nodeId, installationId, topologyVersion, desiredState
```

约束：

- 一个拓扑版本中，一个 Shard 只有一个有效 Placement。
- 一个 Room 只有一个 Master role。
- 同一节点上的监听端口全局唯一。
- Shard 的目录名、`server.ini` 中的 `id` 和产品 ID 分开保存，不能互相猜测。

### 2.4 ProcessObservation

```text
instanceId, nodeId, installationId, roomId, shardId
pid, startedAt, executable, buildId
runtimeState, exitSource, cpuPercent, rssBytes
observedAt, stale
```

Agent 通过进程参数中的 `-cluster`、`-shard` 和 `-persistent_storage_root` 识别归属。无法唯一归属的进程进入诊断列表，不能自动绑定到房间。

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

## 5. Agent 协议

协议版本从能力协商开始。每条操作具有：

```text
operationId, idempotencyKey, action
nodeId, installationId, roomId, shardId
topologyVersion, leaseId, fencingToken
preconditions, parameters, deadline
```

首批只读能力：

- `node.inventory.read`
- `runtime.installations.read`
- `runtime.shards.read`
- `runtime.processes.read`
- `runtime.capacity.read`

后续控制能力：

- `shard.start`、`shard.stop`、`shard.restart`、`shard.save`
- `room.start`、`room.stop`、`room.restart`、`room.save`
- `backup.stage`、`backup.upload`、`backup.restore`
- `mod.prepare`、`mod.publish`、`mod.verify`

Agent 不接受任意 Shell 字符串。路径必须落入已登记 RuntimeInstallation 的允许根目录。

## 6. 生命周期协调

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

## 7. 分布式备份集

完整备份集由 Cluster 公共快照和每个必需 Shard 的子快照组成：

```text
save barrier -> every shard acknowledges snapshot
-> local immutable staging -> manifest/hash
-> optional central upload -> verify -> commit backup set
```

只有所有必需子快照具备相同 Cluster 保存点语义并通过校验时，备份集才是 `complete`。部分成功只能标为 `partial`，不得用于默认一键恢复。

恢复顺序：停止整房间、验证目标拓扑和容量、创建恢复前保护备份、分发全部子快照、校验、启动并确认 Shard 注册。单分片恢复只作为高级实验能力。

## 8. Mod 发布

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

## 9. 看板与控制范围

集中总览以表格和异常队列为主，支持按 Node、Room、Shard 三个维度筛选。

房间看板展示每个 Shard 的节点、进程、玩家、Mod、版本、资源和数据时间。节点看板展示硬件容量、运行分片、安装实例、磁盘和任务。

操作范围必须显式：

- 当前 Shard
- 当前 Room 全部分片
- 当前 Node 上选中的分片
- 用户勾选的多个 Room/Shard

执行确认页展示每个节点启动前/后的 Shard 数和 CPU 建议上限。

## 10. 迁移顺序

1. 保留本机默认路径，增加只读 Node/Inventory/Capacity API。
2. 让 Agent 上报类型化 Runtime inventory，控制器持久化观察快照。
3. 建立 Placement 和拓扑版本，但暂不迁移现有本地 Room ID。
4. 把单 Shard 本地控制适配到统一 typed operation，再接入远程 Agent。
5. 增加房间租约、幂等和 fencing 后开放房间级远程操作。
6. 建立备份集与 Mod 发布协议。
7. 迁移玩家、日志、世界状态和诊断到带来源的新鲜度模型。

任何阶段都不得让远程选择回退到本地执行。旧 Agent 缺少能力时保持只读或显示升级要求。
