# DST Admin

**简体中文（默认）** | [English](README.en.md)

首次访问会自动进入初始化向导，创建管理员后继续完成环境、游戏和房间设置。
进度可在刷新或重启后继续，详见[首次开服说明](docs/startup-guide.md#首次开服与已有存档)。

DST Admin 是一个《饥荒联机版》(Don't Starve Together) 专用服务器管理面板。服主可以在浏览器中创建房间、启动世界、安装模组、管理玩家、查看日志和备份存档。

本项目由两个仓库共同组成，生产环境只运行一个管理服务：

| 仓库 | 作用 |
| --- | --- |
| `dst-admin-go`（当前仓库） | 管理服务、DST 进程、存档、模组和任务 |
| [`dst-admin-vue`](https://git.luocaiyi.top/dst/dst-admin-vue) | 浏览器界面，构建后由管理服务直接提供 |

> **当前为源码预览版本。** 后端最新代码位于 `feature/v2-rebuild`，前端位于 `master`。仓库尚未发布稳定 Tag、安装包或预构建镜像，不建议直接用于无法接受停机和手工恢复的重要服务器。

## 从这里开始

| 你要做什么 | 阅读入口 |
| --- | --- |
| 一台 Linux 服务器首次开服 | 下方 [Docker 快速安装](#docker-快速安装)，推荐 All-in-One |
| 裸机安装，或增加远程 Agent | [安装与启动指南](docs/startup-guide.md) |
| 接入已有游戏和存档 | [游戏安装管理](docs/game-installation-management.md)，存档从房间管理单独导入 |
| 安装 LuaJIT2、跟随上游版本 | [LuaJIT 安装说明](docs/luajit-installation.md) |
| 本地修改前后端源码 | [开发启动说明](docs/development.md) |
| 升级、迁移或回滚已有服务 | [发布与回滚](docs/deployment-and-rollback.md) |

已有服务不要重新套用首次安装模板。保持生效配置、数据库、Agent 身份和存档目录，按升级流程操作。

## 推荐配置

已经安装 Docker 的 x86_64 Linux 服务器，推荐使用 **All-in-One**。Web、管理服务、SteamCMD、tmux 和 DST 位于一个容器中，持久数据保存在宿主机。

| 服务器 | 建议 |
| --- | --- |
| 2 核 4 GB | Master + Caves，不建议再增加世界 |
| 4 核 8 GB | Master + Caves，并为系统和更新任务保留资源 |
| 多台服务器 | 先完成一台本机部署，再按需添加 Agent |

容量建议会统计同机所有运行世界。有效 CPU 不超过 2 个时不整核预留，3 个及以上时默认为系统预留 1 核；内存另行检查。共享 VPS 不建议默认绑核。

项目以 [真实用户部署场景](docs/product-usage-scenarios.md) 作为架构、性能和界面决策基线：优先保证 2C4G All-in-One 运行一个 Master+Caves 房间，再扩展多 All-in-One、All-in-One + Agent 和跨机器分片。

## Docker 快速安装

### 安装前准备

- Docker Engine 和 Compose 插件（[官方安装说明](https://docs.docker.com/engine/install/)）。
- 至少 2 核 CPU、4 GB 内存和 20 GB 可用磁盘。
- 可修改服务器防火墙和云安全组。
- 从 [Klei 服务器页面](https://accounts.klei.com/account/game/servers?game=DontStarveTogether) 获取 DST Cluster Token。

Klei 的 Linux DST 专用服务器目前是 `amd64`。ARM 服务器需要模拟运行，普通服主不建议使用。

### 1. 下载匹配的前后端

```bash
git clone --branch feature/v2-rebuild --single-branch \
  https://git.luocaiyi.top/dst/dst-admin-go.git
git clone --branch master --single-branch \
  https://git.luocaiyi.top/dst/dst-admin-vue.git dst-admin-vue-v3
cd dst-admin-go
```

两个仓库必须处于同一目录：

```text
你的目录/
├── dst-admin-go/
└── dst-admin-vue-v3/
```

### 2. 构建并启动

先在仓库根目录检查 Docker：

```bash
docker version
docker compose version
```

构建前后端镜像，并为这次部署保存固定配置：

```bash
./deploy/scripts/build-all-in-one-image.sh \
  --tag dst-admin/all-in-one:preview

cp deploy/docker/all-in-one.env.example deploy/docker/.env
```

编辑 `deploy/docker/.env`，确认镜像名、宿主数据目录和端口，再启动：

```bash
docker compose --env-file deploy/docker/.env \
  -f deploy/docker/compose.all-in-one.yaml up -d
```

第一次构建会下载基础镜像和依赖，所需时间取决于网络速度。
后续命令都在仓库根目录使用同一份 `--env-file`，避免重启时意外切换镜像或数据目录。
已有 `.env` 不重复复制模板。该文件已加入 Git 忽略规则。

### 3. 确认管理服务可用

```bash
docker compose --env-file deploy/docker/.env \
  -f deploy/docker/compose.all-in-one.yaml ps
curl -fsS http://127.0.0.1:8080/api/v2/auth/session
```

容器应显示为 `healthy`，会话接口应返回 JSON。失败时查看日志：

```bash
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml \
  logs --tail=200 dst-admin
```

### 4. 完成首次开服

浏览器访问 `http://服务器IP:8080`（修改管理端口后使用新端口），然后依次完成：

1. 创建管理员账号。
2. 打开“游戏服务端管理”，在目标机器点击“安装游戏服务端”，等待任务完成；也可检测并接入该机器上已有的游戏目录。
3. 填写 Cluster Token。
4. 创建新房间，或导入已有存档。
5. 添加 Master 和 Caves。
6. 检查端口并启动房间。
7. 等待两个世界都显示“运行中”。
8. 使用页面提供的地址或直连代码加入游戏。

管理页面启动成功不代表 DST 已经安装。必须先完成第 2 步，才能启动世界。

本机与 Agent 的安装状态、版本及目录均可在同一页查看。原生部署需先配置
SteamCMD 和游戏/存档路径；接入已有游戏目录不会导入或删除存档。
详见[游戏服务端管理](docs/game-installation-management.md)。

## 安装成功的标准

- Compose 中的 `dst-admin` 容器为 `healthy`。
- 管理页面可以登录和保存设置。
- 游戏服务端显示已安装。
- Master 和 Caves 均从“启动中”进入“运行中”。
- 实时日志中出现世界加载和服务注册记录。
- 外网玩家能够通过 Master 的玩家端口连接。

其中任何一项未完成，都不应把部署视为成功。

## 端口和玩家连接

All-in-One 默认映射：

| 类型 | 默认范围 |
| --- | --- |
| 管理页面 | `8080/tcp` |
| DST 玩家端口 | `10999-11020/udp` |
| Steam 鉴权端口 | `8766-8790/udp` |
| Steam 列表端口 | `27016-27040/udp` |

玩家连接 Master 的玩家端口，例如：

```lua
c_connect("服务器公网IP", 10999)
```

必须同时检查云安全组、服务器防火墙和路由器端口映射。Web 页面能打开，只能证明 TCP 管理端口可用，不能证明 DST 的 UDP 端口可用。

## 数据保存位置

All-in-One 默认把持久化数据保存在宿主机 `/opt/dst`：

| 目录 | 内容 |
| --- | --- |
| `/opt/dst/control` | 管理配置、账号和数据库 |
| `/opt/dst/saves` | 房间、世界和游戏存档 |
| `/opt/dst/server` | DST 游戏服务端 |
| `/opt/dst/workshop` | Steam Workshop 模组 |
| `/opt/dst/backups` | 手动、定时和保护备份 |
| `/opt/dst/maps` | 地图渲染结果 |

容器更新或删除不会主动移除这个绑定目录。迁移服务器时应复制整个 `/opt/dst`，并额外保留离线备份。

自定义 `DST_ADMIN_DATA_ROOT` 时，上表的宿主路径随之变化，容器内仍是 `/opt/dst`。
页面填写容器内可见路径；接入宿主上已有游戏时，需先把源目录挂载到容器可见位置。

不要在世界运行时手工移动存档、替换数据库或修改受管配置文件。

## 常用维护命令

### 查看状态和日志

```bash
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml ps
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml \
  logs --tail=200 dst-admin
```

### 停止和重新启动

```bash
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml down

docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml up -d
```

容器停止时会先请求受管世界保存并退出。停止容器不会删除 `/opt/dst`。

### 更新源码预览版本

更新前在面板中备份并正常停止房间，停止管理服务写入后备份宿主数据根目录及 `.env`。
另存当前镜像的标签或 image ID，给新镜像使用不同标签。若工作区有未提交修改，先整理这些修改再更新源码。

```bash
git pull --ff-only
git -C ../dst-admin-vue-v3 pull --ff-only

./deploy/scripts/build-all-in-one-image.sh \
  --tag dst-admin/all-in-one:preview-next
```

将 `.env` 的 `DST_ADMIN_IMAGE` 改为新镜像标签，保持 `DST_ADMIN_DATA_ROOT` 不变：

```bash
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml up -d
```

更新后检查容器健康、登录、房间列表，再按需启动原房间并确认日志。下次更新另取新标签。
切回旧镜像前核对数据库兼容性；详细备份与回滚原则见[发布与回滚](docs/deployment-and-rollback.md)。

### 卸载但保留数据

```bash
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml down
```

删除源码目录和镜像不会删除 `/opt/dst`。只有确认备份可恢复且不再需要服务器时，才应单独处理这个数据目录。

## 使用要点

- 一个房间对应一个 DST Cluster，可以包含 Master、Caves 和其他世界。
- Master 是房间内唯一的主分片角色，不代表它一定是森林。
- 模组“下载”只保存文件；还需要添加到房间并选择启用的世界。
- 不同房间和世界可以使用不同的模组配置。
- 启用、关闭或修改模组后，需要重启受影响的世界。
- 存档由 DST 持续写入；备份用于恢复、下载和迁移完整房间。
- 世界停止后保留的季节、天数等快照会标记为已过期，不是实时状态。

## 常见问题

### 页面能打开，但世界无法启动

确认游戏服务端已安装、Cluster Token 有效、磁盘空间充足，并查看任务失败提示和请求 ID。

### 玩家无法连接

确认使用 Master 的玩家端口，并检查 UDP 防火墙、云安全组和公网端口映射。

### 两个房间能使用相同端口吗

可以保存相同端口，但不能在同一台机器上同时运行。需要同时运行时，应分配不同端口。

### 添加模组后为什么没有立即生效

DST 在世界启动时读取模组启用状态和 `modoverrides.lua`。修改后需要重启受影响的世界。

### 如何提交可排查的问题

请同时提供：

- 管理页面显示的请求 ID。
- 失败任务的错误代码和目标。
- `docker compose ... ps` 输出。
- 最近 200 行管理服务日志。
- 受影响房间和世界名称。

不要公开 Cluster Token、Steam API Key、管理员密码或 Agent 密钥。

## 其他部署方式

普通服主优先使用 All-in-One。下列方式需要更多系统知识：

- [Linux/macOS 原生安装与远程 Agent 启动](docs/startup-guide.md)
- [部署模型与角色](docs/deployment-profiles.md)
- [多机器架构与能力边界](docs/distributed-room-management.md)
- [模组管理](docs/mod-management.md)
- [备份和恢复](docs/distributed-room-management.md)
- [LuaJIT 安装与上游更新](docs/luajit-installation.md)
- [平台能力和限制](docs/dst-platform-matrix.md)

原生安装需从匹配源码构建后端二进制、地图渲染器和前端产物，命令已在启动指南中给出；仓库尚未提供可直接下载的稳定安装包。

## 安全建议

- 首次安装后立即设置强管理员密码。
- 公网使用时配置 HTTPS，并限制能够访问管理页面的 IP。
- 不要直接暴露未加密的管理端口。
- 定期把存档备份复制到另一台机器或对象存储。
- 不要把任何服务端密钥提交到仓库。
- 分片容器模式需要 Docker Socket，只适合可信的单用户服务器。
