# 裸机与容器部署

**简体中文（默认）** | [English](container-and-native-deployment.en.md)

> 新安装优先选择裸机或 All-in-One。需要每个世界独立容器时，管理容器直接承担本机 Runtime，不再额外运行本机 Agent 容器。Agent 只用于远程机器。

首次部署按[安装与启动指南](startup-guide.md)执行，那里包含依赖、用户、目录、连接密钥、
配置和验收。本文保留运行机制及进阶排障；升级时保留生效配置与 Agent 身份，不重新套用模板。

## 支持的简单模型

| 场景 | 本机控制方式 | 远程扩展 |
| --- | --- | --- |
| 裸机 | 管理程序直接控制本机 tmux 和 DST 文件 | 可开启管理中心并添加 Agent |
| All-in-One | 管理程序与全部 DST Shard 在同一容器 | 可开启管理中心并添加 Agent |
| Docker 独立世界 | 管理容器通过 Docker socket 直接控制每个 Shard 容器 | 可开启管理中心并添加 Agent |
| 仅控制端 | 不运行本机 DST | 只管理 Agent |

同一台机器只有一个本地管理者。不要同时让管理容器和另一个本机 Agent 控制相同存档、容器或 tmux 会话。远程 Agent 仍通过类型化操作、Placement、租约和审计接入，不接受任意 Shell。

## Docker 独立世界

`deploy/docker/compose.yaml` 只常驻一个 `dst-admin` 管理容器。`dst-runtime-image` 是构建占位服务，不会常驻。Compose 不预先写死房间，也不会在启动后台时自动启动 Master/Caves。

```bash
deploy/scripts/build-all-in-one-image.sh --tag dst-admin/all-in-one:dev
docker compose -f deploy/docker/compose.yaml --profile build build dst-runtime-image
docker compose -f deploy/docker/compose.yaml up -d dst-admin
```

打开 `http://HOST:8080`。未安装 DST 时，服务总览会显示“安装游戏服务端”；安装由普通 Job 执行，Web 不会因 SteamCMD 下载失败而无法启动。也可以设置 `DST_ADMIN_BOOTSTRAP_DST=true`，让无人值守部署在 API 启动前完成下载。

用户第一次启动任意世界时，管理程序会创建对应容器。之后启动、停止、控制台命令和 CPU 策略都复用该容器。容器只能由以下四个标签识别：

```text
com.dst-admin.managed=true
com.dst-admin.installation=default
com.dst-admin.cluster=<Cluster directory>
com.dst-admin.shard=<Shard directory>
```

管理程序不会根据容器名称猜测归属，也不会控制缺少标签或标签重复的容器。每个 Runtime 内部使用私有 tmux socket；控制台命令使用固定 `docker exec ... tmux` 参数，不向 Web 暴露宿主 Shell。

## 数据目录

默认宿主根目录是 `/opt/dst`：

| 路径 | 内容 | 世界容器权限 |
| --- | --- | --- |
| `/opt/dst/control` | 配置、SQLite 数据库和管理状态 | 不挂载 |
| `/opt/dst/server` | 一份共享 DST 二进制和 `mods` | 只读 |
| `/opt/dst/saves` | 房间公共配置和各 Shard 存档 | 读写 |
| `/opt/dst/workshop` | SteamCMD Workshop 内容 | UGC 根只读 |
| `/opt/dst/backups` | 存档备份和系统快照 | 不挂载 |
| `/opt/dst/maps` | 地图渲染制品 | 不挂载 |

Master、Caves 和其他世界不会各下载一份服务端。SteamCMD 只更新 `/opt/dst/server`；世界重启后读取同一版本。模组由管理容器写入共享服务端/Workshop 目录，世界容器通过只读挂载和 `-ugc_directory` 使用这些文件。每个 Shard 只写自己的存档目录，房间备份仍由应用协调全部世界，不用逐容器复制代替。

Klei 的 Linux 专服只提供 amd64 二进制，因此管理镜像和世界运行时镜像固定构建为 `linux/amd64`。Debian/Ubuntu x86_64 是该模式的推荐宿主；macOS 默认继续使用本机 Runtime，Apple Silicon 上的容器模式仅适合兼容性测试，不适合作为高性能游戏宿主。

