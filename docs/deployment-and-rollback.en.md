# DST Admin production deployment, migration, and rollback

[简体中文（默认）](deployment-and-rollback.md) | **English**

## 1. Scope

This guide covers production deployment of the Vue 3 static frontend and Go management API at the same origin.
Examples use `/opt/dst-admin` and service user `dstadmin`. Actual paths must match system settings and the dedicated DST user.
The control plane manages the local machine by default. Remote Agents are optional Runtime targets enabled explicitly; see [container and native deployment](container-and-native-deployment.en.md).

For first installation, read the [installation and startup guide](startup-guide.en.md).
This document uses a custom release directory and `dst-admin.service`, which differ from the initial installer layout at `/var/lib/dst-admin` with `dst-admin-local.service`.
Keep the layout of an existing deployment; do not mix paths or create a second management service.

Production cutover requires full backend tests, race tests, `go vet`, frontend lint/unit/build, manual acceptance of core flows against a real backend, verified database backups, configuration backups, and previous-version artifacts.
The repository currently has no browser E2E or OpenAPI code generation script; do not invent those commands as release gates.
Do not start a new version's migrations without a recoverable database copy.

## 2. Release directories

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

- Keep binaries and frontend artifacts read-only by version; keep runtime data in `shared/`.
- Switch releases with atomic symlink replacement on the same filesystem. Do not rebuild ad hoc on production hosts.
- Set `app.conf` and database permissions to `0600`, with service `UMask=0077`.
- `VERSION` must record at least backend Git SHA, frontend Git SHA, build time, and minimum compatible version.

## 3. Back up before release

Stop writes before backing up SQLite. If downtime is impossible, use SQLite `.backup`; do not copy only an actively written main database file.

```bash
sudo systemctl stop dst-admin
sudo install -d -o dstadmin -g dstadmin -m 0700 /opt/dst-admin/shared/backups
sudo -u dstadmin sqlite3 /opt/dst-admin/shared/go-dont.db ".backup '/opt/dst-admin/shared/backups/go-dont.db.pre-20260808-120000'"
sudo -u dstadmin sqlite3 /opt/dst-admin/shared/backups/go-dont.db.pre-20260808-120000 "PRAGMA integrity_check;"
sudo -u dstadmin cp -p /opt/dst-admin/shared/app.conf /opt/dst-admin/shared/backups/app.conf.pre-20260808-120000
sudo chmod 0600 /opt/dst-admin/shared/backups/go-dont.db.pre-20260808-120000 /opt/dst-admin/shared/backups/app.conf.pre-20260808-120000
```

`PRAGMA integrity_check` must return `ok`. Also confirm:

- Save and backup directories have sufficient free disk space.
- `app.conf.bak` is readable and has mode `0600`. It is the quick rollback copy from the latest system-settings save, not a substitute for this release's backup.
- Previous binaries, static assets, and matching configuration remain available in the directory referenced by `previous`.
- Any Agent key exposed in repository files or historical logs is rotated after deployment. Removing its current value does not invalidate an old key.

## 4. Build and install

Prefer building artifacts on a separate build machine:

```bash
cd dst-admin-go
go version # Requires go1.25.13 or a newer compatible patch release
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

Place `dist/dst-admin`, `dist/dst-map-renderer`, and frontend `dist/` in the new release directory, naming the frontend directory `public/`.
Verify SHA-256 before switching. Deployment probes must also confirm that `application.version` and `application.commit` from `/api/v2/system/status` match the release.
Do not bundle `.env`, databases, `app.conf`, Agent keys, or Steam API keys in frontend output.

Frontend API methods and distributed types are currently maintained as handwritten clients/declarations.
Keep backend `docs/openapi-v2.yaml`, frontend `src/api/v2.js`, and `src/api/distributedManagement.d.ts` consistent through review and tests.
Complete real-backend acceptance using the release checklist in frontend `docs/DST_ADMIN_FUNCTION_TRUTH.md` (Chinese).

## 5. Start the service

Below is a minimal systemd unit.
First create user `dstadmin` with a working shell/tmux environment, prepare all configured directories, and put the reviewed current configuration at `/opt/dst-admin/shared/app.conf`, with its database path pointing to `go-dont.db` in the same directory.
New deployments may adapt `deploy/systemd/local.conf.example`; upgrades must retain existing configuration.
The configuration must belong to `dstadmin` with mode `0600`. The service account needs read access to release artifacts and write access to configured data directories.

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

Save it as `/etc/systemd/system/dst-admin.service`, then run:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now dst-admin
sudo systemctl status dst-admin --no-pager
curl -fsS http://127.0.0.1:8000/api/v2/auth/session
```

First startup runs backward-compatible database table migrations. If startup fails, preserve logs and follow section 10; repeated restarts can obscure the failure state.

Native Runtime tmux socket identity is bound to the normalized DST save root, independent of the release directory, Agent state file, and installation ID.
Do not change the service user or `DST_ADMIN_SAVE_PATH` during release/rollback and then directly resume existing worlds.
If paths must move, stop rooms normally, change configuration, then start them.
Controller/Agent systemd units must retain `KillMode=process` so management restarts do not terminate independent tmux/DST processes as children.

Native `STEAMCMD_PATH`/`STEAM_CMD_PATH` must point to an absolute, executable, stable entry point.
Official installers discover real SteamCMD, create missing stable symlinks, and abort if it cannot be found before switching services.
Manual releases must perform equivalent checks before game updates or mod downloads expose an invalid path.

