# 安装与启动指南

**简体中文（默认）** | [English](startup-guide.en.md)

本指南对应当前源码，按“准备环境 → 启动管理服务 → 安装游戏 → 开服”执行。
发布二进制内嵌页面，由 Go 服务同时提供页面和 API；部署机器无需构建前端或运行 Vite。
首次安装和升级使用不同步骤；已有部署请先看下方的[升级与数据保护](#升级与数据保护)。

## 选择部署方式

| 需求 | 路径 | 默认管理地址 |
| --- | --- | --- |
| 一台 Linux 服务器开服，推荐 | [README：Docker All-in-One](../README.md#docker-快速安装) | `http://服务器IP:8080` |
| Linux 直接运行，使用 systemd | [Linux 原生部署](#linux-原生部署) | `http://服务器IP:8000` |
| macOS 使用本机 Steam 游戏 | [macOS 本机部署](#macos-本机部署) | `http://127.0.0.1:8000` |
| 已有管理服务，增加远程机器 | [接入远程 Agent](#接入远程-agent) | 使用现有管理页面 |
| 多台 All-in-One 集中管理 | [已有管理实例加入中心](#已有管理实例加入中心) | 使用管理中心页面 |
| 修改前后端源码 | [开发启动说明](development.md) | Vite `5173`，API `8000` |

2 核 4 GB 主机默认运行一个 Master+Caves 房间，不默认绑核，不为系统整核预留。
单机直接使用内置 Runtime，不额外启动本机 Agent。只管理远端时，可选择“仅集中管理”角色。
独立世界容器等高级部署见[部署模型](deployment-profiles.md)；它与本指南的 Native/All-in-One 安装能力有区别。

## Linux 原生部署

以下命令以 **Debian 12 amd64** 为例，从本仓库下载完整原生包，后续安装命令在解压目录执行。
如果已有 DST，沿用它的运行用户和数据目录，跳过新用户、新目录及 SteamCMD 下载步骤中已经完成的部分。
不要对运行中的存档递归修改权限或移动目录。

### 1. 安装依赖和准备用户

```bash
sudo apt-get update
sudo apt-get install -y ca-certificates curl tar tmux lua5.1 python3 \
  lib32gcc-s1 lib32stdc++6 libcurl3-gnutls build-essential sqlite3 git
```

全新部署创建专用用户。tmux 需要可执行 shell，不能使用 `nologin` 或 `false`：

```bash
sudo useradd --system --user-group --create-home --home-dir /var/lib/dst --shell /bin/bash dst
sudo install -d -o dst -g dst -m 0750 \
  /opt/dst /opt/dst/server /opt/dst/saves /opt/dst/backups /opt/dst/maps \
  /opt/dst/workshop /opt/dst/workshop/steamapps/workshop/content/322330 \
  /opt/dst/steamcmd
```

安装前先创建配置中的目录，游戏目录可以为空。游戏目录和存档目录必须分开，不能互相包含。
已有用户不重复执行 `useradd`；已有目录应先核对归属，再将后续命令中的 `dst` 和路径替换为实际值。

### 2. 准备 SteamCMD

已有可用 SteamCMD 时直接记录其绝对路径。全新部署可按 Valve 的
[SteamCMD 说明](https://developer.valvesoftware.com/wiki/SteamCMD) 安装：

```bash
sudo -u dst sh -c 'cd /opt/dst/steamcmd && curl -fL https://steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz -o steamcmd_linux.tar.gz && tar -xzf steamcmd_linux.tar.gz'
sudo -u dst /opt/dst/steamcmd/steamcmd.sh +quit
```

第二条命令应完成自更新并退出。如果提示找不到动态加载器或 32 位库，先修复依赖。
这一步没有下载 DST。稍后通过页面安装游戏；配置中的 SteamCMD 路径使用
`/opt/dst/steamcmd/steamcmd.sh`，而不是示例默认的 `/usr/games/steamcmd`。

### 3. 获取原生安装包

从主仓库 Releases 或 Package 工作流的 Artifacts 下载 `dst-admin-VERSION-linux-amd64.tar.gz` 和 `.sha256`，在目标机器校验并解压：

```bash
sha256sum -c dst-admin-VERSION-linux-amd64.tar.gz.sha256
tar -xzf dst-admin-VERSION-linux-amd64.tar.gz
cd dst-admin-VERSION-linux-amd64
./dst-admin -version
```

将 `VERSION` 换成实际文件名。版本输出应包含 `embeddedWebUI: true` 和 `frontendCommit`。包内包含页面、Agent、地图渲染器、辅助程序和安装模板，运行机器不需要 Go 或 Node.js。后续命令在解压目录执行。

如需自行构建，请按[打包说明](deployment-and-rollback.md)操作；只有构建机需要 Go、C 编译器、Git 和 Node.js。

### 4. 配置并安装

复制模板到仓库外，不使用仓库中的实际 `conf/app.conf`：

```bash
mkdir -p ../dst-admin-local
cp deploy/systemd/local.conf.example ../dst-admin-local/app.conf
chmod 600 ../dst-admin-local/app.conf
```

编辑 `../dst-admin-local/app.conf`，逐项核对：

| 配置 | 首次部署值或含义 |
| --- | --- |
| `[deployment] PACKAGING` | `native` |
| `[fleet] LOCAL_EXECUTOR_ENABLED` | `true` |
| `[fleet] CONTROLLER_ENABLED`、`MEMBER_ENABLED` | 单机均为 `false` |
| `[database] PATH` | `/var/lib/dst-admin/go-dont.db` |
| `[fleet] STATE_PATH` | `/var/lib/dst-admin/fleet` |
| `[paths] DST_SERVER_PATH` | `/opt/dst/server`，或实际已有游戏目录 |
| `[paths] DST_SAVE_PATH` | `/opt/dst/saves`，保存各个 Cluster 的父目录 |
| `[paths] DST_BACKUP_PATH`、`DST_MAP_PATH` | `/opt/dst/backups`、`/opt/dst/maps` |
| `[paths] DST_UGC_PATH` | `/opt/dst/workshop/steamapps/workshop` |
| `[mod] STEAM_CMD_PATH` | 实际 SteamCMD 绝对路径 |
| `[mod] WORKSHOP_MOD_PATH` | `/opt/dst/workshop` |
| `[mod] WORKSHOP_CONTENT` | `/opt/dst/workshop/steamapps/workshop/content/322330` |
| `[map] RENDERER_PATH` | `/usr/local/bin/dst-map-renderer` |

首次安装保留模板的 `SETUP = incomplete`，在页面创建管理员。已有服务必须保留原配置和数据库，
不能用初始模板重置。房间管理中的 Cluster Token 与 Agent 连接密钥是不同用途的凭据。

```bash
sudo deploy/scripts/install-native-local.sh \
  --binary "$PWD/dst-admin" \
  --renderer "$PWD/dst-map-renderer" \
  --config "$PWD/../dst-admin-local/app.conf" \
  --user dst
sudo systemctl enable --now dst-admin-local
sudo systemctl status dst-admin-local --no-pager
curl -fsS http://127.0.0.1:8000/api/v2/auth/session
```

服务应为 `active`，接口返回 JSON。默认监听 `0.0.0.0:8000`。systemd 已明确指定
`DST_ADMIN_CONFIG=/var/lib/dst-admin/app.conf` ；页面由二进制内嵌提供，生效配置以后位于这个安装位置。
`HTTP_PORT` 不覆盖启动命令中的 `-addr`。修改监听地址时使用 systemd override，并保留其他服务配置。

```bash
journalctl -u dst-admin-local -n 100 --no-pager
sudo systemctl restart dst-admin-local
```

打开页面后按[首次开服](#首次开服与已有存档)完成游戏安装。反向代理示例见
[部署与回滚](deployment-and-rollback.md#nginx-同源反向代理)。

## 接入远程 Agent

控制端提供页面；Agent 在远程机器执行下载、安装和房间操作。Agent 本身没有独立管理页面。
Agent 主动连接控制端的 `/agent`，不需要为普通命令额外开放 Agent 入站 TCP 端口。
游戏 UDP 端口仍需在实际运行世界的机器上开放。

### 1. 开启控制端

1. 打开“机器管理”（`/agents/list`）的“部署角色”。选择“本机 + 集中管理”；
   不运行本机世界时选择“仅集中管理”。角色切换前先停止将被关闭的本机运行目标上的世界。
2. 保存后重启管理服务。systemd 使用 `systemctl restart dst-admin-local`；
   All-in-One 使用 README 中带相同环境文件的 Compose 命令，将 `up -d` 换成 `restart dst-admin`。
   All-in-One 重启会停止容器内世界，服务恢复后检查并按需启动房间。
3. 打开“Agent 安全设置”（`/agents/security`）。首次接入且尚无 Agent 时生成新密钥，
   保存本次显示的明文，用于远端 `[agent] SECURITY_KEY`。
4. 确认远端能访问控制端：局域网可用 `ws://控制端IP:8080/agent`，
   原生默认端口为 `8000`；通过 HTTPS 反向代理时使用 `wss://域名/agent`。

已有 Agent 时复用妥善保存的连接密钥，不要为新增节点随意轮换。页面只显示旧密钥的掩码；
丢失密钥时需按[密钥轮换流程](deployment-and-rollback.md#密钥轮换)处理全部相关节点。
密钥写入权限为 `0600` 的配置文件，不拼进 WebSocket URL 或命令行。

### 2. 在远程机器准备环境

远端需要同样的 tmux、游戏运行库、SteamCMD、运行用户和数据目录，按
[Linux 原生部署第 1、2 步](#linux-原生部署)准备。只运行 Agent 的机器不需要 Node.js、
前端文件或管理服务数据库。可以在另一台构建机上生成匹配目标系统/架构的 Agent 后复制过去。

原生包已包含 `dst-admin-agent` 和 `deploy/`。将匹配远端系统架构的完整包传到远端，解压后执行：

```bash
mkdir -p ../dst-agent-local
cp deploy/systemd/agent.conf.example ../dst-agent-local/agent.conf
chmod 600 ../dst-agent-local/agent.conf
```

编辑配置，至少保留连接和 Runtime 两部分：

```ini
[agent]
SERVER_URL = wss://dst.example.com/agent
SECURITY_KEY = 在此填写控制端生成的连接密钥

[runtime.native]
DRIVER = native
SAVE_PATH = /opt/dst/saves
SERVER_PATH = /opt/dst/server
STEAMCMD_PATH = /opt/dst/steamcmd/steamcmd.sh
UGC_PATH = /opt/dst/workshop/steamapps/workshop
WORKSHOP_CONTENT_PATH = /opt/dst/workshop/steamapps/workshop/content/322330
MOD_CACHE_PATH = /var/lib/dst-admin-agent/mod-cache
MOD_STATE_PATH = /var/lib/dst-admin-agent/mod-state
SERVER_MODE = 64
```

这些路径都属于 **Agent 所在机器**。`[runtime.native]` 的安装 ID 为 `native`。
只配置 `[agent]` 会建立连接，但无法管理未登记的游戏位置。多份安装使用不同的
`[runtime.名称]`，存档根目录不能重叠或由多个写入者同时管理。

### 3. 安装、启动并验收

```bash
sudo deploy/scripts/install-native-agent.sh \
  --binary "$PWD/dst-admin-agent" \
  --config "$PWD/../dst-agent-local/agent.conf" \
  --user dst
sudo systemctl enable --now dst-admin-agent
sudo systemctl status dst-admin-agent --no-pager
journalctl -u dst-admin-agent -n 100 --no-pager
```

安装后的生效配置是 `/var/lib/dst-admin-agent/agent.conf`，操作状态为同目录下的
`runtime-state.json`，二进制在 `bin/dst-admin-agent`。`/etc/dst-admin/agent.env` 是可选项；
里面的连接地址和密钥会覆盖配置文件，排障时检查是否存在旧值。

回到控制端完成以下检查：

1. “机器管理”中 Agent 在线，安装目录与远端配置一致。
2. 只有一份登记安装时通常自动采用；有多份时，在该机器的运行配置中选择正确安装。
3. “游戏服务端管理”中选中该机器，执行安装或接入已有目录，任务成功且显示版本。
4. 新建或导入房间，将世界放到该 Agent，启动后确认世界状态及日志。

“Agent 在线”只证明连接正常，不代表游戏已安装或世界已启动。游戏安装管理需要新版
`runtime.game-install.v1`，LuaJIT 管理需要 `runtime.luajit.v2`；出现升级提示时使用与控制端匹配的源码构建 Agent。
停止 Agent 通常不会停止已经运行的 native 世界，停服应在房间页面执行并确认完成。

## 已有管理实例加入中心

如果远端已运行 All-in-One 或原生完整管理服务，可以直接把它的部署角色改为
“加入管理中心”，填写上级 `ws(s)://…/agent` 和节点连接密钥，保存并重启该实例。
它使用内嵌 Member 连接上级，继续管理自身原有文件。不要再安装第二个独立 Agent 控制相同存档。
加入后从管理中心执行写操作；远端页面保留只读访问。

环境变量锁定角色时，页面会提示由环境管理，应修改部署环境再重启。
内嵌 Member 与独立 Agent 的变量名不同，详见[部署角色变量表](deployment-profiles.md#fleet-roles)。

## macOS 本机部署

macOS 使用当前登录用户的 Steam 游戏文件。Linux 一键下载和 LuaJIT 安装不适用于 macOS。

1. 用 `brew install tmux` 安装 tmux。
2. 通过 Steam 安装 DST；Apple Silicon 运行 x86_64 游戏需准备 Rosetta 2。
3. 下载匹配架构的 macOS 原生包（Apple Silicon 使用 `darwin-arm64`），用 `shasum -a 256 -c 文件名.tar.gz.sha256` 校验并解压；后续命令在解压目录执行。Intel Mac 可按打包指南在本机构建 `darwin-amd64`。
4. 将 `local.conf.example` 复制到仓库外编辑。配置路径必须是当前用户可写的绝对路径，
   INI 中不要使用 `~` 或 `$HOME` 代替真实路径。

首次部署在解压目录准备配置：

```bash
mkdir -p ../dst-admin-local
cp deploy/systemd/local.conf.example ../dst-admin-local/app.conf
chmod 600 ../dst-admin-local/app.conf
```

| 配置 | macOS 示例（将 `alice` 换成实际用户名） |
| --- | --- |
| `[database] PATH` | `/Users/alice/Library/Application Support/DST Admin/go-dont.db` |
| `[fleet] STATE_PATH` | `/Users/alice/Library/Application Support/DST Admin/fleet` |
| `[paths] DST_SERVER_PATH` | `/Users/alice/Library/Application Support/Steam/steamapps/common/Don't Starve Together` |
| `[paths] DST_SAVE_PATH` | `/Users/alice/Documents/Klei/DoNotStarveTogether`，以实际存档位置为准 |
| `[paths] DST_BACKUP_PATH`、`DST_MAP_PATH` | 用户自建的备份和地图目录 |
| `[paths] DST_UGC_PATH` | 用户自建的 SteamCMD 库下 `steamapps/workshop` |
| `[mod] WORKSHOP_MOD_PATH`、`WORKSHOP_CONTENT` | 同一 SteamCMD 库根目录及其 `steamapps/workshop/content/322330` |
| `[mod] STEAM_CMD_PATH` | 已安装的 SteamCMD 绝对路径；暂不下载模组时可留空 |
| `[mod] LUA_BINARY`、`PYTHON_BINARY` | 已安装解释器的绝对路径；外部 Lua 解析回退需另行安装 Lua 5.1 |
| `[map] RENDERER_PATH` | `/Users/alice/Library/Application Support/DST Admin/bin/dst-map-renderer` |

创建所填写但尚不存在的目录后安装，**不要使用 sudo**：

```bash
deploy/scripts/install-macos-local.sh \
  --binary "$PWD/dst-admin" \
  --renderer "$PWD/dst-map-renderer" \
  --config "$PWD/../dst-admin-local/app.conf"
launchctl print "gui/$(id -u)/top.luocaiyi.dst-admin-local"
curl -fsS http://127.0.0.1:8000/api/v2/auth/session
```

安装脚本会立即启动 LaunchAgent。生效配置、日志分别在
`~/Library/Application Support/DST Admin/app.conf` 和该目录的 `logs/`。
Steam 客户端维护的游戏通过 Steam 更新；更多路径说明见[macOS 游戏支持](macos-dedicated-server.md)。
只运行 macOS Agent 的安装步骤见[macOS launchd Agent](container-and-native-deployment.md#macos-launchd-agent)。

## 首次开服与已有存档

首次访问没有管理员的实例会自动进入 `/setup` 初始化向导，依次完成“管理员账户 →
管理方式 → 环境与目录 → 游戏服务端 → 准备房间 → 完成检查”。账号创建需要输入用户名、
密码和确认密码；页面按服务端配置的密码规则校验，默认中文，可切换 English。

向导进度保存到数据库，刷新页面、重新登录或重启管理服务后可以继续。
角色及路径变更需要重启时，会暂缓向导中的安装和建房操作，重启后点击“重新检查”。
游戏步骤可安装或接入已有目录，LuaJIT 为可选项；存档导入只创建新房间，不提供覆盖已有房间的模式。
远程房间可从“配置运行节点”选择机器并完成部署，然后从页头返回向导检查；仅创建房间目录不会被当作已准备就绪。
向导内上传存档用于本机开服。纯控制端接入已有存档时，请使用 Agent 配置的存档目录并刷新识别；新房间配置下发不会传输本地 Session 存档。
现有管理员升级后不会被强制重新初始化，可从页面顶部“初始化向导”手动打开。

完成向导不等于世界已经开服。最后会列出未完成事项，可以稍后开服，或前往房间控制明确启动。
向导不会因完成步骤自动下载游戏、启动或停止世界、重建房间或清理存档。

1. 访问管理页面，通过初始化向导创建管理员。
2. 打开“游戏服务端管理”，确认选中的机器和安装位置。新机器点击“安装游戏服务端”；
   已有游戏可直接配置其路径，或在空的登记位置选择“使用已有服务端”并检测、确认。
3. 如需 LuaJIT，在游戏安装成功后按[LuaJIT 安装说明](luajit-installation.md)安装；原版游戏不要求安装 LuaJIT。
4. 在房间管理中创建房间或导入已有存档，填写有效 Cluster Token，配置 Master/Caves 和端口。
5. 启动世界，确认世界进入“运行中”，日志显示世界加载完成，再测试玩家实际连接。

接入游戏文件与导入存档是两个操作。页面继续使用配置的存档目录，不会因为选择游戏目录而搬动或删除存档。
已有目录接入与在线安装的条件见[游戏安装管理](game-installation-management.md)。
容器内填写容器可见路径；宿主机任意路径不会自动出现在容器里。

Web 端口通过不代表游戏端口通过。防火墙放行实际世界配置的玩家、Steam 鉴权、Steam 列表 UDP 端口；
跨机器世界还需让次分片能访问 Master 的分片通信地址与端口。All-in-One 默认端口范围见 README。

## 升级与数据保护

| 部署 | 正常启停与日志 | 升级时必须保留 |
| --- | --- | --- |
| All-in-One | README 中同一 `--env-file` 的 Compose 命令 | 宿主数据根目录、环境文件和旧镜像 |
| Linux 本机 | `systemctl restart dst-admin-local`；`journalctl -u dst-admin-local` | `/var/lib/dst-admin` 及配置指向的全部数据目录 |
| Linux Agent | `systemctl restart dst-admin-agent`；`journalctl -u dst-admin-agent` | `/var/lib/dst-admin-agent`、可选 `agent.env`、游戏及存档目录 |
| macOS 本机 | `launchctl kickstart -k "gui/$(id -u)/top.luocaiyi.dst-admin-local"` | Application Support 中的配置/状态以及外部游戏、存档目录 |

更新前创建可恢复的存档、数据库和配置备份，并保留旧安装包或镜像。
复制存档前先正常停止相关世界；停止 native 管理服务或 Agent 不等于停服。
SQLite 使用停写后的复制或 `.backup`，不能只复制正在写入的数据库主文件。

安装脚本会复制传入的配置到生效位置，因此升级时应先把**当前生效配置**备份到另一文件，
审核后用该副本安装；不要再次传入初始模板，也不要把源和目标设成同一个文件。
Agent 配置中持久化的身份、密钥，以及 `runtime-state.json` 必须保留。
安装脚本安装完成后，Linux 的已有服务需显式重启；macOS 安装脚本会立即重启对应 LaunchAgent。

同一存档只能有一个本地控制者。不要通过删除 `owner.lock`、tmux socket 或存档文件解决冲突。
正常停止原管理者并保持运行用户、存档路径一致，再启动新的管理进程。
完整发布与回滚见[部署与回滚](deployment-and-rollback.md)。

## 启动故障速查

| 现象 | 检查与处理 |
| --- | --- |
| 找不到 `app.conf` | 确认 `DST_ADMIN_CONFIG` 指向存在的配置；仅设置 WorkingDirectory 不会自动加载其中的 `app.conf` |
| `go-sqlite3 requires cgo` | 管理服务以 `CGO_ENABLED=1` 重新构建，并安装 C 编译器 |
| 数据目录不存在或 Permission denied | 核对绝对路径、服务用户、父目录访问权限；本机首次启动前创建配置中的空目录 |
| SteamCMD 不可用 | 使用服务账号执行所登记的绝对路径；检查 32 位库、目录写权限和下载网络 |
| 管理 API 正常、页面 404 | 检查 `DST_ADMIN_WEB_ROOT` 中是否有构建后的 `index.html` |
| Agent 不在线 | 检查控制端角色是否重启生效、`/agent` 地址、双方密钥、环境变量覆盖和反向代理 WebSocket |
| Agent 在线却不能安装 | 检查 `[runtime.名称]`、SteamCMD、新版 Agent 能力以及页面是否选中正确安装 |
| `RUNTIME_OWNER_CONFLICT` | 检查同一存档是否同时交给本机管理服务和另一个 Agent；正常停止多余管理者 |
| 游戏运行时拒绝安装/校验 | 在房间页面停止所有使用该游戏安装的世界，确认停止后重试 |
| LuaJIT 系统库不兼容 | 选择适合节点系统的兼容构建，详见 LuaJIT 文档；普通启动不会自动修复 |
| 世界运行但玩家连不上 | 核对 Master 玩家 UDP 端口、公网地址、防火墙及路由器映射 |
