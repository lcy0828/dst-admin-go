# DST Admin 生产部署、迁移与回滚

**简体中文（默认）** | [English](deployment-and-rollback.en.md)

## 1. 适用范围

本文适用于 Vue 3 静态前端和 Go 管理 API 的同源生产部署。示例目录使用 `/opt/dst-admin`，服务用户使用 `dstadmin`；实际路径必须与系统设置和 DST 专用用户一致。控制面默认管理本机；远程 Agent 是显式启用的可选运行目标，部署方式见 `docs/container-and-native-deployment.md`。

首次安装请先读[安装与启动指南](startup-guide.md)。本文采用自定义 release 目录和
`dst-admin.service`，与官方首次安装脚本的 `/var/lib/dst-admin`、`dst-admin-local.service`
是两种布局。现有部署应沿用自己的布局，不要直接混用路径或创建第二个管理服务。

生产切换必须满足：后端全量测试、竞态测试和 `go vet`，前端 lint/unit/build，真实后端核心流程人工验收、数据库备份校验、配置备份和上一版本产物均已完成。当前仓库没有浏览器 E2E 或 OpenAPI 代码生成脚本，不能把不存在的命令伪装成发布门禁。不要在没有可恢复数据库副本时直接启动新版本迁移。

## 2. 发布目录

```text
/opt/dst-admin/
  releases/
    20260808-120000/
      dst-admin
      dst-map-renderer
      public/
      VERSION
  shared/
    app.conf
    go-dont.db
    backups/
  current -> releases/20260808-120000
  previous -> releases/<last-version>
```

- 二进制和前端产物按版本只读保存，运行数据放在 `shared/`。
- 切换使用同一文件系统内的符号链接原子替换，不在生产机临时重新构建。
- `app.conf` 和数据库权限为 `0600`，运行服务的 `UMask` 为 `0077`。
- `VERSION` 至少记录后端 Git SHA、前端 Git SHA、构建时间和最低兼容版本。

## 3. 上线前备份

先停止写入，再备份 SQLite。若不能停机，必须使用 SQLite `.backup`，不能只复制正在写入的主数据库文件。

```bash
sudo systemctl stop dst-admin
sudo install -d -o dstadmin -g dstadmin -m 0700 /opt/dst-admin/shared/backups
sudo -u dstadmin sqlite3 /opt/dst-admin/shared/go-dont.db ".backup '/opt/dst-admin/shared/backups/go-dont.db.pre-20260808-120000'"
sudo -u dstadmin sqlite3 /opt/dst-admin/shared/backups/go-dont.db.pre-20260808-120000 "PRAGMA integrity_check;"
sudo -u dstadmin cp -p /opt/dst-admin/shared/app.conf /opt/dst-admin/shared/backups/app.conf.pre-20260808-120000
sudo chmod 0600 /opt/dst-admin/shared/backups/go-dont.db.pre-20260808-120000 /opt/dst-admin/shared/backups/app.conf.pre-20260808-120000
```

`PRAGMA integrity_check` 必须返回 `ok`。同时确认：

- 存档和备份目录磁盘空间充足。
- `app.conf.bak` 可读且权限为 `0600`；它是系统设置最近一次保存产生的快速回滚副本，不能替代本次发布备份。
- 上一版本二进制、静态资源和对应配置仍在 `previous` 指向的目录中。
- 若仓库或历史日志曾出现 Agent 密钥，上线后必须轮换，不能认为删除当前文件中的值已使旧密钥失效。

## 4. 构建与安装

建议在独立构建机生成产物：

```bash
cd dst-admin-go
go version # 必须为 go1.25.13 或更新的兼容补丁版本
go test -race ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
release_version="$(git describe --tags --always --dirty)"
release_commit="$(git rev-parse HEAD)"
release_time="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
CGO_ENABLED=1 go build -trimpath \
  -ldflags "-X dont/internal/buildinfo.Version=${release_version} -X dont/internal/buildinfo.Commit=${release_commit} -X dont/internal/buildinfo.BuildTime=${release_time}" \
  -o dist/dst-admin ./cmd/admin-api
CGO_ENABLED=0 go build -trimpath -o dist/dst-map-renderer ./cmd/dst-map-renderer

cd ../dst-admin-vue-v3
npm ci
npm audit --registry=https://registry.npmjs.org --audit-level=moderate
npm run lint -- --no-fix
npm test
npm run build
```

把 `dist/dst-admin`、`dist/dst-map-renderer` 和前端 `dist/` 放入新的 release 目录，前端目录命名为 `public/`，校验 SHA-256 后再切换。部署探针还要确认 `/api/v2/system/status` 返回的 `application.version` 和 `application.commit` 与本次 release 一致。不要把 `.env`、数据库、`app.conf`、Agent 密钥或 Steam API Key 打进前端产物。

前端 API 方法和分布式类型目前由手写 client/声明维护。后端 `docs/openapi-v2.yaml`、前端 `src/api/v2.js` 与 `src/api/distributedManagement.d.ts` 必须在评审和测试中保持一致，并按前端 `docs/DST_ADMIN_FUNCTION_TRUTH.md` 的发布清单完成真实后端验收。

