# Deployment and rollback

[简体中文（默认）](deployment-and-rollback.md) | **English**

Start from the [main README](../README.en.md). Images and native packages embed the UI. Update the management program from the panel or replace the full release artifact while preserving configuration, databases, Agent identities and saves.

## Downloads and versions

- The [Package workflow](https://github.com/lcy0828/dst-admin-go/actions/workflows/package.yml) builds Linux amd64 and macOS arm64 native packages. Version tags or explicit manual publication push tested images to GHCR. With mirror credentials configured, it publishes the same images to Docker Hub and Alibaba Cloud without rebuilding.
- In mainland China, stable deployments use `registry.cn-hangzhou.aliyuncs.com/dstadmin/dst-admin-go:latest`; elsewhere use Docker Hub's `lcy0828/dst-admin-go:latest`. Replace `latest` with `vX.Y.Z` to pin a release. Docker Hub prefixes Controller, Agent, and Runtime tags with `controller-`, `agent-`, and `runtime-`, for example `agent-latest` or `agent-v1.0.0`.
- Explicit manual image publication uses `ghcr.io/lcy0828/dst-admin-go/all-in-one:preview`, `control-plane:preview`, `agent:preview`, and `dst-runtime:preview`. Builds also receive `sha-FULL_BACKEND_COMMIT` tags. Rebuilding the same backend with a different frontend can change these tags; pin the image digest and frontend SHA for exact provenance.
- `vX.Y.Z` tags publish stable versions: versioned images are pushed first, then all four `latest` aliases are updated after every native package and image check passes. A GitHub Release contains full native packages, standalone Agent packages and SHA-256 files. Agent packages have stable filenames such as `releases/latest/download/dst-admin-agent-linux-amd64.tar.gz`. Candidate tags such as `vX.Y.Z-rc.N` publish versioned images and a prerelease without updating `latest`.
- `dst-admin -version` reports backend version/commit, frontend commit, and `embeddedWebUI`. Native `manifest.json` also records tools and lockfile hashes. Images carry `io.dst-admin.frontend.commit`.

## Build from one repository

Install Git and Node.js 22+ on the build host. Native builds also need Go (see `go.mod`) and a C compiler; image builds need Docker Buildx and compile Go/Node inside their build stages.

```bash
node deploy/scripts/build-native-release.mjs --version preview-local --output ./dist
node deploy/scripts/build-image.mjs --kind all-in-one --tag dst-admin/all-in-one:preview
node deploy/scripts/build-image.mjs --kind control-plane --tag dst-admin/control-plane:preview
node deploy/scripts/build-image.mjs --kind agent --tag dst-admin/agent:preview
node deploy/scripts/build-image.mjs --kind dst-runtime --tag dst-admin/dst-runtime:preview
```

The default source is `master` in the [GitHub frontend repository](https://github.com/lcy0828/dst-admin-vue). Its revision is fixed once per build. Fetch, `npm ci`, or frontend build failures abort packaging; an old working-directory `dist/` is never substituted. Native management binaries embed the UI using the `webui` build tag. Generated files are not committed.

| Option | Purpose |
| --- | --- |
| `--frontend-ref SHA` | Pin a compatible frontend revision |
| `--frontend PATH` | Use a checkout's committed HEAD; ignore uncommitted changes |
| `--frontend-repository URL` | Select an authenticated source URL, including SSH |
| `--version VERSION` | Set release metadata |
| `--platform linux/amd64` | Image target; All-in-One and game Runtime require amd64 |
| `--push` | Push to `--tag`; default loads into local Docker |

Private frontend source needs Git read access. Use an HTTPS credential helper or SSH:

```bash
node deploy/scripts/build-native-release.mjs --version preview-local \
  --frontend-repository git@github.com:lcy0828/dst-admin-vue.git
```

Native management uses CGO/SQLite: build on the target OS/architecture, or supply compatible prebuilt binaries. Do not copy macOS binaries to Linux. Packages include `dst-admin`, `dst-admin-agent`, `dst-map-renderer`, `mod-local-setup`, and `deploy/`.

`DST_ADMIN_WEB_ROOT` remains an explicit external UI override. Remove stale overrides when using embedded releases. Plain `go build ./cmd/admin-api` is for development and does not fetch or embed frontend files.

## GitHub Actions

A local `git commit` does not trigger GitHub Actions; pushing does. Backend CI runs tests, race detection, vet, vulnerability scanning, and compilation. Frontend CI runs checks, tests, and builds. Package requires Backend CI and pins one frontend SHA for all build jobs.

| Action | Checks and builds | Publication |
| --- | --- | --- |
| Push a branch or open/update a PR | Repository CI | None |
| Push backend `master` | CI, full packaging and startup checks; native packages retained as Actions artifacts | No images, Release, `latest`, or `preview` updates |
| Push `vX.Y.Z` | Checks, packaging, publication | Stable Release and versioned images; `latest` advances after all checks pass |
| Push `vX.Y.Z-rc.N` | Checks, packaging, publication | Prerelease and versioned images; no `latest` update |
| Run Package manually | Checks and packaging by default | Only an explicit `publish_images` selection publishes preview images; no Release |

For private frontend source, store a read-only deploy key in backend secret `FRONTEND_READ_KEY`; public source needs no key. The key is used only for checkout and never enters build contexts. GHCR uses this repository's `GITHUB_TOKEN` with `packages:write`. Update both repository settings and authorization when changing the frontend source.

To enable Docker Hub synchronization:

1. Create the `lcy0828/dst-admin-go` repository on Docker Hub and an access token with Read & Write permissions.
2. Add `DOCKERHUB_TOKEN` under the GitHub repository's **Settings → Secrets and variables → Actions**. The login username defaults to `lcy0828`; set the optional `DOCKERHUB_USERNAME` secret for a different login account. The workflow's `DOCKERHUB_NAMESPACE` sets the destination namespace.
3. Push a stable version tag, for example `git tag v1.0.0 && git push origin v1.0.0`, where `origin` points to the GitHub repository.

To enable Alibaba Cloud ACR synchronization:

1. In the Hangzhou region, create namespace `dstadmin` and repository `dst-admin-go`. Set a fixed registry login password under ACR **Access Credentials**.
2. Add GitHub Actions **Secrets** `ALIYUN_USERNAME` (ACR login username) and `ALIYUN_PASSWORD` (registry password), plus the **Variable** `ALIYUN_NAMESPACE=dstadmin`.
3. Future Package runs also publish to `registry.cn-hangzhou.aliyuncs.com/dstadmin/dst-admin-go`, using the same service tags as Docker Hub: `latest`, `agent-latest`, `agent-v1.0.0`, and so on.

To copy an existing release, run **Sync Alibaba Cloud images** in Actions with `version=v1.0.0`. It copies the four published GHCR images without rebuilding. Stable version inputs also copy GHCR's current `latest`; prereleases copy only their version. Use `latest` to copy only the current stable aliases. Run `docker login registry.cn-hangzhou.aliyuncs.com` on deployment hosts before pulling from a private ACR repository.

Unconfigured mirrors are skipped with a note in the Actions summary. Invalid credentials or failed pushes fail the job. Manual runs default to `publish_images: false` and publish to no registry. Manual runs never create a GitHub Release. All-in-One, Controller, and Agent startup checks still run for 180 seconds; Runtime validates its startup wrapper.

Frontend commits do not update installed services or automatically publish the backend. Branch builds do not publish images. Publish a new `vX.Y.Z` tag to deliver fixes through `latest`; do not rewrite released tags. Run Package to include new pages, or set the manual `frontend_ref` input to pin a revision. Manual image publication is disabled by default; explicitly enabling it publishes preview images only. Tagged release assets exist only after the tag pipeline succeeds.

Before tagging a release, run Package manually with `verification_version` (for example `v1.2.6`) and leave `publish_images` disabled. This verifies native packages, online updates and images without creating a Release, publishing images or changing `latest`. Push the stable tag after all checks pass.

## Online updates

Official Linux amd64 and macOS arm64 releases with online update support display the management version in the header and flag newer releases. Under **System settings → Software updates**, check the latest stable GitHub release and confirm installation. The panel downloads and verifies SHA-256, platform and embedded UI, switches the program, and reconnects after restart. Download progress and speed are shown. Automatic source selection tries GitHub first and falls back to GHFast on connection failure; either source can be selected manually. Development builds and releases without an update bundle display version information and a release link.

Only the management program, embedded frontend and bundled helpers change. Running game rooms continue; wait for backups, game downloads and other background work to finish before updating. The panel offers release checks, forward updates and retries. A new program must start within 60 seconds and remain healthy for 10 seconds before activation. Failed startup automatically restores the committed program and preserves that choice across restarts. Settings, databases and saves are preserved; releases that require incompatible database or base dependency changes need the full upgrade procedure.

Programs are persisted under `software-updates/` next to the configuration file: `/opt/dst/control/software-updates` for All-in-One and `/var/lib/dst-admin/software-updates` for the control plane. Preserve the data mounts to retain updates across container restarts and recreation. A newly installed newer base image is verified once; if it fails, the last verified program is restored. Restarting that same image preserves recovery instead of repeatedly selecting the failed version. `DST_ADMIN_UPDATE_DIR` selects a separate writable directory; the runtime user needs write and execute permission, and the mount must allow execution.

Compose allows a writable container root filesystem by default. Set `DST_ADMIN_READ_ONLY=true` or change `read_only` to `true` for a read-only deployment. Online updates still work with writable data mounts and temporary directories. No `.env` is required. Online updates do not modify the original image; image tags identify the base version, while the panel shows the active program version.

Install a version with the update launcher before using this feature; an older release cannot add the launcher from its existing UI. Routine updates do not require pulling an image. System libraries, SteamCMD and deployment script changes still require an image update. Standalone Agents are upgraded separately under Machines. Clear `DST_ADMIN_WEB_ROOT` when using the embedded frontend to avoid loading an external older UI.

Stable releases contain `dst-admin-update-linux-amd64.tar.gz` or `dst-admin-update-darwin-arm64.tar.gz` and its `.sha256` file, plus `dst-admin-release.json` for release checks through the China download proxy. Bundles contain only `dst-admin`, `dst-map-renderer`, `mod-local-setup` and a verification manifest. CI checks the bundle, embedded frontend and update/recovery lifecycle. Protocol changes require a new full base release; incompatible data migrations must not be shipped as routine updates with automatic program recovery.

### Standalone Agent online updates

Open **Machines → Select a machine → Diagnostics → Agent software updates** to view that node's current release, check for updates, install, or retry. Official Linux amd64/arm64, macOS amd64/arm64 and Windows amd64 Agents, including Docker Agents, use persisted version directories and a stable launcher. Routine updates require neither a new image nor a manual system-service restart. Windows support here refers to updating the Agent program; its game-running capabilities do not change.

Agents check and download official bundles directly by default, with GitHub and the China proxy available. Choose **Controller relay** when a node cannot reach download sources: the Controller streams one trusted official archive, and the Agent verifies and installs it locally. Updating the Controller does not automatically update all Agents. Background release downloads are disabled; checks and updates run only on demand.

Releases provide `dst-admin-agent-update-<platform>.tar.gz` and its `.sha256`, containing the Agent and packaged helpers. The new Agent must start and reconnect over its authenticated control channel within 60 seconds, then remain healthy for 10 seconds before activation. Startup or reconnection failure automatically restores the previous program. Wait for other Agent commands to finish before switching. Running game processes, Agent identity, settings, operation state and saves are preserved. Short connection failures keep observing the update; after a timeout, Reconnect resumes the same job without reinstalling. Management roles and directories cannot be reapplied while an update bundle is downloading; wait for it to finish.

Updates live beside the Agent configuration in `software-updates/`, or `/var/lib/dst-admin-agent/software-updates` in Docker. Override this with `DST_ADMIN_AGENT_UPDATE_DIR` if needed. Preserve its writable, executable data mount. An older Agent requires one package or image upgrade to obtain the launcher before page updates become available. `-version` retains the Agent protocol version used for compatibility; `-build-info` reports the official release build shown in the panel.

## Full upgrade and rollback

1. Save and stop affected rooms through the panel, then verify process exit. Stopping a native management service or Agent alone does not stop game worlds.
2. Back up active configuration, databases, Agent identities/operation state, and saves on every target. Copy SQLite state after stopping writes or use consistent `.backup`; do not copy only a live main database file.
3. Retain the old image digest or package. Verify the new SHA-256 and smoke-test previews in an isolated directory.
4. Docker: preserve the ports and data mounts in the Compose file, replace the image, then `up -d`. Native installers: pass an independent copy of active configuration, never a fresh template. Explicitly restart Linux services; macOS installers restart LaunchAgents.
5. Check pages, login, room inventory, and Agents. Start the rooms you need and inspect actual game logs.

For custom service layouts, extract packages under `ROOT/releases/VERSION` and run `deploy/scripts/activate-native-release.sh --root ROOT --release VERSION` to switch `current`; `--rollback` restores `previous`. Configure the service to run `ROOT/current/dst-admin` beforehand. This helper only switches links, never configuration, saves, or processes. Standard installers copy to fixed paths and do not use this link automatically.

Check database compatibility before binary rollback. Restore a matching database backup when required and separately choose the save recovery point. Switching programs does not restore data. Never delete saves, Agent state, or locks to resolve version conflicts.

## Nginx same-origin proxy

Route pages, `/api`, and `/agent` to the same management service. Configure TLS and define this in the Nginx `http` block:

```nginx
map $http_upgrade $dst_connection_upgrade {
    default upgrade;
    '' close;
}
```

In the HTTPS `server` block:

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
    proxy_read_timeout 7200s;
    proxy_send_timeout 7200s;
    proxy_request_buffering off;
    proxy_buffering off;
}
```

Set `DST_ADMIN_TRUSTED_PROXIES` on the management service to the actual proxy IP addresses or CIDRs, then restart it. For a native deployment this might be `127.0.0.1,::1`; in Docker use the addresses visible to the container. Do not blindly trust loopback addresses or all sources. When unset, forwarded IP headers are ignored and IP allowlists and login limits use the directly connected address.

All-in-One defaults to upstream port `8080`. Set `client_max_body_size` for save uploads. Do not expose an additional unencrypted public management endpoint.

## Key rotation

Prepare updates for every connected node before generating a new key in Agent security settings. Store the new key in protected node configurations, restart connections, and verify they return online. Losing or rotating a key without updating nodes disconnects Agents. Never place keys in URLs, ordinary logs, or public issues.