## 安全与资源边界

- Docker socket 等价于宿主机 root 级控制权，只能部署在可信服务器；不要把它交给远程 Agent 或第三方容器。
- 管理容器启动时会读取 Docker socket 的实际 GID，再以非 root UID `10000` 和该 GID 运行；无需手动设置 `DOCKER_GID`。
- 世界容器以 UID/GID `10000:10000`、只读根文件系统、全部 capability 删除和 `no-new-privileges` 运行。
- 普通启动不执行存档写入预检，直接使用当前磁盘文件创建或拉起世界容器；权限、配置或 Mod 问题由本次分片 Job 和 DST 日志反馈。重启仍会在停止现有世界前执行写入预检，避免先停服后才发现目标无法重新启动。
- 服务端和 UGC 只读，只有存档目录可写；tmux 状态位于容器临时文件系统。
- 世界使用 host network，玩家端口、Steam 端口和 Master 分片端口仍以后台生成的 `cluster.ini`/`server.ini` 为准。创建前必须通过端口检查。
- 一颗物理核心最多运行一个活跃 Shard。2C4G 默认只建议 Master+Caves，不默认绑定 CPU；4C8G 及以上才建议按需要启用独立 CPU 策略。
- 管理容器重启不会主动杀死正在运行的世界容器；恢复后会按受管标签重新发现。正常停止世界仍先发送 `c_shutdown(true)`，超时才进入有审计的容器停止 fallback。

## 远程 Agent

远程 Linux/macOS 机器继续安装独立 Agent，因为管理中心不能直接访问另一台机器的文件、tmux 或 Docker Engine。开启本实例的“本机 + 集中管理”角色后，远程 Agent 使用 `ws://`/`wss://HOST/agent` 连接。Agent 配置只登记该远程主机的受信安装路径。

本机不需要 Agent 容器。只有在明确要求管理服务与本机 Docker 权限分离时，才保留外置 Agent 作为高级兼容部署；它不是默认安装流程。

## Debian 12 裸机 Agent

按启动指南准备好远端运行环境和 `../dst-agent-local/agent.conf` 后，构建 Agent：

```bash
CGO_ENABLED=0 go build -trimpath \
  -o dist/dst-admin-agent ./cmd/agent
sudo deploy/scripts/install-native-agent.sh \
  --binary "$PWD/dist/dst-admin-agent" \
  --config "$PWD/../dst-agent-local/agent.conf" \
  --user dst
```

安装前，`dst` 必须是实际运行 DST 的账号，并拥有可执行 shell；tmux 无法使用
`nologin` 或 `false` 账号启动世界。安装后编辑
`/var/lib/dst-admin-agent/agent.conf` 和可选的 `/etc/dst-admin/agent.env`，
确认存档与运行目录属于该账号，再启动：

```bash
sudo systemctl enable --now dst-admin-agent
sudo systemctl status dst-admin-agent
journalctl -u dst-admin-agent -n 100 --no-pager
```

原生模式的 Agent 与 DST 必须使用同一用户或具备明确的 tmux/文件权限。不要通过放宽整个存档目录为全局可写来解决权限问题。

Agent 配置位于状态目录，由服务账号以 `0600` 权限持有，以便安全持久化节点 ID 和密钥轮换。安装脚本不会创建、移动或复制 DST 数据目录；示例中的 `/opt/dst` 只是可替换路径，必须改成该机器现有的存档、服务端和 Workshop 目录。systemd 保持 `/usr`、`/boot` 和 `/etc` 只读，实际数据访问继续由运行账号的 Unix 权限和 Agent 本地登记的可信安装路径共同限制。运行用户必须拥有 cache/state，且能按所启用功能读写 `server/mods` 和目标 Shard 的 `modoverrides.lua`。

每个 native Installation 的 `STEAMCMD_PATH`（兼容旧名 `STEAM_CMD_PATH`）必须是绝对路径。安装脚本会在启动服务前验证该路径：若配置的稳定入口尚不存在，会从 `PATH`、`/usr/games/steamcmd`、`/usr/bin/steamcmd`、`/opt/steamcmd/steamcmd.sh` 和 `/opt/dst/steamcmd/steamcmd.sh` 中发现真实可执行文件并创建符号链接；若完全找不到 SteamCMD，安装立即失败。后续系统包升级可以改变真实文件位置，但不要随意修改已登记的稳定入口，否则游戏更新和 Workshop 下载会被识别为不同 Runtime 配置。

