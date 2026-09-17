# DST Admin 部署模型

**简体中文（默认）** | [English](deployment-profiles.en.md)

<a id="dst-admin-deployment-profiles"></a>

DST Admin 使用同一套代码和产品模型。部署打包方式与管理角色可以独立选择，
不会因此引入不同的 API、房间模型或功能分支。

首次安装、依赖准备、原生构建和 Agent 接入请按[安装与启动指南](startup-guide.md)执行。
本文说明打包方式、角色及支持边界。

<a id="deployment-model"></a>

## 部署模型

打包方式描述同一个应用如何交付：

- `native`：直接运行在 Linux 或 macOS 上。
- `all_in_one`：应用、Vue 页面、SteamCMD、tmux 和 DST 运行在同一个 OCI 容器中，不使用 Docker socket。
- `container`：一个管理容器直接控制每个本机 Shard 的独立容器，不运行额外的本机 Agent 容器。所有 Shard 共享一份从宿主挂载的 DST 安装。
- `control_plane`：安装管理应用，但不包含本机 DST 执行器。

管理角色描述这个实例控制哪些机器：

- `standalone`：只管理本机 DST 安装。
- `controller_worker`：管理本机安装，同时接受其他 Fleet Member 主动发起的 WebSocket 连接。
- `managed_worker`：代表一个上级 Controller 管理本机安装。本机页面保留只读访问，变更操作由 Controller 执行，避免两个写入者。
- `controller_only`：管理 Fleet Member，不运行本机 DST 执行器。

同一个实例不能同时作为上级 Controller 和下级 Member。
Native、All-in-One 和独立 Shard 容器都可以使用 `standalone`、`controller_worker` 或 `managed_worker`。
因此 Agent 属于传输能力，不是一种独立部署模型。

<a id="local-and-agent-equivalence-boundary"></a>

## 本机与 Agent 的一致性边界

`local` 和 `agent:<id>` 在产品层面都是 Runtime 目标。
房间放置、生命周期、控制台命令、日志、玩家和世界观测、备份、Mod、游戏更新、地图、
网络探测、CPU 策略和节点资源展示，都必须通过能够识别目标的契约访问，不能默默回退到控制端机器。

两者使用不同的传输方式。`local` 使用进程内适配器，可以直接读取控制端主机上的可信状态。
远程 Agent 自己负责认证、心跳、重连、安装登记、路径校验、操作日志，以及观测和接收时间。
Controller 进程、Go runtime 和数据库诊断仅属于控制端，通过 `/system/status` 展示；
各节点可比较的主机 CPU、内存、磁盘、负载、运行时间、数据新鲜度和可用性通过 `/system/resources` 展示。

目标是保持用户可见契约和失败语义一致，无需让控制端通过环回 WebSocket 连接自己。
新增 Runtime 能力必须进入共享 Driver 或节点资源边界。
仅用于本机的直接文件访问只适用于控制端诊断，不能被展示为支持远程的操作。

<a id="install-native-local"></a>

## 安装本机原生服务

按 [Linux/macOS 启动指南](startup-guide.md)准备依赖、运行用户、空数据目录、匹配的前后端构建和配置。
Linux 首次安装从 `local.conf.example` 开始，升级则必须使用当前生效配置的独立副本。

Linux 使用 `dst-admin-local.service`，监听 `0.0.0.0:8000`，显式加载 `/var/lib/dst-admin/app.conf`。
macOS LaunchAgent 使用当前登录用户，监听 `127.0.0.1:8000`，从
`~/Library/Application Support/DST Admin` 加载配置。
两种安装方式都由同一个 Go 服务提供构建后的 Vue 页面，并使用本机进程内 Runtime。

配置 SteamCMD 并登记游戏和存档路径后，在“游戏服务端管理”中安装游戏，再创建房间；
也可以接入已有游戏安装。系统继续使用已配置的存档目录，导入存档是单独的房间操作。
详见[游戏安装管理](game-installation-management.md)。

<a id="shared-ownership-model"></a>

## 共享职责模型

```text
Vue 页面 -> HTTP/SSE -> 控制端 -> Runtime Driver -> tmux -> DST Shard
                                  -> SteamCMD / files / backups
```

控制端维护房间、Shard、配置、Job 和审计的期望状态；Runtime Driver 提供观测到的进程与文件状态。
所有表示实时状态的响应都必须保留 `observedAt`、新鲜度、来源和 Runtime 状态，不能悄悄用缓存数据替代。

统一设置部署时区 `TZ`，默认 `Asia/Shanghai`。
控制端、Agent 和所有 DST Runtime 必须使用相同时区，因为 DST 的 `Current time` 日志标记不包含时区偏移。
API 和数据库时间戳仍使用 RFC 3339/UTC，只在展示时转换。