## 5. 服务启动

最小 systemd 单元示例：

先创建能运行 shell/tmux 的 `dstadmin` 用户，准备所有配置目录，并将审核后的现有配置放在
`/opt/dst-admin/shared/app.conf`，数据库路径指向同目录的 `go-dont.db`。
新部署可从 `deploy/systemd/local.conf.example` 修改；升级必须保留现有配置。
文件归属 `dstadmin`、模式 `0600`，运行账号需能读取发布产物并写入配置中的数据目录。

```ini
[Unit]
Description=DST Admin
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=dstadmin
Group=dstadmin
WorkingDirectory=/opt/dst-admin/shared
ExecStart=/opt/dst-admin/current/dst-admin -addr 127.0.0.1:8000
Environment=DST_ADMIN_CONFIG=/opt/dst-admin/shared/app.conf
Environment=DST_ADMIN_WEB_ROOT=/opt/dst-admin/current/public
Environment=DST_ADMIN_MAP_RENDERER_PATH=/opt/dst-admin/current/dst-map-renderer
Environment=DST_ADMIN_SAVE_PATH=/opt/dst/saves
Environment=DST_ADMIN_BACKUP_PATH=/opt/dst/backups
Environment=DST_ADMIN_SERVER_PATH=/opt/dst/server
Restart=on-failure
RestartSec=5
KillMode=process
UMask=0077
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

保存为 `/etc/systemd/system/dst-admin.service` 后执行：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now dst-admin
sudo systemctl status dst-admin --no-pager
curl -fsS http://127.0.0.1:8000/api/v2/auth/session
```

首次启动会执行向后兼容的数据库表迁移。启动失败时不要反复重启覆盖现场；保留日志并执行第 10 节回滚。

裸机 native Runtime 的 tmux socket 身份绑定规范化后的 DST 存档根目录，不绑定 release 目录、Agent 状态文件或 Installation ID。发布和回滚不得修改运行用户或 `DST_ADMIN_SAVE_PATH` 后直接恢复原有世界；确需迁移路径时先正常停止房间，完成配置切换后再启动。Controller/Agent 的 systemd 单元必须保留 `KillMode=process`，使管理进程重启不会把独立 tmux/DST 当作子进程一并终止。

native 配置中的 `STEAMCMD_PATH`/`STEAM_CMD_PATH` 必须指向绝对、可执行的稳定入口。官方安装脚本会在切换服务前发现真实 SteamCMD、补建缺失的稳定符号链接并在无法发现时中止安装；手工发布也必须执行同等检查，不能等到游戏更新或模组下载时才暴露路径错误。

## 6. Nginx 同源反向代理

以下示例假设域名证书已配置，按实际域名、证书路径修改。管理服务仅监听本机 `8000`。
All-in-One 的本机反向代理可将 `.env` 中 `DST_ADMIN_HTTP_BIND` 设为 `127.0.0.1:8080`，
并把示例的后端端口改为 `8080`；若 Nginx 本身在另一个容器中，使用双方可达的容器网络地址。
独立 Nginx 服务静态页面时还需把同版本 `public/` 放到示例目录。

```nginx
server {
    listen 443 ssl http2;
    server_name dst.example.com;
    ssl_certificate /etc/letsencrypt/live/dst.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/dst.example.com/privkey.pem;
    root /opt/dst-admin/current/public;

    location = /agent {
        proxy_pass http://127.0.0.1:8000;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
        proxy_buffering off;
    }

    location /api/ {
        proxy_pass http://127.0.0.1:8000;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 3600s;
        add_header X-Accel-Buffering no always;
    }

    location /assets/ {
        try_files $uri =404;
        expires 1y;
        add_header Cache-Control "public, max-age=31536000, immutable";
    }

    location = /index.html {
        add_header Cache-Control "no-cache, no-store, must-revalidate";
    }

    location / {
        try_files $uri $uri/ /index.html;
        add_header Cache-Control "no-cache";
    }
}
```

Job 和日志 SSE 都位于 `/api/v2` 下，必须关闭代理缓冲并放宽读取超时。
Agent 使用独立的 `/agent` WebSocket 路径；只转发 `/api/` 会使页面可用但远端无法接入。
保存后先运行 `sudo nginx -t`，通过后再 reload Nginx。连接地址为 `wss://dst.example.com/agent`。

新 release 只注册 `/api/v2`。旧 `/api/*`、`/gamelog`、`/static`、tmux raw-command 和旧 cron raw-command 必须返回 `404`；上线前把这些负向探针纳入检查。兼容旧前端只能通过保留的 `previous` release，禁止在新二进制增加环境开关重新暴露旧路由。

## 7. 切换顺序

