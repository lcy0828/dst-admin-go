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

因此在裸机或共享宿主网络中，端口预检的唯一性范围是“节点网络作用域”，而不是“房间”。两个不同房间放在同一节点时也不能复用被占用的监听端口。容器或 Pod 拥有独立网络命名空间时，内部端口可以重复，但映射到宿主、NodePort 或其他共享地址的端口仍须在各自作用域内唯一。

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

### 1.6 容器 CPU 配额不等于核心绑定

Docker 官方文档区分：

- `--cpus`/`--cpu-quota` 是容器可使用的 CPU 时间上限，达到上限会被节流。
- `--cpu-shares` 只在 CPU 竞争时调整相对权重，不保证预留份额。
- `--cpuset-cpus` 才是限制容器只能在指定逻辑 CPU 集合运行。

因此产品不能把“1 CPU 配额”显示成“独占 1 个物理核心”。独占策略还必须结合宿主物理核心与 SMT sibling 拓扑，避免给两个 Shard 分配同一个物理核心的两个超线程。

来源：

- Docker Docs, Resource constraints：<https://docs.docker.com/engine/containers/resource_constraints/>

访问日期：2026-08-15。可信度：高。

### 1.7 Kubernetes 独占 CPU 有严格前置条件

Kubernetes 官方 CPU Manager 文档说明，`static` 策略下只有 Guaranteed Pod 中具有整数 CPU request 的容器才会获得 exclusive CPU；系统还必须预留 CPU。普通 CPU request 用于调度和份额保障，CPU limit 由内核节流执行，都不能自动等价为固定核心。

因此 Kubernetes Driver 只有确认以下条件时才能报告 `exclusive`：

- Worker 的 kubelet CPU Manager policy 为 `static`。
- Shard 容器 CPU request 与 limit 相等且为整数，使 Pod 满足 Guaranteed QoS。
- kubelet 已为系统预留 CPU，Pod 实际取得的 cpuset 可被观察。

否则只能提供 `reserved` 或 `none`，不能静默把独占请求降级后仍显示“绑核成功”。

来源：

- Kubernetes, Control CPU Management Policies on the Node：<https://kubernetes.io/docs/tasks/administer-cluster/cpu-management-policies/>
- Kubernetes, Resource Management for Pods and Containers：<https://kubernetes.io/docs/concepts/configuration/manage-resources-containers/>

访问日期：2026-08-15。可信度：高。

### 1.8 Kubernetes 的网络和持久化必须显式建模

Kubernetes 官方资料确认：

- StatefulSet 适用于需要稳定网络身份和持久存储的工作负载。
- PVC/PV 把持久存储从 Pod 生命周期中分离，访问模式与回收策略需要单独声明。
- Service 的 `ClusterIP`、`NodePort` 和 `LoadBalancer` 具有不同的可达范围；UDP 端口也必须显式声明。
- NetworkPolicy 分别控制 ingress 和 egress，实际执行能力依赖集群网络插件。

对 DST 的产品推论是：一个 Pod 一个 Shard、一个 Shard 独立 PVC 是安全默认值；Master 优先使用稳定 Service ClusterIP 供 Secondary 连接，玩家与 Steam UDP 端点按部署环境显式暴露。Kubernetes 能转发 UDP 并不能证明 DST/Steam 会公布端口转换后的外部端点，因此 NodePort/LoadBalancer 的发现、连接和源地址行为仍需实机验证。Kubernetes DNS 名能否直接用于 `master_ip` 也不得在验证前假定。CSI snapshot 只能提供卷级快照能力，完整 Room 备份仍需 DST 保存屏障和所有 Shard manifest。

“一个 Pod 一个 Shard”是本项目为隔离故障、CPU、端口和存档所有权作出的设计选择，不是 Kubernetes 或 Klei 的强制规则。

来源：

- Kubernetes, StatefulSets：<https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/>
- Kubernetes, Persistent Volumes：<https://kubernetes.io/docs/concepts/storage/persistent-volumes/>
- Kubernetes, Service：<https://kubernetes.io/docs/concepts/services-networking/service/>
- Kubernetes, Network Policies：<https://kubernetes.io/docs/concepts/services-networking/network-policies/>

访问日期：2026-08-15。可信度：官方平台语义高；DST 组合方案需要实机验证。

### 1.9 Docker 的发布端口和数据卷是独立边界

Docker 官方资料说明，bridge 网络中的端口默认不会从宿主对外开放；`--publish` 会把容器端口映射到指定宿主地址和 TCP/UDP 端口。Volume 的生命周期独立于容器可写层，容器删除后仍可保留数据。

因此容器 Driver 必须分别登记 container endpoint 与 host published endpoint，并显式指定 UDP。DST 存档、Mod、日志和备份 staging 必须落在受管 volume/bind mount，不能依赖容器可写层。Docker 支持端口转换不代表 DST/Steam 会公布转换后的外部端口，这部分仍按 1.8 的原则实机验证。

来源：

- Docker Docs, Port publishing and mapping：<https://docs.docker.com/engine/network/port-publishing/>
- Docker Docs, Volumes：<https://docs.docker.com/engine/storage/volumes/>

访问日期：2026-08-15。可信度：Docker 平台语义高；DST 外部发现行为需要实机验证。

### 1.10 主服务、Agent 与 DST 容器化是三个问题

当前项目代码核验：

- Agent 的 RuntimeInstallation 使用 Agent 所在环境可见的绝对 `SAVE_PATH`、`SERVER_PATH` 和 `UGC_PATH`。
- Agent 的 native Shard control 直接构造 tmux Runtime。
- Agent 将最高 fencing token、lease 和幂等操作结果写入本地 operation state file。

