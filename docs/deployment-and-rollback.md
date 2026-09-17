# 发布与回滚

**简体中文（默认）** | [English](deployment-and-rollback.en.md)

安装入口是[主 README](../README.md)。镜像和原生包都内置页面，升级时替换完整产物。配置、数据库、Agent 身份和游戏存档独立保留。

## 下载与版本

- [Package 工作流](https://github.com/lcy0828/dst-admin-go/actions/workflows/package.yml) 构建 Linux amd64 和 macOS arm64 原生包，并将测试通过的镜像推送到 GHCR；配置镜像仓库凭据后同步发布到 Docker Hub 和阿里云，不重复构建。
- 正式部署使用 `ghcr.io/lcy0828/dst-admin-go/all-in-one:latest` 或 Docker Hub 的 `lcy0828/dst-admin-go:latest`，需要固定版本时将 `latest` 替换为 `vX.Y.Z`。Docker Hub 的控制端、Agent、Runtime 标签分别加 `controller-`、`agent-`、`runtime-` 前缀，例如 `agent-latest`、`agent-v1.0.0`。
- 分支发布使用 `ghcr.io/lcy0828/dst-admin-go/all-in-one:preview`、`control-plane:preview`、`agent:preview` 和 `dst-runtime:preview`；同次构建另有 `sha-后端完整提交号` 标签。前端或手动重建仍可能改变同一后端提交的产物，精确复现请固定镜像 digest 与前端提交。
- `vX.Y.Z` 标签触发正式发布：生成版本镜像，所有原生包与镜像检查通过后更新四类镜像的 `latest` 标签，并创建包含 `.tar.gz` 与 SHA-256 的 GitHub Release。`vX.Y.Z-rc.N` 等候选版只发布对应版本和预发布 Release，不覆盖 `latest`。
- `dst-admin -version` 输出后端版本、提交、前端提交和 `embeddedWebUI`；原生 `manifest.json` 另记录工具链、锁文件哈希。镜像可通过 `docker image inspect` 查看 `io.dst-admin.frontend.commit`。

## 从一个仓库打包

构建机需要 Git、Node.js 22+。原生构建另外需要 Go 工具链（见 `go.mod`）和 C 编译器；镜像构建需要 Docker Buildx，Go/Node 编译在镜像构建阶段进行。

```bash
# 包含最新正式前端的原生包，自动使用当前系统/架构
node deploy/scripts/build-native-release.mjs --version preview-local --output ./dist

# 包含页面的镜像
node deploy/scripts/build-image.mjs --kind all-in-one --tag dst-admin/all-in-one:preview
node deploy/scripts/build-image.mjs --kind control-plane --tag dst-admin/control-plane:preview

# 远程执行端与独立世界运行镜像
node deploy/scripts/build-image.mjs --kind agent --tag dst-admin/agent:preview
node deploy/scripts/build-image.mjs --kind dst-runtime --tag dst-admin/dst-runtime:preview
```

默认从 [GitHub 前端仓库](https://github.com/lcy0828/dst-admin-vue)的 `master` 获取最新已提交源码，并在本次构建中固定 SHA。前端下载、`npm ci` 或构建失败会让打包失败，不使用工作区旧 `dist/` 顶替。原生管理二进制使用 `webui` 构建标签内嵌页面，生成文件不会提交到 Git。

可选参数：

| 参数 | 用途 |
| --- | --- |
| `--frontend-ref SHA` | 固定已知兼容的前端版本 |
| `--frontend PATH` | 使用指定前端检出的已提交 HEAD，忽略未提交修改 |
| `--frontend-repository URL` | 指定可认证的源码地址，例如 GitHub SSH URL |
| `--version VERSION` | 写入发布版本 |
| `--platform linux/amd64` | 镜像目标平台；All-in-One 和游戏 Runtime 使用 amd64 |
| `--push` | 将镜像推送到 `--tag` 指定仓库；默认只加载到本机 Docker |

私有前端需要 Git 读取权限。HTTPS 可使用已配置的 Git credential helper；SSH 示例：

```bash
node deploy/scripts/build-native-release.mjs --version preview-local \
  --frontend-repository git@github.com:lcy0828/dst-admin-vue.git
```

原生管理服务依赖 CGO/SQLite，应在目标系统和架构构建；不要直接把 macOS 产物复制到 Linux。发布包包含 `dst-admin`、`dst-admin-agent`、`dst-map-renderer`、`mod-local-setup` 和 `deploy/`。

`DST_ADMIN_WEB_ROOT` 仅作为显式外部页面覆盖入口保留。使用内嵌发布包时清除旧覆盖变量，以免继续加载旧页面。裸 `go build ./cmd/admin-api` 供开发使用，不会自动下载或嵌入前端。

## GitHub Actions

后端 CI 运行 Go 测试、race、vet、漏洞扫描及编译。Package 工作流在当前发布分支推送、`v*` 标签或手动运行时构建完整产物。它先解析前端提交，再让所有任务使用同一 SHA；原生包启动检查通过后上传附件，镜像启动检查通过后推送。

前端私有仓库通过后端 Actions secret `FRONTEND_READ_KEY` 中的只读 deploy key 检出；公有仓库不需要该密钥。密钥只用于获取源码，不进入构建上下文。GHCR 推送使用当前仓库 `GITHUB_TOKEN` 的 `packages:write` 权限。更换前端仓库时同时调整工作流地址和授权。

启用 Docker Hub 同步：

1. 在 Docker Hub 创建 `lcy0828/dst-admin-go` 仓库，并创建具有 Read & Write 权限的 Access Token。
2. 在 GitHub 仓库 **Settings → Secrets and variables → Actions** 添加 `DOCKERHUB_TOKEN`。登录用户名默认 `lcy0828`；使用其他登录账号时添加 `DOCKERHUB_USERNAME` secret。目标命名空间由工作流中的 `DOCKERHUB_NAMESPACE` 指定。
3. 推送正式版本标签，例如 `git tag v1.0.0 && git push origin v1.0.0`（`origin` 指向 GitHub 仓库）。

启用阿里云 ACR 同步：

1. 在杭州地域创建命名空间 `dstadmin` 和仓库 `dst-admin-go`。在 ACR **访问凭证** 中设置固定登录密码；这里使用的是镜像仓库登录密码。
2. 在 GitHub Actions **Secrets** 添加 `ALIYUN_USERNAME`（ACR 登录用户名）和 `ALIYUN_PASSWORD`（固定登录密码）；在 **Variables** 添加 `ALIYUN_NAMESPACE=dstadmin`。
3. 后续 Package 发布会同步到 `registry.cn-hangzhou.aliyuncs.com/dstadmin/dst-admin-go`，服务标签与 Docker Hub 一致，如 `latest`、`agent-latest`、`agent-v1.0.0`。

补传已有正式版时，在 Actions 运行 **Sync Alibaba Cloud images**，填写 `version=v1.0.0`。它直接复制 GHCR 中已发布的四种镜像，保留构建内容；正式版本号同时同步 GHCR 当前的 `latest`，候选版只同步对应版本。只填写 `latest` 时仅同步最新正式标签。拉取私有 ACR 仓库前，在部署机器运行 `docker login registry.cn-hangzhou.aliyuncs.com`。

未配置某个镜像站的凭据时会跳过该镜像站，并在 Actions 摘要中注明；凭据失效或推送失败会使任务失败。手动运行关闭 `publish_images` 时，所有镜像仓库和 GitHub Release 均不发布。All-in-One、控制端和 Agent 的启动检查保持 180 秒，Runtime 检查启动包装器。

前端提交不会直接修改已安装服务，也不会自动发布后端。需要新页面时运行主仓库 Package 工作流；也可用 `frontend_ref` 输入指定回滚版本。手动构建可选择是否发布镜像，默认发布；标签发布附件只有对应标签流水线成功后可下载。

## 升级与回滚

1. 在页面正常保存并停止受影响房间，确认进程退出。停止 native 管理服务或 Agent 本身不等于停止游戏。
2. 备份当前生效配置、数据库、Agent 身份/操作状态和所有目标节点的存档。SQLite 停写后复制完整状态，或使用一致性 `.backup`；不要仅复制正在写入的主数据库文件。
3. 保留旧镜像 digest 或原生包，验证新包 SHA-256。预览版本升级先在独立目录检查启动和页面。
4. Docker 保留原 `.env` 和数据挂载，替换镜像后 `up -d`。原生安装脚本使用当前生效配置的独立副本，不能重新套初始模板；Linux 安装后显式重启，macOS 安装脚本会重启 LaunchAgent。
5. 检查页面、登录、房间列表、Agent 在线状态，再按需启动原房间并检查真实游戏日志。

若使用自定义服务托管，也可将原生包放在 `ROOT/releases/版本目录`，用 `deploy/scripts/activate-native-release.sh --root ROOT --release 版本目录` 原子切换 `current`，用 `--rollback` 切回 `previous`。服务应事先配置为运行 `ROOT/current/dst-admin`；该工具只切换链接，不改配置、存档或重启进程。普通安装脚本复制到固定路径，不会自动使用此链接。

回滚二进制前检查数据库兼容性。需要恢复旧数据库时使用匹配的备份，并单独核对游戏存档的恢复时间点；只切换程序不会恢复数据。不要删除存档、Agent 状态或锁文件来解决版本冲突。

## Nginx 同源反向代理

Web、`/api` 与 `/agent` 必须路由到同一个管理服务。配置有效 TLS 证书，并在 Nginx `http` 块定义：

```nginx
map $http_upgrade $dst_connection_upgrade {
    default upgrade;
    '' close;
}
```

在对应 HTTPS `server` 中：

```nginx
location / {
    proxy_pass http://127.0.0.1:8000;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection $dst_connection_upgrade;
    proxy_read_timeout 3600s;
    proxy_buffering off;
}
```

All-in-One 默认上游端口为 `8080`。按实际存档上传限制设置 `client_max_body_size`，不要向公网同时暴露未加密的管理入口。

## 密钥轮换

在 Agent 安全设置生成新密钥前，准备所有连接节点的更新计划。保存新密钥到各节点受保护配置，重启对应连接并确认恢复在线。丢失旧密钥或轮换后未更新节点会导致 Agent 离线；密钥不能写进 URL、普通日志或公开 issue。