<a id="native"></a>

## 原生部署

管理服务和所有受管 DST 进程使用同一个专用系统用户。
本机 `NativeDriver` 直接访问可信的存档与服务端根目录，并使用私有 tmux dispatcher。
`standalone` 模式不涉及 Agent 密钥、WebSocket 转发、Docker socket 或 Placement 迁移。
同一个二进制以后可以切换成 Controller 或 Member，无需改变本机执行模型。

Linux 路径示例（已有安装可以保留原位置）：

```text
/var/lib/dst-admin/       数据库和管理状态
/opt/dst/server/          DST 专用服务端
/opt/dst/saves/           Cluster 和 Shard 数据
/opt/dst/workshop/        SteamCMD Workshop 下载根目录
/opt/dst/backups/         备份
```

macOS 服务使用当前登录用户，以访问该用户的 Steam 和 `DoNotStarveTogether` 目录。
UGC 内容必须使用可写的 Steam Workshop 路径，不能把应用包当作可写的 Mod 暂存区域。

<a id="all-in-one"></a>

## All-in-One

All-in-One 镜像包含 Go API、构建后的 Vue 页面、SteamCMD、Lua、tmux 和 DST 运行环境。
Go 同时提供 `/api` 和 SPA，并直接使用本机 `NativeDriver`，无需将 Agent 和 DST 放入不同容器。
Master、Caves 及其他 Shard 是同一容器中的独立进程，可以分别控制。
Klei 当前的 Linux 专服面向 `linux/amd64`，因此 Apple Silicon 宿主通过 Docker 的 amd64 模拟运行此模式，
不构建无法使用的 ARM 镜像。

所有持久化状态放在容器 `/opt/dst` 下，从宿主 `${DST_ADMIN_DATA_ROOT:-/opt/dst}` 绑定挂载。
替换容器不能丢失数据库、存档、Workshop 内容、Mod 发布版本、备份或服务端文件。
容器没有 Docker socket，也不需要特权模式。
容器关闭时，入口脚本先向每个受管 DST pane 发送保存并关闭命令，再停止 API。

该模式优先简化安装和本机操作，具体限制如下：

- 更新或替换容器会重启该容器内的所有 Shard。
- 容器级 CPU 限制作用于整个安装。首版 All-in-One 不宣称支持逐 Shard CPU 强制限制，因为当前 Linux 执行器需要委派可写的 cgroup v2 控制器。未来针对容器的 taskset/cpuset 驱动必须验证后才能开放。
- 本机执行器属于同一个故障和存储范围。Fleet 角色可以连接其他节点，但不会让本机 `/opt/dst` 卷变成分布式存储。

