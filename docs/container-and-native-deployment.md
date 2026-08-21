# 裸机与容器部署

> 新安装优先选择裸机或 All-in-One。需要每个世界独立容器时，管理容器直接承担本机 Runtime，不再额外运行本机 Agent 容器。Agent 只用于远程机器。

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
- 服务端和 UGC 只读，只有存档目录可写；tmux 状态位于容器临时文件系统。
- 世界使用 host network，玩家端口、Steam 端口和 Master 分片端口仍以后台生成的 `cluster.ini`/`server.ini` 为准。创建前必须通过端口检查。
- 一颗物理核心最多运行一个活跃 Shard。2C4G 默认只建议 Master+Caves，不默认绑定 CPU；4C8G 及以上才建议按需要启用独立 CPU 策略。
- 管理容器重启不会主动杀死正在运行的世界容器；恢复后会按受管标签重新发现。正常停止世界仍先发送 `c_shutdown(true)`，超时才进入有审计的容器停止 fallback。

## 远程 Agent

远程 Linux/macOS 机器继续安装独立 Agent，因为管理中心不能直接访问另一台机器的文件、tmux 或 Docker Engine。开启本实例的“本机 + 集中管理”角色后，远程 Agent 使用 `ws://`/`wss://HOST/agent` 连接。Agent 配置只登记该远程主机的受信安装路径。

本机不需要 Agent 容器。只有在明确要求管理服务与本机 Docker 权限分离时，才保留外置 Agent 作为高级兼容部署；它不是默认安装流程。

## Debian 12 裸机 Agent

构建 Agent：

```bash
CGO_ENABLED=0 go build -trimpath -o dist/dst-admin-agent ./agent/cmd/agent
sudo deploy/scripts/install-native-agent.sh \
  --binary "$PWD/dist/dst-admin-agent" \
  --config "$PWD/deploy/systemd/agent.conf.example" \
  --user dst
```

编辑 `/etc/dst-admin/agent.conf` 和可选的 `/etc/dst-admin/agent.env`，确认目录属于运行 DST 的账号，再启动：

```bash
sudo systemctl enable --now dst-admin-agent
sudo systemctl status dst-admin-agent
journalctl -u dst-admin-agent -n 100 --no-pager
```

原生模式的 Agent 与 DST 必须使用同一用户或具备明确的 tmux/文件权限。不要通过放宽整个存档目录为全局可写来解决权限问题。

systemd unit 只允许写 `/var/lib/dst-admin-agent` 和 `/srv/dst`。若把 `WORKSHOP_CONTENT_PATH`、`MOD_CACHE_PATH` 或 `MOD_STATE_PATH` 改到其他根，必须同步收紧地更新 unit 的 `ReadWritePaths`，否则 systemd sandbox 会正确拒绝写入。运行用户必须拥有 cache/state，且能按所启用功能读写 `server/mods` 和目标 Shard 的 `modoverrides.lua`。

Agent 2.5.1 为每个 native Installation 在 Agent 状态目录下生成稳定、私有的 tmux socket，相同 Cluster/Shard 名称在不同 Installation 中不会串服。升级时如发现同名会话仍在旧的默认 socket 运行，Agent 返回 `LEGACY_TMUX_SOCKET_CONFLICT` 并拒绝启动第二个实例。先用旧版管理方式正常停止该 Shard，再由新 Agent 启动一次即完成迁移。

本机排障可以使用同一 Agent 二进制的受控 attach。默认只读，不会暂停自动命令：

```bash
sudo -u dst /usr/local/bin/dst-admin-agent \
  -attach -state /var/lib/dst-admin-agent/runtime-state.json \
  -installation native -cluster Cluster_1 -shard Master
```

可写 attach 必须提供操作者并获取 1 分钟至 1 小时的 maintenance lease；租约期间 dispatcher 拒绝自动命令，进程退出、连接中断或超时都会结束租约：

```bash
sudo -u dst /usr/local/bin/dst-admin-agent \
  -attach -write -owner "$USER" -lease 10m \
  -state /var/lib/dst-admin-agent/runtime-state.json \
  -installation native -cluster Cluster_1 -shard Master
```

未经 Agent CLI 直接建立的可写 tmux client 会将 ConsoleHealth 标记为 `external_writer`，并阻止后续自动输入直到新 Runtime instance 建立。Web UI 不提供宿主 Shell 或 attach 入口。

重复运行安装脚本会原位升级二进制和 service。卸载默认保留配置、Agent ID、最高 fencing token 和幂等状态，防止重装后失去操作历史；只有确认节点不再被控制面管理时才清除状态：

```bash
sudo deploy/scripts/uninstall-native-agent.sh
sudo deploy/scripts/uninstall-native-agent.sh --purge-state
```

## macOS launchd Agent

Agent 应以运行 DST 和 tmux 的同一 macOS 用户安装为 LaunchAgent，不使用 root LaunchDaemon。构建后安装或升级：

```bash
go build -trimpath -o dist/dst-admin-agent ./agent/cmd/agent
deploy/scripts/install-macos-agent.sh \
  --binary "$PWD/dist/dst-admin-agent" \
  --config "$PWD/deploy/systemd/agent.conf.example"
launchctl print "gui/$(id -u)/top.luocaiyi.dst-admin-agent"
```

配置、密钥和幂等状态保存在 `~/Library/Application Support/DST Admin Agent`，日志位于 `~/Library/Logs/DST Admin Agent`。卸载同样默认保留状态：

macOS attach 不需要 `sudo`，`-state` 指向 `~/Library/Application Support/DST Admin Agent/runtime-state.json`。由于 macOS 对 Unix socket 路径长度限制更严，Agent 在当前用户的临时目录中使用基于状态目录哈希的短路径，目录权限固定为 `0700`。

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

```bash
docker exec <container-id> tmux -S /run/dst-admin/tmux/tmux.sock has-session -t =dst
docker logs <container-id>
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
