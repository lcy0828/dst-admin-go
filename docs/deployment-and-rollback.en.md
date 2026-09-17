# Deployment and rollback

[简体中文（默认）](deployment-and-rollback.md) | **English**

Start from the [main README](../README.en.md). Images and native packages embed the UI; upgrade the complete artifact while preserving configuration, databases, Agent identities, and saves.

## Downloads and versions

- The [Package workflow](https://github.com/lcy0828/dst-admin-go/actions/workflows/package.yml) builds Linux amd64 and macOS arm64 native packages and pushes tested images to GHCR. With mirror credentials configured, it publishes the same images to Docker Hub and Alibaba Cloud without rebuilding.
- Stable deployments use `ghcr.io/lcy0828/dst-admin-go/all-in-one:latest` or Docker Hub's `lcy0828/dst-admin-go:latest`. Replace `latest` with `vX.Y.Z` to pin a release. Docker Hub prefixes Controller, Agent, and Runtime tags with `controller-`, `agent-`, and `runtime-`, for example `agent-latest` or `agent-v1.0.0`.
- Branch images are `ghcr.io/lcy0828/dst-admin-go/all-in-one:preview`, `control-plane:preview`, `agent:preview`, and `dst-runtime:preview`. Builds also receive `sha-FULL_BACKEND_COMMIT` tags. Rebuilding the same backend with a different frontend can change these tags; pin the image digest and frontend SHA for exact provenance.
- `vX.Y.Z` tags publish stable versions: versioned images are pushed first, then all four `latest` aliases are updated after every native package and image check passes. A GitHub Release contains `.tar.gz` and SHA-256 files. Candidate tags such as `vX.Y.Z-rc.N` publish versioned images and a prerelease without updating `latest`.
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

Backend CI runs tests, race detection, vet, vulnerability scanning, and compilation. Package runs on release-branch pushes, `v*` tags, or manual dispatch. It resolves the frontend once and shares its SHA across jobs. Native packages are uploaded after startup smoke tests; images are pushed after startup checks.

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

Unconfigured mirrors are skipped with a note in the Actions summary. Invalid credentials or failed pushes fail the job. Turning off `publish_images` on a manual run disables all registries and GitHub Release publication. All-in-One, Controller, and Agent startup checks still run for 180 seconds; Runtime validates its startup wrapper.

Frontend commits do not update installed services or automatically publish the backend. Run Package to include new pages, or set the manual `frontend_ref` input to pin a revision. Manual runs may disable image publication; it is enabled by default. Tagged release assets exist only after the tag pipeline succeeds.

## Upgrade and rollback

1. Save and stop affected rooms through the panel, then verify process exit. Stopping a native management service or Agent alone does not stop game worlds.
2. Back up active configuration, databases, Agent identities/operation state, and saves on every target. Copy SQLite state after stopping writes or use consistent `.backup`; do not copy only a live main database file.
3. Retain the old image digest or package. Verify the new SHA-256 and smoke-test previews in an isolated directory.
4. Docker: preserve `.env` and mounts, replace the image, then `up -d`. Native installers: pass an independent copy of active configuration, never a fresh template. Explicitly restart Linux services; macOS installers restart LaunchAgents.
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
    proxy_read_timeout 3600s;
    proxy_buffering off;
}
```

All-in-One defaults to upstream port `8080`. Set `client_max_body_size` for save uploads. Do not expose an additional unencrypted public management endpoint.

## Key rotation

Prepare updates for every connected node before generating a new key in Agent security settings. Store the new key in protected node configurations, restart connections, and verify they return online. Losing or rotating a key without updating nodes disconnects Agents. Never place keys in URLs, ordinary logs, or public issues.
