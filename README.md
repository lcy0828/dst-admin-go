# DST Admin Go

DST Admin Go 是《饥荒联机版》专用的服务器管理 API。当前 release 只发布同源、会话认证的 `/api/v2`，配套前端位于 `dst-admin-vue`。

## 当前能力

- 首次管理员初始化、bcrypt 密码、HttpOnly 会话、CSRF、登录限流和全会话密码轮换。
- 房间发现、接管、创建，以及 Master/Caves 分片启停和持久化 Job。
- 实时日志与 Job SSE、参数化/原始控制台、结构化日志和世界状态。
- 配置 Diff、revision、保护备份、原子写入和未知 Lua 字段保留。
- 备份上传、下载、恢复、自动快照、地图与 Session 诊断。
- Steam Mod 搜索、依赖安装、更新健康度，以及 gopher-lua 主解析和外部 Lua fallback。
- 游戏更新、DST Docker 容器、自动化任务和多节点 Agent。

完整功能状态与证据见前端仓库的 `docs/DST_ADMIN_FUNCTION_TRUTH.md`。

## 安全边界

- 除首次初始化、登录和会话探针外，所有 v2 接口都要求管理员会话。
- 认证写请求同时要求 `X-CSRF-Token` 和 `Idempotency-Key`。
- Agent 只允许 `system.refresh` 和 `disk.inspect` 两个领域动作；任意 shell、script 和 custom 命令不属于生产 API。
- Agent 密钥常规读取只返回掩码与 SHA-256 指纹，轮换结果只显示一次。
- 新二进制不注册旧 `/api/*`、旧静态日志页、tmux raw-command 或旧 cron raw-command。需要回滚时切换到 `previous` release，不能在当前进程重新开启旧路由。
- 测试 memory adapter 只有在 `DST_ADMIN_ENV=test` 且显式设置对应 `DST_ADMIN_TEST_*=memory` 时才能启用。

## 本地验证

项目固定 Go 1.25.12 工具链，并使用 SQLite，因此构建机需要可用的 CGO 编译环境。

```bash
GOTOOLCHAIN=go1.25.12+auto go test -race ./...
GOTOOLCHAIN=go1.25.12+auto go vet ./...
GOTOOLCHAIN=go1.25.12+auto go build ./...
```

启动独立 API：

```bash
GOTOOLCHAIN=go1.25.12+auto go run ./cmd/admin-api -addr 127.0.0.1:18000
```

实际路径和密钥通过 `conf/app.conf` 或 `DST_ADMIN_*` 环境变量配置。不要用测试 adapter 运行生产服务。

## 发布构建

发布产物应注入可查询的版本信息：

```bash
release_version="$(git describe --tags --always --dirty)"
release_commit="$(git rev-parse HEAD)"
release_time="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
CGO_ENABLED=1 go build -trimpath \
  -ldflags "-X dont/internal/buildinfo.Version=${release_version} -X dont/internal/buildinfo.Commit=${release_commit} -X dont/internal/buildinfo.BuildTime=${release_time}" \
  -o dist/dst-admin .
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-X main.version=${release_version}" \
  -o dist/dst-map-renderer ./cmd/dst-map-renderer
```

两个二进制应发布到同一目录。API 会先检查显式配置，再检查自身所在目录，最后检查 `PATH`，并通过 Renderer v1 能力握手确认版本兼容；只有文件存在但握手失败不会启用地图功能。

登录后可从 `GET /api/v2/system/status` 的 `application` 字段核对版本、提交和构建时间。

## 关键文档

- API 契约：`docs/openapi-v2.yaml`
- 生产部署与回滚：`docs/deployment-and-rollback.md`
- Mod 兼容策略：`docs/mod-management.md`
- 地图渲染器：`docs/map-renderer.md`

## Agent

Agent WebSocket 由主程序可选启动，生产环境必须通过同源 TLS 代理，并使用 `Authorization: Bearer <key>`。URL query key 仅为旧 Agent 的临时兼容路径，可能泄漏到访问日志，不应用于新部署。

Agent 和独立 Agent Server 的构建入口仍位于 `agent/cmd/agent` 与 `server/cmd/server`；它们的底层协议代码不等于公开管理 API，生产动作范围始终以 `/api/v2/agents/actions` 返回的白名单为准。
