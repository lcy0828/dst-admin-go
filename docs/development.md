# 从源码启动开发环境

**简体中文（默认）** | [English](development.en.md)

生产部署从 [README](../README.md) 或[安装与启动指南](startup-guide.md)开始。
开发模式才分别运行 Go API 与 Vite。下面的目录和配置放在仓库外，不使用真实的 `conf/app.conf`。

## 环境与仓库

- Go 工具链按 `go.mod`（当前为 `go1.25.13`）；管理服务依赖 CGO 和 C 编译器。
- Node.js 24 LTS、npm；使用仓库的 `package-lock.json` 执行 `npm ci`。
- 本机 Runtime 需要 tmux、可读写的独立数据目录；Linux 下载游戏/模组需要 SteamCMD。
  依赖准备见[启动指南](startup-guide.md#linux-原生部署)。
- 前后端放在相邻目录：[后端](https://github.com/lcy0828/dst-admin-go)使用 `master`，[前端](https://github.com/lcy0828/dst-admin-vue)使用 `master`。

旧版后端保留在 [`legacy/v1`](https://github.com/lcy0828/dst-admin-go/tree/legacy/v1)。

从 GitHub 获取源码：

```bash
mkdir -p workspace
cd workspace
git clone --branch master https://github.com/lcy0828/dst-admin-go.git
git clone --branch master https://github.com/lcy0828/dst-admin-vue.git dst-admin-vue-v3
cd dst-admin-go
```

下面的示例使用以下目录结构：

```text
workspace/
  dst-admin-go/
  dst-admin-vue-v3/
  dst-admin-dev/       本次开发的配置和数据，不纳入 Git
```

已有正式服务占用端口时使用其他端口和独立数据目录；不要启动第二个管理进程控制同一存档。

## 1. 准备独立配置

在后端仓库根目录执行：

```bash
mkdir -p ../dst-admin-dev/control ../dst-admin-dev/server ../dst-admin-dev/saves \
  ../dst-admin-dev/backups ../dst-admin-dev/maps \
  ../dst-admin-dev/workshop/steamapps/workshop/content/322330
cp deploy/systemd/local.conf.example ../dst-admin-dev/app.conf
chmod 600 ../dst-admin-dev/app.conf
```

手工编辑该配置，将以下值改为 `workspace/dst-admin-dev` 内的**绝对路径**：

| 配置 | 开发目录中的位置 |
| --- | --- |
| `[database] PATH` | `control/go-dont.db` |
| `[fleet] STATE_PATH` | `control/fleet` |
| `[paths] DST_SERVER_PATH`、`DST_SAVE_PATH` | `server`、`saves` |
| `[paths] DST_BACKUP_PATH`、`DST_MAP_PATH` | `backups`、`maps` |
| `[paths] DST_UGC_PATH` | `workshop/steamapps/workshop` |
| `[mod] WORKSHOP_MOD_PATH`、`WORKSHOP_CONTENT` | `workshop`、`workshop/steamapps/workshop/content/322330` |

同时把 SteamCMD、Lua、Python 解释器路径改为本机实际值。首次开发保留
`SETUP = incomplete` 和单机角色；不要复制生产数据库、Cluster Token 或 Agent 密钥。
需要真实游戏运行时，可以使用独立游戏副本或显式接入已有安装，但不能与另一个管理进程同时操作它。

## 2. 启动后端

仍在后端仓库根目录执行：

```bash
mkdir -p dist
CGO_ENABLED=0 go build -o dist/dst-map-renderer ./cmd/dst-map-renderer
DST_ADMIN_CONFIG="$PWD/../dst-admin-dev/app.conf" \
DST_ADMIN_MAP_RENDERER_PATH="$PWD/dist/dst-map-renderer" \
CGO_ENABLED=1 go run ./cmd/admin-api -addr 127.0.0.1:8000
```

另开终端检查：

```bash
curl -fsS http://127.0.0.1:8000/api/v2/auth/session
```

未登录会话返回 JSON 即证明 API 启动；此时后端根路径可能没有页面，因为开发页面由 Vite 提供。
命令行 `-addr` 决定监听地址，省略时该入口默认使用 `127.0.0.1:18000`，不会自动采用开发端口。

## 3. 启动前端

另开终端进入前端仓库：

```bash
cd ../dst-admin-vue-v3
npm ci
npm run dev -- --host 127.0.0.1 --port 5173 --strictPort
```

打开 `http://127.0.0.1:5173`。默认 `/api` 请求代理到 `http://127.0.0.1:8000`。
后端使用其他端口时，例如 `18080`，同时修改前端代理：

```bash
VITE_API_PROXY_TARGET=http://127.0.0.1:18080 \
  npm run dev -- --host 127.0.0.1 --port 15173 --strictPort
```

`VITE_API_BASE_URL` 默认 `/api`，通常不需修改。不要把密码或密钥放入 `VITE_*` 变量，
这些变量会进入浏览器代码。`npm run preview` 只预览构建产物，不替代完整生产管理服务。

## 4. 验证生产页面与 API 同源运行

前端构建完成后，由 Go 提供静态文件。先停止前面的开发 API，再从后端仓库运行：

```bash
npm --prefix ../dst-admin-vue-v3 run build
CGO_ENABLED=1 go build -o dist/dst-admin ./cmd/admin-api
DST_ADMIN_CONFIG="$PWD/../dst-admin-dev/app.conf" \
DST_ADMIN_WEB_ROOT="$PWD/../dst-admin-vue-v3/dist" \
DST_ADMIN_MAP_RENDERER_PATH="$PWD/dist/dst-map-renderer" \
  ./dist/dst-admin -addr 127.0.0.1:8000
```

此时直接访问 `http://127.0.0.1:8000`。验证管理员登录、页面刷新、游戏安装状态和任务结果。
管理服务停止后，独立 tmux 中的游戏可能继续运行；测试结束前通过房间页面正常停服。

## 代码检查

后端在相应仓库执行，按改动选择包；正式发布要求见[部署与回滚](deployment-and-rollback.md)：

```bash
DST_ADMIN_CONFIG="$PWD/deploy/systemd/local.conf.example" go test ./...
go vet ./...
```

前端：

```bash
npm test
npm run lint
npm run build
```

Agent 从后端 `./cmd/agent` 构建，配置和 `-state` 使用独立路径，不能复用正式 Agent 的身份文件。
开发启动不会自动添加远程节点；如需联调，按[远程 Agent 指南](startup-guide.md#接入远程-agent)配置。

## 完整产品打包

镜像和原生包会自动获取正式前端并嵌入管理二进制，见[打包说明](deployment-and-rollback.md)。开发时直接 `go run` 不内嵌页面，可以继续用 Vite 代理或显式设置 `DST_ADMIN_WEB_ROOT`。私有前端仓库需要 Git 访问权限；`--frontend PATH` 仅使用该目录已提交的 HEAD。
