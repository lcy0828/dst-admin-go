<p align="center">
  <img src="docs/assets/readme/banner.svg" alt="DST Admin — 饥荒联机版服务器管理面板" width="100%">
</p>

<p align="center">
  <a href="https://github.com/lcy0828/dst-admin-go/stargazers"><img src="https://img.shields.io/github/stars/lcy0828/dst-admin-go?style=flat&amp;label=Stars&amp;color=e7b76e&amp;labelColor=20362a" alt="GitHub Stars"></a>
  <a href="https://github.com/lcy0828/dst-admin-go/forks"><img src="https://img.shields.io/github/forks/lcy0828/dst-admin-go?style=flat&amp;label=Forks&amp;color=8ac496&amp;labelColor=20362a" alt="GitHub Forks"></a>
  <a href="https://github.com/lcy0828/dst-admin-go/actions/workflows/ci.yml"><img src="https://github.com/lcy0828/dst-admin-go/actions/workflows/ci.yml/badge.svg?branch=master&amp;event=push" alt="CI"></a>
  <a href="https://github.com/lcy0828/dst-admin-go/actions/workflows/package.yml"><img src="https://github.com/lcy0828/dst-admin-go/actions/workflows/package.yml/badge.svg?branch=master&amp;event=push" alt="Package"></a>
</p>

<p align="center"><strong>从第一次开服，到每一天的冒险。</strong></p>
<p align="center">
  在浏览器里管理《饥荒联机版》的世界、模组、玩家与存档。<br>
  一台机器即可开始，也能通过 Agent 管理远程服务器。
</p>

<p align="center">
  <strong>简体中文</strong> · <a href="README.en.md">English</a>
</p>
<p align="center">
  <a href="#docker-快速安装">快速安装</a> ·
  <a href="#支持环境">支持环境</a> ·
  <a href="#界面预览">界面预览</a> ·
  <a href="#动态演示">动态演示</a> ·
  <a href="docs/startup-guide.md">部署指南</a> ·
  <a href="docs/luajit-installation.md">LuaJIT2</a> ·
  <a href="https://github.com/lcy0828/dst-admin-go/issues">反馈问题</a>
</p>

<p align="center">
  <a href="docs/assets/readme/dashboard.zh.webp"><img src="docs/assets/readme/dashboard.zh.webp" alt="服务总览：Master 与 Caves 运行状态、季节、在线玩家与备份入口" width="100%"></a>
  <br><sub>当前版本界面 · 演示数据 · 点击查看大图</sub>
</p>

## 开服与管理，都在这里

| 从零开始 | 日常管理 |
| --- | --- |
| **安装游戏与 LuaJIT2**<br>在页面安装、更新服务端，也可接入本机已有安装。 | **管理房间与世界**<br>创建房间，启动或停止 Master、Caves，查看运行状态。 |
| **配置世界与模组**<br>调整房间和世界设置，管理模组的启用、配置与更新。 | **备份与恢复存档**<br>创建备份、导入已有存档，需要时恢复世界。 |
| **接入远程机器**<br>通过 Agent 管理其他主机，查看各节点的安装与资源状态。 | **掌握游戏动态**<br>查看在线玩家、季节、日志与聊天记录，执行控制台命令。 |

镜像和原生安装包均内置管理页面。默认中文，支持英文、多套配色与深色模式；动态天气默认开启，跟随游戏中的季节、昼夜和降水，也可关闭或手动预览。

## 支持环境

管理页面可通过 Windows、macOS、Linux 的现代浏览器访问。下表说明**部署管理端 / Agent、运行游戏的机器**需要什么环境。

| 系统 / 架构 | 管理端 / Agent 安装方式 | 本机运行 DST |
| --- | --- | --- |
| **Linux x86_64（amd64）** | **推荐**：Docker 或 `linux-amd64` 原生包 | 支持，可在页面安装、更新游戏 |
| **macOS Apple Silicon（arm64）** | `darwin-arm64` 原生包 | 支持，需 Steam 游戏、tmux 和 Rosetta 2；通过 Steam 更新游戏 |
| macOS Intel（x86_64） | 自行构建 `darwin-amd64`，无预编译包，未纳入发布 CI 验证 | 使用 Steam 游戏与 tmux |
| Windows | 暂不支持原生部署，无 Windows 安装包 | 暂不支持管理 Windows 原生游戏进程 |
| Linux ARM64 | 可自行构建控制端，未提供预编译包，未纳入发布 CI 验证 | DST Linux 服务端不提供原生 ARM 版本 |