native Runtime 根据规范化后的 `SAVE_PATH` 生成稳定、私有的 tmux socket。本机 Runtime 与 Agent 使用同一规则；修改 Agent 状态文件位置或 Installation ID 不会改变已有世界的控制通道。不同存档根目录仍使用不同 socket，同一物理存档根目录不能被两个独立写入者拆成不同控制面。

Agent `2.14.0` 起可选启用 Runtime Peer HTTP Range 服务，让可信网络中的目标 Agent 直接复用源节点已经校验的精确 Mod。该服务默认关闭；为兼容现有部署，配置键仍为 `MOD_PEER_LISTEN_ADDR` 与 `MOD_PEER_ADVERTISE_URL`。Docker Agent 还需要显式发布对应 TCP 端口，防火墙只允许可信 Controller/Agent 网段。未启用时仍按节点 Steam、Controller HTTP Range、旧协议兜底收敛，不影响单机路径。显式 Publication 和 Placement Migration 可在精确收敛后的受控恢复启动中使用一次性 `-skip_update_server_mods`；普通房间启动不传入该参数，也不回读日志进行模组一致性确认。Agent `2.16.0` 起复用同一受控端口传输显式 Placement Migration 产生的不可变存档 ZIP；授权同时绑定目标 Agent、Migration ID、大小、SHA256 和有效期，目标不可达或旧 Agent 会自动回退 Controller relay。

每个 native `SAVE_PATH` 还会持有 `.dst-admin/runtime/owner.lock` 的主机内核独占锁。锁随 Controller/Agent 进程退出自动释放，不依赖容易残留的 PID 文件；tmux 和 DST 不持有该锁，因此控制服务重启后新进程可以立即重新取得所有权，同时保留游戏进程。若本机另一套 All-in-One 或 Agent 指向同一存档，第二个写入者返回 `RUNTIME_OWNER_CONFLICT`，并带出当前持有者、PID 和主机。一个 Agent 配置内也禁止两个 Installation 使用相同 `SAVE_PATH`，或两个 native Installation 使用相同宿主 `CONSOLE_SOCKET`。

每次状态读取与启动前都会同时检查受管 socket、当前用户的默认 tmux socket 和实际 DST 进程参数。默认 socket 冲突返回 `LEGACY_TMUX_SOCKET_CONFLICT`，其他未受管进程返回 `UNMANAGED_DST_PROCESS_CONFLICT`，重复进程返回 `DUPLICATE_DST_PROCESS_CONFLICT`；三者都禁止继续启动。正常停止旧进程后再从页面启动即可建立唯一归属，系统不会自动杀死或接管来源不明的进程。

systemd 使用 `KillMode=process`，页面升级或 Agent 主进程重启不会连带终止 tmux/DST。服务重启必须保留相同运行用户与 `SAVE_PATH`；整机重启后 tmux 和 DST 均已退出，可按普通房间启动流程恢复。游戏更新只通过同一 Runtime 执行停服、更新和恢复，不创建旁路 tmux 会话。

第一个采用稳定 socket 的未发布版本需要在测试环境完成一次切换：确认玩家为 0，使用旧控制通道执行 `c_shutdown(true)`，确认默认 socket 和旧私有 socket 均无该世界会话，再从页面重新启动。正式首发后不得更改 socket 派生规则；后续二进制升级、配置文件迁移和 Installation 改名不再需要重复切换。

本机排障可以使用同一 Agent 二进制的受控 attach。默认只读，不会暂停自动命令：

```bash
sudo -u dst /var/lib/dst-admin-agent/bin/dst-admin-agent \
  -attach -state /var/lib/dst-admin-agent/runtime-state.json \
  -installation native -cluster Cluster_1 -shard Master
```

可写 attach 必须提供操作者并获取 1 分钟至 1 小时的 maintenance lease；租约期间 dispatcher 拒绝自动命令，进程退出、连接中断或超时都会结束租约：