## 6. Nginx same-origin reverse proxy

This example assumes certificates are configured. Replace the domain and certificate paths with your actual values.
The management service listens locally on `8000`.
For an All-in-One proxy on the host, set `DST_ADMIN_HTTP_BIND=127.0.0.1:8080` in `.env` and change the example backend port to `8080`.
If Nginx runs in another container, use a container network address reachable by both.
When Nginx serves static pages itself, place the same release's `public/` in the example location too.

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

Job and log SSE endpoints are under `/api/v2`; disable proxy buffering and extend read timeouts.
Agents use the separate `/agent` WebSocket path. Proxying only `/api/` leaves the UI working but prevents remote enrollment.
Run `sudo nginx -t` after saving, then reload only if validation passes. The connection URL is `wss://dst.example.com/agent`.

New releases register only `/api/v2`. Legacy `/api/*`, `/gamelog`, `/static`, tmux raw-command, and old cron raw-command endpoints must return `404`; include these negative probes before release.
Compatibility with an old frontend is provided only by retaining the `previous` release. Do not add an environment switch to re-expose legacy routes in new binaries.

## 7. Cutover sequence

1. Complete section 3 backups and record the old version's health.
2. Install the new release and verify binary/static-asset digests.
3. Point `previous` to the current release, then atomically switch `current`.
4. Start `dst-admin` and check for migration/configuration errors.
5. Run the unauthenticated session probe: `curl -fsS https://dst.example.com/api/v2/auth/session`.
6. After login, check `/api/v2/system/capabilities`, `/api/v2/system/status`, the room list, and recent jobs. The database must be available, use `WAL`, have foreign keys enabled, and report the release's migration version.
7. Verify a refresh job with no side effects, observing `queued -> running -> terminal` through `/api/v2/jobs/events`.
8. Confirm that native Runtime reacquired `.dst-admin/runtime/owner.lock`. `RUNTIME_OWNER_CONFLICT` means another local Controller/Agent is still writing the same `SAVE_PATH`; do not bypass it by deleting the lock file.
9. For every previously running native world, confirm Runtime status remains `running`, with no `LEGACY_TMUX_SOCKET_CONFLICT`, `UNMANAGED_DST_PROCESS_CONFLICT`, or `DUPLICATE_DST_PROCESS_CONFLICT`.
10. Check that each native installation's registered `STEAMCMD_PATH` is executable. Stable entry points created by installers must still resolve to existing SteamCMD files.
11. Disconnect SSE and reconnect with `Last-Event-ID`; confirm event replay without duplicate business actions.
12. Verify that frontend `index.html` is not cached and hashed assets use long caching, then open traffic.

## 8. Health and runtime checks

- Process: systemd is active; port 8000 listens only for the local proxy.
- API: `GET /api/v2/auth/session` returns JSON; capability and system-status endpoints succeed after login.
- Data: database `PRAGMA quick_check` returns `ok`; recent jobs persist and remain readable after refresh.
- Live updates: job and world-log SSE stay connected; Nginx logs have no recurring 499/504 responses.
- Mods: capabilities show both the embedded parser and external Lua fallback. Verify at least one main-path sample and one forced-fallback sample.
- Runtime ownership: each `SAVE_PATH` has one owner-lock holder. The tmux socket path stays unchanged across upgrades, and restarting the control service does not change actual DST PIDs.

## 9. Key rotation

This section applies when remote Agents are enabled. A local single-node deployment does not require an Agent.
Once remote nodes are enabled, key rotation, Agent reconnection, and capability readback are release gates.

- Normal `GET /api/v2/agents/security` reads return only a mask and SHA-256 fingerprint.
- Rotation requires typing `ROTATE AGENT KEY` in the frontend and calling `/api/v2/agents/security/actions/rotate`.
- The new key appears only once in the rotation response and corresponding UI result. Immediately update offline Agents' `0600` configuration files.
- Legacy `/api/agent/security/key/generate`, `/update`, and other legacy APIs are unregistered and return `404 Not Found`; do not use them in deployment scripts.
- After rotation, check that every Agent reconnects and capabilities for Runtime inventory, Placement, Console, backups, mods, and game updates have not regressed. Then destroy temporary records. Never put keys in command history, tickets, or URLs.

## 10. Rollback

Triggers include failed database migration, unavailable login, core room-control regressions, persistent 5xx responses, unrecoverable SSE, widespread Agent disconnection, or a serious secret exposure.

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

If migrations consist only of verified backward-compatible additions, try the old version with the current database first.
If the old version cannot read it, migration was interrupted, or the new version wrote semantics the old version cannot understand, restore the pre-release database:

```bash
sudo -u dstadmin cp -p /opt/dst-admin/shared/backups/go-dont.db.pre-20260808-120000 /opt/dst-admin/shared/go-dont.db
sudo chmod 0600 /opt/dst-admin/shared/go-dont.db
sudo systemctl start dst-admin
```

After rollback, repeat the session probe, login, room reads, and a refresh with no side effects.
If Agent keys were rotated during the new release, restoring an old key file is not sufficient: the server and every Agent must use the same valid key. Rotate again if necessary.

## 11. Release completion record

Record version SHAs, backup locations and integrity results, migration logs, health-check times, SSE reconnect results, online Agent count, both mod-parser sample results, rollback rehearsal results, and the approver.
Mark a release for long-term retention only when the evidence is complete. Retain the previous version for at least one complete release cycle.