按 [README Docker 说明](../README.md#docker-快速安装)构建并启动镜像。
镜像标签、宿主数据根目录和开放端口保存在 `deploy/docker/.env`；
所有 Compose 操作，包括升级和重启，都使用相同的显式 `--env-file`。

官方 Debian 镜像源较慢时，构建脚本接受 `--debian-mirror`、`--debian-security-mirror`，
或对应的 `DST_ADMIN_DEBIAN_MIRROR`、`DST_ADMIN_DEBIAN_SECURITY_MIRROR` 环境变量。
管理页面可在 DST 尚未安装时启动。“游戏服务端管理”可以在没有房间时通过后台 Job 安装游戏。
Agent 安装由对应 Agent 下载。只有无人值守部署需要在 API 启动前完成 SteamCMD 下载时，才设置 `DST_ADMIN_BOOTSTRAP_DST=true`。

健康检查通过后打开 `http://HOST:8080`。
容量估算计入每台主机全部运行中的 Shard。有效 CPU 不超过两颗时不预留整核，三颗及以上时为系统和维护预留一颗。
内存风险单独评估，详见[产品使用场景（英文）](product-usage-scenarios.md)。

常见的 2 vCPU / 4 GiB 主机保持默认 All-in-One，只运行 Master 和 Caves。
默认不为任一 Shard 绑核：共享或超售 VPS 的 CPU 拓扑往往不可靠，亲和性限制可能降低调度器利用短暂空闲的能力。
两颗 CPU 主机的容量预览不预留完整 CPU；预计空闲内存低于 768 MiB 时警告，低于 384 MiB 时要求确认。

<a id="container-shards"></a>

## 独立 Shard 容器

`container` 模式将页面、API、SteamCMD、文件和调度放在一个管理容器内，不再运行第二个本机 Agent。
管理容器只通过 Docker socket 发现和控制带有固定 `com.dst-admin.*` 标签的容器。

Compose 不写死任何房间。Shard 第一次启动时，系统根据配置的可信镜像创建容器；以后复用同一容器启停。
每个 Shard 以只读方式挂载共享的服务端和 Workshop 根目录，以读写方式挂载共享的存档根目录。
Runtime 专用的 tmux 状态留在该 Shard 容器内。
这样可以分别设置 CPU 策略和隔离故障，同时不重复保存 DST 二进制。

Klei 仅发布 amd64 Linux 专服，因此此模式的两个镜像都构建为 `linux/amd64`。
推荐 Debian 或 Ubuntu x86_64 宿主。Apple Silicon 可以模拟此模式做兼容性测试，实际游戏应使用原生 macOS Runtime。

构建并启动本机容器模式：

```sh
deploy/scripts/build-all-in-one-image.sh --tag dst-admin/all-in-one:dev
docker compose -f deploy/docker/compose.yaml --profile build build dst-runtime-image
docker compose -f deploy/docker/compose.yaml up -d dst-admin
```

默认宿主数据根目录为 `/opt/dst`。打开 `http://HOST:8080`，在总览安装游戏，创建房间，再启动 Master 和 Caves。
Docker socket 具有宿主级控制权限，因此此模式只适合可信的单用户服务器。
远程机器仍通过 Agent 协议连接，不会获取本机 Docker socket。

对于 2 vCPU / 4 GiB 服务器，All-in-One 仍是最简单的默认选择。
当独立世界重启或 CPU 隔离的收益值得增加镜像和 Docker socket 边界时，才使用独立 Shard 容器。
两种模式都不能运行超过主机承载能力的活跃 Shard，Master 加 Caves 是实际默认配置。

<a id="fleet-roles"></a>

## 集群管理角色

启用 Controller 会在同源 `/agent` WebSocket 端点启动 Gateway。
启用 Member 会启动内嵌 Agent，主动连接一个 `ws://` 或 `wss://` Controller 地址，并上报本机可信 RuntimeInstallation。
身份和执行中操作状态持久化在 `fleet.STATE_PATH` 下。

Fleet 保留现有类型化 Agent 协议、RuntimeInstallation 登记、Placement、租约、fencing、幂等和持久恢复机制。
Member 不接受任意 Shell 命令。执行目标来自登记安装；显式游戏检测和接入可以校验所选节点上的源路径。
首版继续使用现有共享 Gateway 密钥。逐节点注册码和独立凭据仍是后续加固项目，不能宣称已经实现。

角色变更通过“机器管理”的部署角色面板保存，重启进程后生效。
部署打包方式在运行时只读，因为变更需要重新安装或替换部署产物。

内嵌角色与兼容的独立 Agent 进程使用不同的环境变量：

| 用途 | 配置 | 环境变量 |
| --- | --- | --- |
| 启用本机执行 | `fleet.LOCAL_EXECUTOR_ENABLED` | `DST_ADMIN_LOCAL_EXECUTOR_ENABLED` |
| 启用 Controller Gateway | `fleet.CONTROLLER_ENABLED` | `DST_ADMIN_FLEET_CONTROLLER_ENABLED` |
| 启用内嵌 Member | `fleet.MEMBER_ENABLED` | `DST_ADMIN_FLEET_MEMBER_ENABLED` |
| 上级 Controller WebSocket | `agent.SERVER_URL` | `DST_ADMIN_FLEET_CONTROLLER_URL` |
| 内嵌 Member 凭据 | `agent.SECURITY_KEY` | `DST_ADMIN_FLEET_MEMBER_KEY` |
| 内嵌 Member 稳定 ID | `fleet.NODE_ID` | `DST_ADMIN_FLEET_NODE_ID` |

Controller Gateway 凭据仍使用 `server.SECURITY_KEY` 或 `DST_ADMIN_AGENT_SECURITY_KEY`，不复用为本机 Member 配置。
`DST_ADMIN_AGENT_SERVER_URL` 和 `DST_ADMIN_AGENT_SECURITY_KEY` 继续作为独立 Agent 二进制的输入，
不会成为内嵌 Member 的回退变量。部署可以在连接两端提供同一密钥值，但必须通过各进程对应角色的变量传入；密钥不能放入 WebSocket URL。

只有同一操作通过完整的“UI → Job → Agent → Runtime → 观测 → UI”链路成功，Fleet 才算可用。
单元测试通过或 Agent 声明某项能力，都不能单独作为完成证据。

<a id="release-gates"></a>

## 发布门禁

先通过本机 Native 验收：

- 创建或导入 Master+Caves 房间；
- 启动、停止、重启、保存、控制台、日志、玩家及世界状态；
- Mod 下载、配置、更新和待重启状态；
- 备份、恢复、游戏更新和磁盘空间保护；
- macOS 与 Debian 12 冒烟测试。

随后 All-in-One 在没有独立 Agent 容器或 Docker socket 的条件下重复同一矩阵。
两种模式都通过后，Fleet 专属开发才增加远程 Placement、传输和分布式一致性用例。
