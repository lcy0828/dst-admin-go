# 裸机与容器部署

> 实现状态：控制面容器、Agent 容器/裸机服务和一 Shard 一容器 Runtime 已交付。Kubernetes 仍属于实验阶段，不能把本页的 Docker 能力等同于 Kubernetes 能力。

## 安全边界

- 控制面容器不挂 Docker socket、宿主 PID、DST 服务端或存档目录。管理本机裸机 DST 也通过外部 Agent。
- Agent 与 DST 不放在同一容器。Agent 升级、重连或崩溃不会直接终止 DST；DST Shard 也不能读取 Agent 密钥。
- 一个 DST 容器只运行一个 Shard。每个容器必须带四个受管 label，Agent 逐条复核，不通过名称猜测归属。
- 挂载 Docker socket 的 Agent 等价于拥有宿主 root 级控制权，只能在可信节点显式启用 `container-control` profile。不要把该 Agent 暴露到公网。
- DST 镜像不包含 Klei 专有文件。`dst-server` volume 必须由管理员通过 SteamCMD 合法安装并以只读方式挂载。
- `linux/amd64` 控制面镜像通过 Debian `non-free` 包安装 `/usr/games/steamcmd`，仅用于维护控制器 Workshop 内容库；内容库位于持久卷 `/var/lib/dst-admin/workshop`。控制面不挂载 Docker socket、DST 存档或 DST 服务端目录。
- Compose 固定设置 `DST_ADMIN_DISABLE_LOCAL_GAME_UPDATE=true`。版本查询仍可使用 Steam/Klei 数据源，但控制面容器不会把“镜像中存在 SteamCMD”误判为可更新本地 DST，也不会向 `unmanaged-server` 下载一份无人管理的服务端。DST 二进制更新必须由对应 Runtime 节点执行。
- 控制面镜像在所有架构安装 Lua 5.1，作为 Go `modinfo.lua` 解析器的兼容 fallback；是否使用 fallback 仍由解析结果和诊断信息明确标记。
- 控制面镜像保留 `tmux` 客户端，用于判断其容器内部本地 Runtime 是否已停止以及执行迁移前保护；它没有宿主 PID namespace、宿主 tmux socket 或 Docker socket，不能借此控制宿主 DST。
- Valve 没有提供 ARM Linux SteamCMD。ARM 控制面仍可管理房间和节点，但 Workshop 下载会报告 SteamCMD 不可用；需要完整 Mod 流程时，应部署 `linux/amd64` 控制面镜像（ARM 主机需具备 amd64 模拟）或通过自定义镜像提供经过验证的 SteamCMD。DST Linux Runtime 同样以 x86_64 为生产目标。
- 默认建议一颗物理核心最多运行一个 Shard，并至少为系统、Agent、SteamCMD 和备份预留一核。

## 部署组合与权限

主服务、Agent 和 DST Runtime 是三个独立部署维度。当前已交付的组合是：

| 主服务 | Agent | DST | 当前支持情况 |
| --- | --- | --- | --- |
| 裸机 | 裸机 | 裸机 | 支持；Agent 与 DST 使用同一账号或等价的受控文件/tmux 权限 |
| 容器 | 裸机 | 裸机 | 推荐；同机 Agent 连接 `ws://127.0.0.1:8000/agent`，跨机使用控制面实际可达地址 |
| 容器 | 容器 | Shard 容器 | 支持的容器 Runtime 形态；Agent 通过 Docker/Podman CLI 和受管 label 控制 |
| 容器 | 容器 | 裸机 | 当前 Compose 不支持；不要通过 host PID、宿主 tmux 和大范围 hostPath 拼出兼容模式 |
| Kubernetes | 外部 Provider | Pod | 实验安全内核，尚未接入生产 Driver、API 或 UI |

Agent 和 DST 始终是不同容器。tmux session 与 socket 位于每个 DST Shard 容器内；容器 Driver 执行固定的 `docker exec ... tmux` 客户端参数，Agent 不接管 DST 的 PID namespace，也不把任意 exec 暴露给控制面。`CONSOLE_SOCKET` 是 Shard 容器内的路径，不是要求挂载到 Agent 容器的 Unix socket。