1. 完成第 3 节备份并记录旧版本健康状态。
2. 安装新 release，校验二进制和静态资源摘要。
3. 令 `previous` 指向当前 release，再原子切换 `current`。
4. 启动 `dst-admin`，确认没有迁移和配置错误。
5. 执行未登录会话探针：`curl -fsS https://dst.example.com/api/v2/auth/session`。
6. 登录后检查 `/api/v2/system/capabilities`、`/api/v2/system/status`、房间列表和最近 Job；数据库状态必须为可用、`WAL`、外键已启用且迁移版本与本次发布一致。
7. 验证一个无副作用刷新 Job，并观察 `/api/v2/jobs/events` 的 `queued -> running -> terminal`。
8. 确认 native Runtime 已重新取得 `.dst-admin/runtime/owner.lock`；`RUNTIME_OWNER_CONFLICT` 表示另一套本机 Controller/Agent 仍在写同一 `SAVE_PATH`，不得通过删除锁文件绕过。
9. 对每个原本运行的 native 世界确认 Runtime 状态仍为 `running`，且没有 `LEGACY_TMUX_SOCKET_CONFLICT`、`UNMANAGED_DST_PROCESS_CONFLICT` 或 `DUPLICATE_DST_PROCESS_CONFLICT`。
10. 对每个 native Installation 确认登记的 `STEAMCMD_PATH` 可执行；安装脚本创建的稳定入口必须仍指向存在的 SteamCMD。
11. 断开 SSE 后携带 `Last-Event-ID` 重连，确认事件可回放且没有重复业务动作。
12. 验证前端 `index.html` 不缓存、带 Hash 的 assets 长缓存，最后开放流量。

## 8. 健康与运行检查

- 进程健康：systemd 状态为 active，8000 仅对本机代理监听。
- API 健康：`GET /api/v2/auth/session` 返回 JSON；登录后能力与系统状态接口成功。
- 数据健康：数据库 `PRAGMA quick_check` 返回 `ok`，最新 Job 可持久化并在刷新后读取。
- 实时健康：Job SSE 和世界日志 SSE 保持连接；Nginx 日志中没有周期性 499/504。
- Mod 健康：能力页同时显示内嵌解析器与外部 Lua fallback；至少各验证一个主路径和强制 fallback 样本。
- Runtime 归属：同一 `SAVE_PATH` 只有一个 owner lock 持有者；升级前后的 tmux socket 路径一致，实际 DST PID 没有因控制服务重启而变化。

## 9. 密钥轮换

本节适用于启用了远程 Agent 的部署。本地单节点部署不要求安装 Agent；一旦启用远程节点，密钥轮换、Agent 重连和 capability 回读都属于发布门禁。

- 常规读取 `GET /api/v2/agents/security` 只返回掩码和 SHA-256 指纹。
- 轮换必须在前端输入 `ROTATE AGENT KEY`，调用 `/api/v2/agents/security/actions/rotate`。
- 新密钥只在轮换响应和对应前端结果中显示一次；立即更新离线 Agent 的 `0600` 配置。
- 旧 `/api/agent/security/key/generate`、`/update` 及其余 legacy API 不再注册并返回 `404 Not Found`，不得用于部署脚本。
- 轮换完成后检查所有 Agent 重新上线，并确认 Runtime inventory、Placement、Console、备份、Mod 和游戏更新所需 capability 没有降级，再销毁临时记录，不把密钥写进命令历史、工单或 URL。

## 10. 回滚

触发条件包括：数据库迁移失败、登录不可用、核心房间控制回归、持续 5xx、SSE 无法恢复、Agent 大面积离线或发现高危泄漏。

```bash
sudo systemctl stop dst-admin
rollback_release="$(readlink -f /opt/dst-admin/previous)"
case "$rollback_release" in
  /opt/dst-admin/releases/*) ;;
  *) echo "invalid rollback release: $rollback_release" >&2; exit 1 ;;
esac
sudo ln -sfn "$rollback_release" /opt/dst-admin/current.next
sudo mv -Tf /opt/dst-admin/current.next /opt/dst-admin/current
sudo -u dstadmin cp -p /opt/dst-admin/shared/backups/app.conf.pre-20260808-120000 /opt/dst-admin/shared/app.conf
sudo chmod 0600 /opt/dst-admin/shared/app.conf
```

数据库迁移仅包含经验证的向后兼容新增时，可先使用原数据库启动旧版。若旧版不能读取、迁移中断或新版本已写入旧版不认识的语义，则恢复发布前数据库：

```bash
sudo -u dstadmin cp -p /opt/dst-admin/shared/backups/go-dont.db.pre-20260808-120000 /opt/dst-admin/shared/go-dont.db
sudo chmod 0600 /opt/dst-admin/shared/go-dont.db
sudo systemctl start dst-admin
```

回滚后重新执行会话探针、登录、房间读取和一个无副作用刷新。若新版本期间已经轮换 Agent 密钥，不能恢复旧密钥文件后直接结束：必须让服务端和全部 Agent 使用同一有效密钥，必要时再次轮换。

## 11. 发布完成记录

每次发布记录以下证据：版本 SHA、备份路径与完整性结果、迁移日志、健康检查时间、SSE 重连结果、Agent 在线数、Mod 双路径样本结果、回滚演练结果和批准人。只有证据齐全，才把 release 标记为可长期保留；上一版本至少保留一个完整发布周期。