[正式版下载](https://github.com/lcy0828/dst-admin-go/releases/latest)提供 Linux x86_64、macOS Apple Silicon 的完整原生包和独立 Agent 包。四种 Docker 镜像均为 `linux/amd64`；Apple Silicon 推荐[原生 macOS 部署](docs/startup-guide.md#macos-本机部署)。Windows 可在 Linux x86_64 虚拟机内按 Linux 方式部署；WSL2 / Docker Desktop 尚未纳入发布验证。

**LuaJIT2 页面安装**目前支持 Linux x86_64 的 64 位 DST，适用于原生运行和常规 All-in-One；macOS、Windows、Linux ARM 和独立分片容器暂不支持。详见 [LuaJIT2 安装说明](docs/luajit-installation.md)。

LuaJIT2 来源于 [fesily/DontStarveLuaJIT2](https://github.com/fesily/DontStarveLuaJIT2)，感谢原作者 [fesily](https://github.com/fesily)。本项目提供安装与版本管理，内置兼容构建也基于该上游项目；原项目说明与许可请见其仓库。

## 界面预览

真实产品界面，使用演示数据。点击图片查看大图。

<table>
  <tr>
    <td width="50%" valign="top"><strong>浏览创意工坊</strong><br><a href="docs/assets/readme/workshop.zh.webp"><img src="docs/assets/readme/workshop.zh.webp" alt="浏览创意工坊" width="100%"></a><br><sub>查看模组封面与简介，搜索、下载并添加到房间。</sub></td>
    <td width="50%" valign="top"><strong>房间与世界模组</strong><br><a href="docs/assets/readme/mods.zh.webp"><img src="docs/assets/readme/mods.zh.webp" alt="房间与世界模组" width="100%"></a><br><sub>查看版本和世界启用状态，统一或分别配置模组。</sub></td>
  </tr>
  <tr>
    <td width="50%" valign="top"><strong>游戏服务端管理</strong><br><a href="docs/assets/readme/game.zh.webp"><img src="docs/assets/readme/game.zh.webp" alt="游戏服务端管理" width="100%"></a><br><sub>查看安装状态，安装、更新或接入已有服务端。</sub></td>
    <td width="50%" valign="top"><strong>LuaJIT2 安装</strong><br><a href="docs/assets/readme/luajit.zh.webp"><img src="docs/assets/readme/luajit.zh.webp" alt="LuaJIT2 安装" width="100%"></a><br><sub>选择版本，检查上游更新，按需导入安装包。</sub></td>
  </tr>
  <tr>
    <td width="50%" valign="top"><strong>创建管理员</strong><br><a href="docs/assets/readme/setup.zh.webp"><img src="docs/assets/readme/setup.zh.webp" alt="创建管理员" width="100%"></a><br><sub>首次启动创建自己的管理账户。</sub></td>
    <td width="50%" valign="top"><strong>主题配色与深色模式</strong><br><a href="docs/assets/readme/themes.zh.webp"><img src="docs/assets/readme/themes.zh.webp" alt="主题配色与深色模式" width="100%"></a><br><sub>浅色、深色与配色预设，也支持自定义主题色。</sub></td>
  </tr>
  <tr>
    <td width="50%" valign="top"><strong>存档备份与恢复</strong><br><a href="docs/assets/readme/backups.zh.webp"><img src="docs/assets/readme/backups.zh.webp" alt="存档备份与恢复" width="100%"></a><br><sub>查看完整房间备份，按需恢复或导入存档。</sub></td>
    <td width="50%" valign="top"><strong>玩家管理</strong><br><a href="docs/assets/readme/players.zh.webp"><img src="docs/assets/readme/players.zh.webp" alt="玩家管理" width="100%"></a><br><sub>查看角色、在线状态与游玩记录。</sub></td>
  </tr>
</table>

模组名称与封面来自对应的 Steam Workshop 页面，见[素材来源](docs/assets/readme/CREDITS.md)。

## 动态演示

真实界面录制，使用演示任务和版本数据。

<details open>
<summary><strong>更新模组并重启世界</strong></summary>

确认更新 → 下载模组 → 查看 Master / Caves 重启进度 → 世界恢复运行。

<p align="center">
  <a href="docs/assets/readme/mod-update.zh.webp"><img src="docs/assets/readme/mod-update.zh.webp" alt="更新模组并重启世界" width="100%"></a>
</p>

</details>

<details open>
<summary><strong>主题切换与动态天气</strong></summary>

切换深色模式，预览季节与昼夜光效、下雨和下雪；可调节强度、暂停或结束预览。

<p align="center">
  <a href="docs/assets/readme/weather.zh.webp"><img src="docs/assets/readme/weather.zh.webp" alt="主题切换与动态天气" width="100%"></a>
</p>

</details>

## Docker 快速安装

适合一台机器同时运行面板和游戏。推荐 **Linux x86_64 · 2 核 / 4 GB 内存起 · 20 GB 空闲磁盘**。

尚未安装 Docker 时，以 root 用户在 Bash 中执行对应命令，安装 Docker 和 Compose；已有环境可跳过。也可按 [Docker 官方文档](https://docs.docker.com/engine/install/)安装。

**国内服务器（阿里云软件源）：**

```bash
bash <(curl -Ls https://get.docker.com) --mirror Aliyun
```

**海外服务器（官方软件源）：**

```bash
bash <(curl -Ls https://get.docker.com)
```

然后启动面板：

```bash
mkdir -p /opt/dst
cd /opt/dst
curl -fL https://ghfast.top/https://raw.githubusercontent.com/lcy0828/dst-admin-go/master/deploy/docker/compose.all-in-one.yaml -o compose.yaml
docker compose up -d
```

部署文件默认通过 [GHFast](https://ghfast.top/) 代理下载，也可使用 [GitHub 直连地址](https://raw.githubusercontent.com/lcy0828/dst-admin-go/master/deploy/docker/compose.all-in-one.yaml)。

默认使用**阿里云镜像**，无需克隆仓库或配置 `.env`。其他地区可将 `compose.yaml` 中的 `image` 改为 `lcy0828/dst-admin-go:latest`，使用 Docker Hub。端口和数据目录也直接在此文件中调整。

打开 **`http://服务器IP:8080`**，跟随初始化向导完成：

**创建管理员 → 安装或接入游戏 → 填写 [Klei Token](https://accounts.klei.com/account/game/servers?game=DontStarveTogether) → 创建房间或导入存档 → 启动世界**

> 数据默认保存在 `/opt/dst`：`saves/` 是存档，`control/` 是配置和数据库。升级时保留数据目录和 `compose.yaml` 的挂载配置，在部署目录执行 `docker compose pull && docker compose up -d`。

<details>
<summary><strong>需要开放哪些端口？</strong></summary>

在主机防火墙和云服务器安全组中放行实际使用的端口。默认映射如下：

| 用途 | 端口 |
| --- | --- |
| 管理页面 | `8080/tcp` |
| 玩家连接 | `10999-11020/udp` |
| Steam 通信 | `8766-8790/udp`、`27016-27040/udp` |

</details>

## 选择适合你的部署

| 使用场景 | 部署方式 |
| --- | --- |
| 一台机器开服 | **All-in-One**，按上面的步骤开始 |
| 直接运行二进制 | [原生安装指南](docs/startup-guide.md) · [下载安装包](https://github.com/lcy0828/dst-admin-go/releases/latest) |
| 管理更多机器 | [接入远程 Agent](docs/startup-guide.md#接入远程-agent) |
| 控制端与游戏分开部署 | [部署方案](docs/deployment-profiles.md) |

<details>
<summary><strong>可用的 Docker 镜像（包含 Agent）</strong></summary>

正式版使用 `latest`，也可指定 `vX.Y.Z` 固定版本。Docker Hub 的四种服务统一放在 [`lcy0828/dst-admin-go`](https://hub.docker.com/r/lcy0828/dst-admin-go)，阿里云使用 `registry.cn-hangzhou.aliyuncs.com/dstadmin/dst-admin-go`。

| 用途 | GHCR 镜像后缀 | Docker Hub / 阿里云标签 |
| --- | --- | --- |
| 管理页面、控制端和本机游戏 | `all-in-one:latest` | `latest` |
| 管理页面和控制端 | `control-plane:latest` | `controller-latest` |
| 远程容器 Runtime 管理 | `agent:latest` | `agent-latest` |
| 独立世界运行环境 | `dst-runtime:latest` | `runtime-latest` |

国内用户推荐阿里云，其他地区推荐 Docker Hub；GHCR 也保留同步，前缀为 `ghcr.io/lcy0828/dst-admin-go/`。例如 Agent 的固定版本标签为 `agent-v1.0.0`。`preview` 仅供测试，镜像同步配置见[发布说明](docs/deployment-and-rollback.md#github-actions)。

远程机器直接运行原生游戏进程时，使用原生 Agent 安装包。具体配置见[部署指南](docs/startup-guide.md#接入远程-agent)。

</details>

## 继续探索

- **开服与维护**：[游戏服务端安装](docs/game-installation-management.md) · [LuaJIT2 安装](docs/luajit-installation.md) · [升级与回滚](docs/deployment-and-rollback.md) · [游戏工作台](docs/game-workbench.md)
- **参与开发**：[开发指南](docs/development.md) · [前端源码](https://github.com/lcy0828/dst-admin-vue) · [问题反馈](https://github.com/lcy0828/dst-admin-go/issues)

## Star 趋势

<p align="center">
  <a href="https://www.star-history.com/?repos=lcy0828%2Fdst-admin-go&amp;type=date">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/svg?repos=lcy0828/dst-admin-go&amp;type=Date&amp;theme=dark">
      <img src="https://api.star-history.com/svg?repos=lcy0828/dst-admin-go&amp;type=Date" alt="GitHub Star 数量随时间的变化" width="100%">
    </picture>
  </a>
</p>