每个 Runtime installation 的 Mod 路径由 Agent 本地配置固定：

| 配置 | 内容 | 持久化和权限 |
| --- | --- | --- |
| `WORKSHOP_CONTENT_PATH` | 受信 Steam Workshop 内容根 | 可重建的下载源；不能由远程请求覆盖 |
| `MOD_CACHE_PATH` | 按 tree SHA 保存的不可变发布缓存 | 需要 Agent 可写并持久化；丢失后需重新分发，离线回滚会受影响 |
| `MOD_STATE_PATH` | 上传断点、发布计划、journal 和 installation state | 必须私有、可写、持久化；不能与 cache 路径相同或互相嵌套 |

裸机模板把 cache/state 放在 `/var/lib/dst-admin-agent`。容器模板也放在 `agent-data` volume，而不是默认的 `SERVER_PATH/.dst-admin`，避免状态落入只读安装卷。三个路径必须是绝对路径；cache/state 初始化和后续文件操作会拒绝不安全的符号链接或越界目标，Workshop 根也不应配置为可被其他用户替换的链接。

## Docker Compose

先构建并启动无宿主控制权的控制面：

```bash
cd deploy/docker
docker compose up -d control-plane
docker compose ps
```

首次启动会在 `control-data` 中生成 `app.conf` 和 SQLite 数据库。启动前必须通过安全的 secret 管理方式提供 `DST_ADMIN_AGENT_SECURITY_KEY`；Compose 会把同一个密钥同时注入控制面网关和 Agent，任一侧缺失都会在配置展开阶段失败。不要把密钥写进 Compose 文件或 URL。

镜像默认从 Debian 官方源安装运行依赖。在官方源访问较慢的网络中，可以在构建时临时设置 `DEBIAN_MIRROR` 和 `DEBIAN_SECURITY_MIRROR`；这两个参数只改变 Debian 包下载地址，不改变 Go 模块、Steam Workshop 或应用运行时网络。镜像必须使用可信、同步完整且支持 HTTPS 的 Debian 镜像站，例如：

```bash
DEBIAN_MIRROR=https://mirrors.aliyun.com/debian \
DEBIAN_SECURITY_MIRROR=https://mirrors.aliyun.com/debian-security \
docker compose build control-plane agent
```

### 主服务容器 + 裸机 Agent

这是管理宿主机裸机 DST 的推荐组合。同一宿主上把 `/etc/dst-admin/agent.conf` 的 `SERVER_URL` 设为 `ws://127.0.0.1:8000/agent`，Runtime 使用 `[runtime.native]`；只启动 Compose 的 `control-plane`，不要再启动 `container-control` profile 中的 Agent，避免同一安装出现两个控制者。

Agent 在另一台机器时，`127.0.0.1` 不可用。应让控制面的 `/agent` WebSocket 经 TLS 反向代理或受信内网地址可达，并在裸机 Agent 使用 `wss://`/实际地址。不要为了远程 Agent 挂载控制面容器的 Docker socket、DST 目录或宿主 PID。

### Agent 容器 + DST 容器

容器 Runtime 模式需要先填充 `dst-server` 和 `dst-saves` volumes，确认 Cluster 配置中的 UDP 端口和 compose 映射一致。每台节点必须设置一个全局唯一、重建后保持不变的 `DST_ADMIN_AGENT_ID`；控制面以该 ID 保存拓扑、fencing 和审计记录，不能使用临时容器 ID。然后显式启动：

```bash
export DST_ADMIN_AGENT_SECURITY_KEY='base64-key-from-control-plane'
export DST_ADMIN_AGENT_ID='node-shanghai-01'
export DOCKER_GID="$(stat -c '%g' /var/run/docker.sock)"
docker compose --profile container-control --profile dst-runtime up -d agent dst-master
```

Agent 只控制以下 label 同时匹配的容器：

