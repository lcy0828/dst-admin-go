# 裸机与容器部署

> 实现状态：控制面容器、Agent 容器/裸机服务和一 Shard 一容器 Runtime 已交付。Kubernetes 仍属于实验阶段，不能把本页的 Docker 能力等同于 Kubernetes 能力。

## 安全边界

- 控制面容器不挂 Docker socket、宿主 PID、DST 服务端或存档目录。管理本机裸机 DST 也通过外部 Agent。
- Agent 与 DST 不放在同一容器。Agent 升级、重连或崩溃不会直接终止 DST；DST Shard 也不能读取 Agent 密钥。
- 一个 DST 容器只运行一个 Shard。每个容器必须带四个受管 label，Agent 逐条复核，不通过名称猜测归属。
- 挂载 Docker socket 的 Agent 等价于拥有宿主 root 级控制权，只能在可信节点显式启用 `container-control` profile。不要把该 Agent 暴露到公网。
- DST 镜像不包含 Klei 专有文件。`dst-server` volume 必须由管理员通过 SteamCMD 合法安装并以只读方式挂载。
- 默认建议一颗物理核心最多运行一个 Shard，并至少为系统、Agent、SteamCMD 和备份预留一核。

## Docker Compose

先构建并启动无宿主控制权的控制面：

```bash
cd deploy/docker
docker compose up -d control-plane
docker compose ps
```

首次启动会在 `control-data` 中生成 `app.conf`、Agent 通信密钥和 SQLite 数据库。读取 `[server] SECURITY_KEY` 后，通过安全的 secret 管理方式提供给 Agent。不要把密钥写进 Compose 文件或 URL。

容器 Runtime 模式需要先填充 `dst-server` 和 `dst-saves` volumes，确认 Cluster 配置中的 UDP 端口和 compose 映射一致，然后显式启动：

```bash
export DST_ADMIN_AGENT_SECURITY_KEY='base64-key-from-control-plane'
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