因此可以直接支持的首要组合是“主服务容器 + 裸机 Agent + 裸机 DST”：主服务容器只负责 Web/API/数据库，裸机 Agent 继续拥有真实路径、tmux 和进程视图。把 Agent 放进容器后，如果仍要控制宿主 native DST，仅挂一个配置文件并不足够，还需要受信路径、tmux socket/运行目录、宿主进程可见性、相同用户权限和持久 Agent state。

Docker 官方资料同时说明：bind mount 默认可写宿主文件并与宿主目录结构强耦合；Docker daemon 通常具有高权限，只能向受信主体开放。因此：

- 主服务容器默认不挂 DST 目录、host PID 或 Docker Socket。
- 容器 Agent 管理容器化 DST 时优先使用 rootless Podman 或受限 Docker Socket Proxy。
- 容器 Agent 管理宿主 native DST 作为单独的 `native-host-integration` 高权限 profile，验证前不能作为默认安装方式。
- Agent ID、凭证、Runtime 注册表和 operation/fencing state 必须使用持久卷；状态丢失后禁止自动接管旧 Shard。

来源：

- 当前代码：`agent/runtime_installations.go`、`agent/shard_operations.go`。
- Docker Docs, Bind mounts：<https://docs.docker.com/engine/storage/bind-mounts/>
- Docker Docs, Docker Engine security：<https://docs.docker.com/engine/security/>

访问日期：2026-08-15。可信度：当前代码与 Docker 平台语义高；容器 Agent 的 native host-integration 仍需 Linux 实机验证。

### 1.11 tmux 当前承担 DST 控制台输入，容器化不能删除该能力

当前代码核验：

- `internal/shards/tmux_control.go` 的 `Send` 最终调用 `tmux.DSTServer.SendCommand`。
- `tmux/tmux.go` 使用 `tmux send-keys ... C-m` 把 Lua 输入正在运行的 DST 会话。
- 保存、优雅停止、玩家管理、命令目录、自动化、运行时激活和 console fallback 都依赖统一 `Send` 接口。
- `internal/console/service.go` 会在脚本前后打印 `START/DONE` marker，但当前 `Store.Complete` 在 `Send` 返回后就将状态记为 `sent`；尚无日志消费者用 `DONE` 确认 DST 已实际执行。

因此“容器能启动/停止”不等于 Runtime 功能等价。Docker 官方说明，`docker exec` 会在运行容器内启动一个新进程；它不能直接向已有 DST 主进程 stdin 写入 Lua。Docker attach 可以连接运行容器主进程的 stdin/stdout/stderr，Engine API 也提供 `AttachStdin`、`OpenStdin` 和 `StdinOnce`，但 attached client 断开、日志缓冲、Engine 重启和并发写入仍需实机验证。

产品结论：

- native Runtime 继续使用 tmux。
- container Runtime 首版在每个 Shard 容器内保留 `tmux-compat`，Agent 通过固定 container Driver 动作调用 tmux client；Agent 与 DST 仍不放在同一容器。
- attach/stdin 作为后续候选 transport，要求 `OpenStdin=true`、`StdinOnce=false`、`Tty=false` 并只 attach stdin；完成等价矩阵前不替换 tmux。
- Runtime Driver 必须提供 `SendConsole`、`ConsoleHealth` 和命令 marker 确认，区分“已写入 transport”和“DST 已执行”。
- 目标实例变化或确认中断时结果为 `unknown`；保存、关服等危险命令不得自动重放到新实例。

来源：

- 当前代码：`internal/shards/tmux_control.go`、`tmux/tmux.go`、`internal/console/service.go`。
- Docker Docs, `docker container attach`：<https://docs.docker.com/reference/cli/docker/container/attach/>
- Docker Docs, `docker container exec`：<https://docs.docker.com/reference/cli/docker/container/exec/>
- Docker Engine API container configuration and attach endpoint：<https://docs.docker.com/reference/api/engine/>

访问日期：2026-08-15。可信度：当前 tmux 与 Docker API 语义高；DST attach/stdin 的长期可靠性需实机验证。

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
- CPU 策略默认 `none`；只有平台能力和核心拓扑均可验证时才提供独占绑核。
- Linux 裸机优先通过 cgroup/cpuset，Docker 通过 `cpuset-cpus`，Kubernetes 通过 CPU Manager static policy；macOS 不宣称支持可靠的独占绑核。

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
8. 物理核心、SMT、性能核/能效核和容器 CPU quota 下的容量换算。
9. Docker bridge/host 网络下四类 UDP 端口的暴露、Steam 列表可见性和跨宿主 Master 连接。
10. Kubernetes ClusterIP/NodePort/LoadBalancer 下玩家连接、Steam 列表、Shard 注册和源地址行为。
11. Shard Pod 被强制删除、Worker 失联、PVC 重挂载时，lease/fencing 是否能阻止双实例写入。
12. 保存屏障完成后使用不同 CSI snapshot provider 生成一致备份集的时序和失败语义。
13. 主服务容器经反向代理连接同宿主/远程裸机 Agent 时的 WebSocket 重连、真实来源地址和健康检查。
14. 容器 Agent 在同 UID/GID、受限 bind mount、tmux socket 和宿主进程视图下控制 native DST 的完整生命周期。
15. Agent 容器状态卷丢失、回滚或复制后，identity/fencing 防止重复接管的行为。
16. container tmux-compat 在 Agent/Engine 重启、Shard 高日志量和多次命令发送后的控制台可用性。
17. Docker attach/stdin 的断开重连、并发串行、stdin 是否保持打开，以及 Lua 换行/编码行为。
18. 命令 transport 成功但 marker 未出现、命令执行中 Shard 重启和危险命令去重语义。

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
