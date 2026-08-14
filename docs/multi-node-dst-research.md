# DST 多节点与多分片机制核验

> 核验日期：2026-08-15
> 适用范围：Don't Starve Together Dedicated Server，多节点集中管理设计
> 结论等级：官方文档/官方脚本 > 官方论坛技术说明 > 可复现社区实现

## 1. 已确认结论

### 1.1 一个世界分片对应一个独立专服进程

Klei 的 Dedicated Server 命令行文档用独立命令分别启动 `Master` 与 `Caves`，每个命令都指定同一个 `-cluster` 和不同的 `-shard`。因此：

- 房间是 Cluster 级逻辑对象。
- 地表、洞穴和自定义附加世界都是独立 Shard。
- 每个运行中的 Shard 都对应一个独立 DST Dedicated Server 进程。
- 多层世界可以在同一台机器运行，也可以分布到不同机器。

产品不能用“运行中的房间数”估算进程和 CPU 容量，必须统计运行中的 Shard 数。

来源：

- Klei Support, Dedicated Server Command Line Options Guide：<https://support.klei.com/hc/en-us/articles/360029556192-Dedicated-Server-Command-Line-Options-Guide>
- Klei Forums, Understanding Shards and Migration Portals：<https://forums.kleientertainment.com/forums/topic/59174-understanding-shards-and-migration-portals/>
- 可读取归档：<https://web.archive.org/web/20170709205355id_/http://forums.kleientertainment.com/topic/59174-understanding-shards-and-migration-portals/>

可信度：高。

### 1.2 跨机器分片依赖相同的 Cluster 身份与 Master 连接配置

官方论坛技术说明给出的跨机器配置要点包括：

- 各节点使用一致的 `cluster.ini`。
- Secondary 通过 `master_ip` 和 `master_port` 连接 Master。
- 跨机器监听通常配置 `bind_ip=0.0.0.0`，实际暴露范围仍应由防火墙收敛。
- `cluster_key` 用于拒绝不属于同一 Cluster 的 Shard。
- 每个 Shard 的 `server.ini` 具有独立 `server_port`、`is_master`、`name` 和 `id`。

因此控制中心需要把 Cluster 公共配置与 Shard 私有配置分开管理，并在启动前检查：Cluster 身份、Master 地址、端口、Shard ID 和配置版本是否一致。

来源：同 1.1 的官方论坛说明及归档。

可信度：高；NAT、IPv6 和复杂防火墙组合仍需实机验证。

### 1.3 同机多分片需要避免端口冲突

Klei 命令行文档说明，多层 Cluster 的每个专服进程必须使用不同的玩家 UDP 端口。同机运行时，Steam authentication/master server 相关端口也必须避免冲突。

因此端口预检的唯一性范围是“节点”，而不是“房间”。两个不同房间放在同一节点时也不能复用被占用的监听端口。

来源：Klei Support Dedicated Server Command Line Options Guide。

可信度：高。

### 1.4 保存由 Cluster 内分片协调，文件备份仍需统一屏障

当前游戏脚本基线：

- 本地目录：`/Users/lcy/dst/.p0-cache/scripts-5c8a4303cd92884c/scripts`
- Dedicated Server build ID：`24080846`
- Client scripts build ID：`24080983`

脚本核验结果：

- `c_save()` 在 Master 触发 `ms_save`。
- Secondary 收到保存事件后向 Master 请求保存。
- Master 发布 snapshot，Secondary 在 `components/autosaver.lua` 中将本地 snapshot 同步到对应位置或执行回滚。
- `c_regenerateworld()` 明确提示离线或正在加载的 Shard 不能被正确处理。
- `c_shutdown()` 先保存当前 Shard，再退出当前进程。

由此可知：游戏内部具有跨 Shard 的 snapshot 协调语义，但控制面复制目录时仍必须等待统一保存屏障完成。不能把不同时间点、不同 Session 或不同 snapshot 序号的目录拼成“完整备份”。

来源：当前 DST 官方游戏脚本 `consolecommands.lua`、`components/autosaver.lua` 及相关事件处理代码。

可信度：高；跨节点网络中断发生在保存中途时的精确恢复行为仍需故障注入验证。

### 1.5 Mod 文件是节点/安装级资源，启用配置是 Shard 级资源

DST 的 Mod 相关边界：

- `dedicated_server_mods_setup.lua` 位于 DST 安装侧，声明需要下载的 Workshop 内容。
- Workshop/UGC 文件实际存在于运行该 Shard 的节点。
- `modoverrides.lua` 位于 Shard 配置目录，可在不同 Shard 使用不同启用组合和配置。

因此一个房间跨节点运行时，所有承载相关 Shard 的节点都必须准备匹配的 Mod 文件版本；但各 Shard 的启用状态和配置允许不同。控制面应采用“全部节点预下载并校验 -> 发布各 Shard 配置 -> 协调重启 -> 日志确认加载”的阶段式发布。

来源：Klei Dedicated Server 配置约定、当前项目运行目录和游戏脚本行为。

可信度：高。

## 2. CPU 容量规则

用户产品要求：同一服务器允许运行多个世界，但必须提醒用户“一核心最多安排一层世界”，避免卡顿。

实现口径：

- 默认把每个运行中的 Shard 计为 1 个 CPU 核心预算单位。
- 默认预留 1 个物理核心给操作系统、Agent、SteamCMD、压缩和备份任务。
- 建议上限为 `max(1, physicalCores - reservedCores)`。
- 无法可靠取得物理核心数时，采用 `max(1, floor(logicalProcessors / 2))` 作为保守估计，并标记为 `estimated`。
- `runningShards >= recommendedLimit` 时为容量已满；`runningShards > recommendedLimit` 时为超配。
- 启动预检按“启动后的 Shard 数”计算，超配时要求显式确认，但不强行禁止手动放置。
- 自动放置不把新 Shard 分配到容量已满的节点。

这不是 Klei 的性能保证。大型世界、高玩家数、洞穴蠕虫潮、复杂世界生成或高负载 Mod 都可能让单个 Shard 消耗超过一个核心预算。

社区交叉参考：Jamesits/docker-dst-server 建议小型服务器至少约 1 CPU core，并明确高 tick 设置需要更多性能：<https://github.com/Jamesits/docker-dst-server>

可信度：产品保守策略；需要用实机采样持续校准。

## 3. 需要继续实机验证

以下行为不能只靠资料推断：

1. Master 在保存屏障中途掉线时，各 Secondary 的 snapshot 最终状态。
2. Secondary 与 Master 网络分区后，进程是否保持、何时重连，以及玩家迁移的失败表现。
3. Master 与 Secondary 的最佳启动/停止顺序，以及不同版本下的注册等待时间。
4. Cluster 跨 Linux/macOS 节点时，存档、Mod 和脚本大小写差异。
5. 单 Shard 恢复到多 Shard Cluster 的可接受边界。
6. 同一 KU ID 在迁移窗口被两个 Shard 短暂报告在线时的去重窗口。
7. SteamCMD/UGC 在不同平台对同一 Workshop item 的落盘目录和完成标记。
8. 物理核心、性能核/能效核和容器 CPU quota 下的容量换算。

## 4. 实机测试记录格式

每次验证至少记录：

```text
caseId:
gameBuildId:
agentVersion:
controllerVersion:
platforms:
topology:
initialState:
operation:
faultInjection:
observedTimeline:
diskArtifacts:
logs:
result:
followUp:
```

未完成实机验证的条目不得升级为无警告的自动化破坏性操作。