```text
com.dst-admin.managed=true
com.dst-admin.installation=<Runtime installation ID>
com.dst-admin.cluster=<Cluster directory>
com.dst-admin.shard=<Shard directory>
```

Compose 示例只展示 Master。增加 Caves 时复制 Shard 服务并使用独立 `server_port`、`authentication_port`、`master_server_port`，不要复用 UDP 映射。分片互联参数仍由 DST 的 `cluster.ini` 和 `server.ini` 决定。

Compose 中各 volume 的边界如下：

- `control-data`：控制面数据库/WAL、`app.conf`、备份集、地图制品、控制器 Workshop 内容库、SteamCMD HOME，以及兼容本地接口使用的 `unmanaged-saves`、`unmanaged-server`、`unmanaged-ugc`。它不挂给 Agent 或 DST，但容量不能只按 SQLite 估算；Workshop、备份和地图可能成为主要占用。
- `agent-data`：Agent ID、fencing/幂等状态以及 `MOD_CACHE_PATH`、`MOD_STATE_PATH`，不挂给 DST。
- `dst-saves`：Cluster/Shard 存档和 `modoverrides.lua`；DST 需要读写，启用 Mod 发布和恢复时 Agent 也需要读写。
- `dst-server`：DST 二进制安装卷；Agent 和 DST 都只读挂载。
- `dst-mods`：覆盖安装卷的 `mods` 子目录；Agent 读写，DST 只读，用于 `workshop-*` 和 `dedicated_server_mods_setup.lua` 的原子发布。
- `dst-workshop`：Steam Workshop 下载内容，对应 `WORKSHOP_CONTENT_PATH`；它不是房间存档。

Compose 使用嵌套的 `dst-mods` volume，只给 Agent 的 `/srv/dst/server/mods` 写权限；`dst-server` 的其余内容始终只读。DST Shard 在 `/opt/dst/server/mods` 只读查看同一 volume，因此 Agent 可以原子执行 Mod `prepare/publish`，但不能通过这条挂载修改服务端二进制。cache/state 仍位于 `agent-data`，不落入安装卷。

Agent 和 DST 镜像固定使用同一非 root UID/GID `10000:10000`，但仍运行在不同容器、拥有不同进程和密钥边界。`volume-init` 只以 root 运行一次，用于迁移/初始化 `agent-data`、`dst-saves`、`dst-mods` 和 `dst-workshop` 的所有权，完成后退出；它是唯一临时可写挂载 `dst-server` 的容器，仅用于保证空安装卷存在 `server/mods` 挂载点。首次创建 `dst-mods` 时还会复制已有 `server/mods` 内容，避免嵌套挂载隐藏旧的 setup 或本地 Mod。常驻 Agent 和 DST 对 `dst-server` 仍只有只读权限，控制面也不依赖或挂载这些 volume。不要使用 `chmod -R 777`，也不要让两个 Shard 同时无约束地写同一个私有世界目录。

控制面以 UID/GID `10001:10001` 运行。`control-data-init` 只在卷布局 marker 缺失时以 root 创建目录并修正旧卷所有权，随后退出；这同时解决旧版本由 root 创建 Workshop 父目录后 SteamCMD 报 `Staging folder not writable` 的升级问题。升级前必须对整个 `control-data` 做一致性备份，至少包含 SQLite 主文件及 WAL/SHM、配置、备份索引和 Workshop 状态；不能在数据库仍写入时只复制 `go-dont.db`。首次升级后应确认 marker、目录所有者、剩余空间，以及 `/var/lib/dst-admin/workshop/steamapps/workshop/{content,downloads}/322330` 可由 UID 10001 写入。

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

```bash
deploy/scripts/uninstall-macos-agent.sh
deploy/scripts/uninstall-macos-agent.sh --purge-state
```

## 验证与故障处理

```bash
deploy/scripts/smoke-deployment.sh
DST_ADMIN_SMOKE_BUILD=1 deploy/scripts/smoke-deployment.sh
```

