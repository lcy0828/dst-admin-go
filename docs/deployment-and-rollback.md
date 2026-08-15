# DST Admin 生产部署、迁移与回滚

## 1. 适用范围

本文适用于 Vue 3 静态前端和 Go 管理 API 的同源生产部署。示例目录使用 `/opt/dst-admin`，服务用户使用 `dstadmin`；实际路径必须与系统设置和 DST 专用用户一致。控制面默认管理本机；远程 Agent 是显式启用的可选运行目标，部署方式见 `docs/container-and-native-deployment.md`。

生产切换必须满足：后端全量测试、竞态测试和 `go vet`，前端 lint/unit/build，真实后端核心流程人工验收、数据库备份校验、配置备份和上一版本产物均已完成。当前仓库没有浏览器 E2E 或 OpenAPI 代码生成脚本，不能把不存在的命令伪装成发布门禁。不要在没有可恢复数据库副本时直接启动新版本迁移。

## 2. 发布目录

```text
/opt/dst-admin/
  releases/
    20260808-120000/
      dst-admin
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
go version # 必须为 go1.25.12 或更新的兼容补丁版本
go test -race ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
release_version="$(git describe --tags --always --dirty)"
release_commit="$(git rev-parse HEAD)"
release_time="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
CGO_ENABLED=1 go build -trimpath \
  -ldflags "-X dont/internal/buildinfo.Version=${release_version} -X dont/internal/buildinfo.Commit=${release_commit} -X dont/internal/buildinfo.BuildTime=${release_time}" \
  -o dist/dst-admin ./cmd/admin-api

cd ../dst-admin-vue-v3
npm ci
npm audit --registry=https://registry.npmjs.org --audit-level=moderate
npm run lint -- --no-fix
npm test
npm run build
```

把 `dist/dst-admin` 和前端 `dist/` 放入新的 release 目录，校验 SHA-256 后再切换。部署探针还要确认 `/api/v2/system/status` 返回的 `application.version` 和 `application.commit` 与本次 release 一致。不要把 `.env`、数据库、`app.conf`、Agent 密钥或 Steam API Key 打进前端产物。

前端 API 方法和分布式类型目前由手写 client/声明维护。后端 `docs/openapi-v2.yaml`、前端 `src/api/v2.js` 与 `src/api/distributedManagement.d.ts` 必须在评审和测试中保持一致，并按前端 `docs/DST_ADMIN_FUNCTION_TRUTH.md` 的发布清单完成真实后端验收。

## 5. 服务启动

最小 systemd 单元示例：

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
Environment=DST_ADMIN_SAVE_PATH=/srv/dst/.klei/DoNotStarveTogether
Environment=DST_ADMIN_BACKUP_PATH=/srv/dst/backups
Environment=DST_ADMIN_SERVER_PATH=/srv/dst/server
Restart=on-failure
RestartSec=5
UMask=0077
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

首次启动会执行向后兼容的数据库表迁移。启动失败时不要反复重启覆盖现场；保留日志并执行第 10 节回滚。

## 6. Nginx 同源反向代理

```nginx
server {
    listen 443 ssl http2;
    server_name dst.example.com;
    root /opt/dst-admin/current/public;

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

新 release 只注册 `/api/v2`。旧 `/api/*`、`/gamelog`、`/static`、tmux raw-command 和旧 cron raw-command 必须返回 `404`；上线前把这些负向探针纳入检查。兼容旧前端只能通过保留的 `previous` release，禁止在新二进制增加环境开关重新暴露旧路由。

## 7. 切换顺序

1. 完成第 3 节备份并记录旧版本健康状态。
2. 安装新 release，校验二进制和静态资源摘要。
3. 令 `previous` 指向当前 release，再原子切换 `current`。
4. 启动 `dst-admin`，确认没有迁移和配置错误。
5. 执行未登录会话探针：`curl -fsS https://dst.example.com/api/v2/auth/session`。
6. 登录后检查 `/api/v2/system/capabilities`、`/api/v2/system/status`、房间列表和最近 Job；数据库状态必须为可用、`WAL`、外键已启用且迁移版本与本次发布一致。
7. 验证一个无副作用刷新 Job，并观察 `/api/v2/jobs/events` 的 `queued -> running -> terminal`。
8. 断开 SSE 后携带 `Last-Event-ID` 重连，确认事件可回放且没有重复业务动作。
9. 验证前端 `index.html` 不缓存、带 Hash 的 assets 长缓存，最后开放流量。

## 8. 健康与运行检查

- 进程健康：systemd 状态为 active，8000 仅对本机代理监听。
- API 健康：`GET /api/v2/auth/session` 返回 JSON；登录后能力与系统状态接口成功。
- 数据健康：数据库 `PRAGMA quick_check` 返回 `ok`，最新 Job 可持久化并在刷新后读取。
- 实时健康：Job SSE 和世界日志 SSE 保持连接；Nginx 日志中没有周期性 499/504。
- Mod 健康：能力页同时显示内嵌解析器与外部 Lua fallback；至少各验证一个主路径和强制 fallback 样本。

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