```bash
sudo -u dst /var/lib/dst-admin-agent/bin/dst-admin-agent \
  -attach -write -owner "$USER" -lease 10m \
  -state /var/lib/dst-admin-agent/runtime-state.json \
  -installation native -cluster Cluster_1 -shard Master
```

未经 Agent CLI 直接建立的可写 tmux client 会将 ConsoleHealth 标记为 `external_writer`，并阻止后续自动输入直到新 Runtime instance 建立。Web UI 不提供宿主 Shell 或 attach 入口。

重复运行安装脚本会原位复制二进制、配置和 service。升级时先另存当前生效配置，用该副本作为
`--config`，避免模板覆盖 Agent ID 和密钥；保留 `runtime-state.json`，安装后重启服务。
卸载默认保留配置、Agent ID、最高 fencing token 和幂等状态，防止重装后失去操作历史；只有确认节点不再被控制面管理时才清除状态：

```bash
sudo deploy/scripts/uninstall-native-agent.sh
sudo deploy/scripts/uninstall-native-agent.sh --purge-state
```

## macOS launchd Agent

Agent 应以运行 DST 和 tmux 的同一 macOS 用户安装为 LaunchAgent，不使用 root LaunchDaemon。
先复制 `deploy/systemd/agent.conf.example` 到仓库外的 `../dst-agent-local/agent.conf`，填写上级地址、
连接密钥及本机绝对路径。将 `MOD_CACHE_PATH`、`MOD_STATE_PATH` 改到当前用户的
`~/Library/Application Support/DST Admin Agent` 下（配置中展开为真实绝对路径），
游戏、存档和 Workshop 路径按[macOS 启动指南](startup-guide.md#macos-本机部署)核对。
无需下载模组时可留空 SteamCMD；需要下载时配置真实入口。构建后安装：

```bash
go build -trimpath \
  -o dist/dst-admin-agent ./cmd/agent
deploy/scripts/install-macos-agent.sh \
  --binary "$PWD/dist/dst-admin-agent" \
  --config "$PWD/../dst-agent-local/agent.conf"
launchctl print "gui/$(id -u)/top.luocaiyi.dst-admin-agent"
```

配置、密钥和幂等状态保存在 `~/Library/Application Support/DST Admin Agent`，日志位于 `~/Library/Logs/DST Admin Agent`。卸载同样默认保留状态：

安装脚本会立即重启 LaunchAgent。升级时使用当前生效配置的独立副本，不使用初始模板，
并保留状态目录。原生 Agent 的版本使用源码自身的版本号，不用旧文档的固定版本覆盖。

macOS attach 不需要 `sudo`，`-state` 指向 `~/Library/Application Support/DST Admin Agent/runtime-state.json`。由于 macOS 对 Unix socket 路径长度限制更严，native Runtime 在 `/tmp/dst-admin-runtime-<uid>` 中使用基于规范化 `SAVE_PATH` 哈希的短路径，目录权限固定为 `0700`。

```bash
deploy/scripts/uninstall-macos-agent.sh
deploy/scripts/uninstall-macos-agent.sh --purge-state
```

## 验证与故障处理

```bash
deploy/scripts/smoke-deployment.sh
DST_ADMIN_SMOKE_BUILD=1 deploy/scripts/smoke-deployment.sh
```

验证项包括容器 label 过滤、首次创建参数、固定 tmux socket、远程 Agent 类型化操作和 Compose 解析。若容器显示 running 但控制台健康为 starting，进入容器检查：

Agent `2.13.0` 起，native Runtime 使用 `SAVE_PATH` 派生稳定 tmux socket，并通过 `shard.control.v2` 返回经过进程归属确认的生命周期结果。Agent `2.13.1` 进一步兼容了 systemd `PrivateTmp` 重启后仍留在旧 mount namespace 的默认 tmux socket。由旧版 Agent 启动且在升级时仍运行的世界会被识别为旧通道：页面升级 Agent 后可直接执行一次“停止”；只有同名会话、唯一 DST 进程及 `SAVE_PATH/room/world` 全部一致时才会发送关闭命令。该世界下次启动会自动进入稳定通道，不需要手工删除 socket 或终止 PID。旧 Agent 仍可提供只读状态，但控制器会拒绝其启动、停止、重启和保存请求，避免把不可靠回执显示为成功。

```bash
runtime_container=replace-with-world-container-id
docker exec "$runtime_container" tmux -S /run/dst-admin/tmux/tmux.sock has-session -t =dst
docker logs "$runtime_container"
```

停止通过控制台发送 `c_shutdown(true)`，由 DST 自己完成存档并退出。不要用通用 `docker kill` 作为正常停止路径；强制退出必须显示未保存风险并进入异常退出审计。

本机容器 Runtime 会等待容器真实退出，超时才依次使用 Engine `stop` 和 `kill --signal KILL`。Engine 退出观察区分正常退出、非零退出、SIGKILL、dead 和 `CONTAINER_OOM_KILLED`；进入 fallback 即使最终停止也不会伪装成已确认保存。

## 备份边界

房间备份以 `dst-saves` 中的 Cluster 公共文件和各 Shard 私有存档为数据面，必须先经过保存/停服一致性协调。不要把 Docker volume 的逐卷复制直接声明为完整房间备份。

`/opt/dst/control` 属于平台灾备，应与房间备份分开保护。远程 Agent 的 runtime state 决定 fencing 与幂等语义，恢复远程节点时必须保留；本机容器通过数据库、受管标签和固定 installation ID 重新发现。Mod cache、Workshop 内容和 DST 二进制原则上可重建，但保留 cache 能保证精确版本回滚和 Steam 不可用时恢复。

Kubernetes 的 PVC/CSI snapshot 只能替代单卷复制，不能替代跨 Shard 保存屏障、manifest 和集中校验。当前已提供默认关闭的 Provider 状态、只读 REST observation、类型化 preflight API/UI、namespace RBAC 与实验安全内核，但固定 `applyAllowed=false`，没有 Apply 路由、lease-aware supervisor、Console、Mod 分发、备份或恢复链路，不能作为生产安装步骤。

## 历史 Debian 12 实机证据

2026-08-15 在全新 Debian 12 / Docker 环境完成以下远程 Agent 链路，测试工作区与既有 DST 环境隔离。它证明远程协议与跨节点数据链路，不替代当前“管理容器直控本机 Shard”的重新验收：

- 非 root 控制面首次启动与旧 `control-data` 权限迁移，控制面健康检查和两个 Agent 重连。
- 裸机 Agent 与容器 Agent 同时注册，Runtime inventory、物理核心容量和受管容器 label 识别正常。
- 分片跨节点/跨 volume apply 成功，目标 Placement 变为 `aligned`，源端保留可恢复迁移目录。
- SteamCMD 下载 Workshop `1392778117` 成功，约 110 MB、1433 个文件，并验证 tree SHA。
- 发布前创建分布式保护备份；Mod cache bundle 以 256 KiB 分块上传，完成跨节点 tree 校验与两端本地 manifest 校验后原子发布，再回读目标 `modoverrides.lua`。
- 2026-08-16 继续验证迁移后控制器本机已不存在 Shard 目录的场景：房间 Mod 列表、配置文件和 `modinfo.lua` schema 均从当前 Placement 正常聚合，没有回落到控制器本机。
- 对 Workshop `1392778117` 依次完成禁用、启用、配置 `AutoStackedLoot=true` 和移除。四个 Job 与 Publication 均为 `succeeded/full`；每步都回读目标文件。移除后 `modoverrides.lua` 为 `return {}`，托管 setup 段无 `ServerModSetup`，节点不可变 cache 仍保留用于重用和回滚。

对应最终 Job ID 为 `c23bcdb2-09fc-43ba-bbb8-04e6eba38f9d`、`a908e856-8df7-4ed9-90cb-87d5e926b87d`、`f8233414-f253-4e6c-9bb5-bba4a2a4aca8`、`0bce17e5-0801-4755-a7cb-6f3fc1a094c0`；Publication ID 为 `3e7dd0ad-a2b4-4948-b738-0dd311057050`、`321d6b7b-3e2e-42ba-bfc3-ddc85c138245`、`7ddc78bc-1299-44cf-9da5-0d63bd6ffc84`、`2dfe6b50-3baf-42e0-80e1-7d72d41703a2`。

这些证据验证的是 Debian 12 Docker/native 组合，不扩展为 Podman、macOS 容器或 Kubernetes 生产兼容声明。