验证项包括 Agent WebSocket 注册、心跳、Runtime inventory、类型化 Shard 操作、容器 label 过滤、固定 tmux socket 和 Compose 解析。若容器显示 running 但控制台健康为 starting，进入容器检查：

```bash
docker exec <container-id> tmux -S /run/dst-admin/tmux/tmux.sock has-session -t =dst
docker logs <container-id>
```

停止通过控制台发送 `c_shutdown(true)`，由 DST 自己完成存档并退出。不要用通用 `docker kill` 作为正常停止路径；强制退出必须显示未保存风险并进入异常退出审计。

## 备份边界

房间备份以 `dst-saves` 中的 Cluster 公共文件和各 Shard 私有存档为数据面，必须先经过保存/停服一致性协调。不要把 Docker volume 的逐卷复制直接声明为完整房间备份。

控制面数据库和 `agent-data` 属于平台灾备：应与房间备份分开保护。`MOD_STATE_PATH` 和 Agent runtime state 决定中断恢复、fencing 与幂等语义，恢复 Agent 时必须作为同一节点身份的一组状态处理；不能只恢复旧 Agent ID 而丢弃更高 fencing token。`MOD_CACHE_PATH`、Workshop 内容和 DST 二进制原则上可重建，但保留 cache 能保证精确版本回滚和 Steam 不可用时恢复。

Kubernetes 的 PVC/CSI snapshot 只能替代单卷复制，不能替代跨 Shard 保存屏障、manifest 和集中校验。当前 `deploy/kubernetes` 只有 namespace RBAC 与类型化实验内核，没有可部署的 Provider、lease-aware supervisor、Mod 分发、备份或恢复链路，不能作为生产安装步骤。

## Debian 12 实机证据

2026-08-15 在全新 Debian 12 / Docker 环境完成以下真实链路，测试工作区与既有 DST 环境隔离：

- 非 root 控制面首次启动与旧 `control-data` 权限迁移，控制面健康检查和两个 Agent 重连。
- 裸机 Agent 与容器 Agent 同时注册，Runtime inventory、物理核心容量和受管容器 label 识别正常。
- 分片跨节点/跨 volume apply 成功，目标 Placement 变为 `aligned`，源端保留可恢复迁移目录。
- SteamCMD 下载 Workshop `1392778117` 成功，约 110 MB、1433 个文件，并验证 tree SHA。
- 发布前创建分布式保护备份；Mod cache bundle 以 256 KiB 分块上传，完成跨节点 tree 校验与两端本地 manifest 校验后原子发布，再回读目标 `modoverrides.lua`。
- 2026-08-16 继续验证迁移后控制器本机已不存在 Shard 目录的场景：房间 Mod 列表、配置文件和 `modinfo.lua` schema 均从当前 Placement 正常聚合，没有回落到控制器本机。
- 对 Workshop `1392778117` 依次完成禁用、启用、配置 `AutoStackedLoot=true` 和移除。四个 Job 与 Publication 均为 `succeeded/full`；每步都回读目标文件。移除后 `modoverrides.lua` 为 `return {}`，托管 setup 段无 `ServerModSetup`，节点不可变 cache 仍保留用于重用和回滚。

对应最终 Job ID 为 `c23bcdb2-09fc-43ba-bbb8-04e6eba38f9d`、`a908e856-8df7-4ed9-90cb-87d5e926b87d`、`f8233414-f253-4e6c-9bb5-bba4a2a4aca8`、`0bce17e5-0801-4755-a7cb-6f3fc1a094c0`；Publication ID 为 `3e7dd0ad-a2b4-4948-b738-0dd311057050`、`321d6b7b-3e2e-42ba-bfc3-ddc85c138245`、`7ddc78bc-1299-44cf-9da5-0d63bd6ffc84`、`2dfe6b50-3baf-42e0-80e1-7d72d41703a2`。

这些证据验证的是 Debian 12 Docker/native 组合，不扩展为 Podman、macOS 容器或 Kubernetes 生产兼容声明。
