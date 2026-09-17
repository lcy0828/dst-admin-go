# DST Admin

**简体中文** | [English](README.en.md)

《饥荒联机版》服务器管理面板，支持房间、模组、玩家、日志、存档备份和远程 Agent 管理。镜像和原生安装包内置前端，默认中文，支持英文。

## Docker 快速安装

推荐 Linux x86_64，至少 2 核、4 GB 内存、20 GB 空闲磁盘，安装 [Docker 和 Compose](https://docs.docker.com/engine/install/)。

```bash
git clone https://github.com/lcy0828/dst-admin-go.git
cd dst-admin-go/deploy/docker
cp all-in-one.env.example .env
```

编辑 `.env`，设置镜像和数据目录：

```ini
DST_ADMIN_IMAGE=ghcr.io/lcy0828/dst-admin-go/all-in-one:preview
DST_ADMIN_DATA_ROOT=/opt/dst
```

启动：

```bash
docker compose --env-file .env -f compose.all-in-one.yaml up -d
```

打开 `http://服务器IP:8080`，按初始化向导创建管理员、安装或接入游戏服务端、填写 [Klei Token](https://accounts.klei.com/account/game/servers?game=DontStarveTogether)，然后创建房间或导入存档，启动 Master/Caves。

默认端口：`8080/tcp`（管理页面）、`10999-11020/udp`（玩家）、`8766-8790/udp` 和 `27016-27040/udp`（Steam）。在防火墙中放行相应端口。

数据默认保存在 `/opt/dst`，其中 `saves/` 为存档、`control/` 为配置和数据库。升级时保留数据目录和原 `.env`。

## 其他部署方式

| 方式 | 入口 |
| --- | --- |
| Linux / macOS 原生安装 | [下载安装包](https://github.com/lcy0828/dst-admin-go/actions/workflows/package.yml) · [安装指南](docs/startup-guide.md) |
| 远程 Agent | [接入指南](docs/startup-guide.md#接入远程-agent) |
| Controller / 独立世界容器 | [部署说明](docs/deployment-profiles.md) |

镜像前缀为 `ghcr.io/lcy0828/dst-admin-go/`，当前标签为 `preview`：

| 镜像 | 用途 |
| --- | --- |
| `all-in-one` | 管理页面、控制端和本机游戏 |
| `control-plane` | 管理页面和控制端 |
| `agent` | 管理远程容器 Runtime |
| `dst-runtime` | 独立世界运行环境 |

## 文档

- [游戏服务端管理](docs/game-installation-management.md) · [LuaJIT2](docs/luajit-installation.md)
- [升级、回滚与源码打包](docs/deployment-and-rollback.md)
- [开发指南](docs/development.md) · [前端源码](https://github.com/lcy0828/dst-admin-vue)
