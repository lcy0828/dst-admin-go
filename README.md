# DST Admin

**简体中文（默认）** | [English](README.en.md)

《饥荒联机版》专用服务器管理面板。通过浏览器管理房间、Master/Caves、模组、玩家、日志、存档备份和远程机器。

**安装、下载和升级都从本仓库开始。** All-in-One 镜像、Controller 镜像和原生安装包均内置管理页面，无需单独下载前端、安装 Node.js 或启动 Vite。首次访问进入中文初始化向导，可切换 English，并创建自己的管理员用户名和密码。

当前为预览版本。构建产物见本仓库的 [Package 工作流](https://github.com/lcy0828/dst-admin-go/actions/workflows/package.yml)，正式版本发布到 [Releases](https://github.com/lcy0828/dst-admin-go/releases)。Actions 原生安装包需登录 GitHub 下载；仓库或镜像为私有时需相应访问权限。

## 选择安装方式

| 需求 | 入口 |
| --- | --- |
| 一台 Linux x86_64 服务器开服，推荐 | 下方 Docker All-in-One |
| Linux/macOS 原生安装，或增加远程 Agent | [安装与启动指南](docs/startup-guide.md) |
| 安装游戏或接入已有游戏目录 | [游戏服务端管理](docs/game-installation-management.md) |
| 安装、升级 LuaJIT2 | [LuaJIT 安装说明](docs/luajit-installation.md) |
| 多机、独立世界容器 | [部署模型](docs/deployment-profiles.md) |
| 升级、备份、回滚、源码打包 | [发布与回滚](docs/deployment-and-rollback.md) |
| 修改源码 | [开发指南](docs/development.md) |

2 核 4 GB 主机建议先运行一个 Master+Caves 房间。单机使用内置 Runtime，无需额外 Agent。Klei Linux 游戏服务端为 amd64；ARM 游戏主机需要模拟运行，不是推荐部署。

## Docker 快速安装

安装 [Docker Engine 和 Compose 插件](https://docs.docker.com/engine/install/)，准备至少 20 GB 空闲磁盘和 [Klei Cluster Token](https://accounts.klei.com/account/game/servers?game=DontStarveTogether)。

只需获取这一个仓库：

```bash
git clone --branch feature/v2-rebuild --single-branch https://github.com/lcy0828/dst-admin-go.git
cd dst-admin-go
cp deploy/docker/all-in-one.env.example deploy/docker/.env
```

编辑 `.env`：将 `DST_ADMIN_IMAGE` 设为 `ghcr.io/lcy0828/dst-admin-go/all-in-one:preview`，确认 `DST_ADMIN_DATA_ROOT`（默认 `/opt/dst`）和端口。已有部署保留原 `.env`，不要再次复制模板。稳定部署应固定版本标签或镜像 digest。

如果镜像是私有的，先用具备 `read:packages` 权限的 GitHub 凭据登录 `ghcr.io`：

```bash
docker login ghcr.io
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml pull
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml up -d
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml ps
curl -fsS http://127.0.0.1:8080/api/v2/auth/session
```

打开 `http://服务器IP:8080`，按向导创建管理员、确认环境与目录、安装或接入已有游戏、填写 Token、创建房间或导入存档。最后明确启动 Master/Caves，检查运行状态和日志，再测试玩家连接。

页面能打开只代表管理服务可用。游戏本体需要在“游戏服务端管理”中安装；LuaJIT2 为可选项。选择已有游戏目录不会移动或删除存档，存档导入是单独操作。

需要自己构建镜像时，在构建机安装 Node.js 22+、Git 和 Docker Buildx，执行：

```bash
./deploy/scripts/build-all-in-one-image.sh --tag dst-admin/all-in-one:preview
```

脚本自动获取正式前端 `master` 并内嵌到二进制。无需预先检出前端；私有源码需要 Git 访问权限。然后将 `.env` 的镜像名设为该本地标签。详见[打包说明](docs/deployment-and-rollback.md)。

## 发布镜像

| 镜像（前缀 `ghcr.io/lcy0828/dst-admin-go/`） | 用途 |
| --- | --- |
| `all-in-one` | 管理页面、控制端与本机游戏 Runtime |
| `control-plane` | 管理页面与控制端，游戏由远程节点执行 |
| `agent` | 独立远程 Agent，管理登记的容器 Runtime |
| `dst-runtime` | 独立世界容器的基础运行环境 |

四种镜像都由主仓库 CI 构建并推送，预览标签为 `preview`。Agent 不提供独立管理页面；裸机 DST 使用原生包中的 Agent，详见[Agent 安装](docs/startup-guide.md#接入远程-agent)。Agent 镜像中的身份、配置和操作状态目录 `/var/lib/dst-admin-agent` 必须持久化。

## 端口与数据

| 用途 | All-in-One 默认端口 |
| --- | --- |
| 管理页面 | `8080/tcp` |
| 玩家连接 | `10999-11020/udp` |
| Steam 鉴权 | `8766-8790/udp` |
| Steam 列表 | `27016-27040/udp` |

玩家连接 Master 的玩家端口，例如 `c_connect("服务器公网IP", 10999)`。检查云安全组、防火墙和路由映射；Web 可访问不代表 UDP 游戏端口可访问。跨机器分片还需能访问 Master 的分片通信端口。

| 宿主默认目录 | 内容 |
| --- | --- |
| `/opt/dst/control` | 配置、账号和数据库 |
| `/opt/dst/saves` | 房间和游戏存档 |
| `/opt/dst/server` | 游戏本体 |
| `/opt/dst/workshop` | 模组 |
| `/opt/dst/backups` | 备份 |
| `/opt/dst/maps` | 地图 |

自定义 `DST_ADMIN_DATA_ROOT` 会改变宿主目录，容器内仍使用 `/opt/dst`。接入其他已有目录时，先挂载到容器可见位置。升级镜像保留相同数据挂载；不要删除数据目录或使用清卷命令。迁移前正常停止房间，并备份配置、数据库、Agent 身份和存档。

## 维护

```bash
# 查看日志
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml logs --tail=200 dst-admin
# 停止服务，保留绑定挂载的数据
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml down
# 使用相同配置启动
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml up -d
```

升级前保存并停止房间、备份数据、保留旧镜像标签；拉取或构建新镜像后，修改 `.env` 的镜像标签，保持数据目录不变，再执行 `up -d`。升级后检查登录、房间列表、安装状态和游戏日志。回滚数据库须先核对版本兼容性，见[升级与回滚](docs/deployment-and-rollback.md)。

公网访问请使用 HTTPS 和强密码。提交问题时提供版本、错误代码、请求 ID 和脱敏日志，不上传密码、Token、Agent 密钥或真实存档。

## 开发与版本来源

本仓库负责发布完整产品。正式前端源码在 [GitHub dst-admin-vue](https://github.com/lcy0828/dst-admin-vue) 的 `master`；旧 Vue 2 代码保存在该仓库 `legacy/vue2-original`，不参与当前发布。

每次打包从正式前端解析一个固定提交号，同一次 CI 的镜像与原生包使用同一版本。`dst-admin -version`、原生包 `manifest.json` 和镜像 `io.dst-admin.frontend.commit` 标签可查前端提交。前端有更新后重新打包即可包含更新，已安装服务不会在运行时自动替换页面。
