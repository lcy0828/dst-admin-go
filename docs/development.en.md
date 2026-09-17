# Start a development environment from source

[简体中文（默认）](development.md) | **English**

For production, start with the [README](../README.en.md) or [installation guide](startup-guide.en.md).
Development runs the Go API and Vite separately. The paths below keep configuration outside the repositories and do not use the actual `conf/app.conf`.

## Tools and repositories

- Use the Go toolchain specified in `go.mod` (currently `go1.25.13`). The management service needs CGO and a C compiler.
- Use Node.js 24 LTS and npm; run `npm ci` with the repository's `package-lock.json`.
- The local Runtime needs tmux and separate writable data directories. Linux game/mod downloads need SteamCMD. See [Linux prerequisites](startup-guide.en.md#linux-native-deployment).
- Place the repositories side by side: backend `feature/v2-rebuild`, frontend `master`.

```text
workspace/
  dst-admin-go/
  dst-admin-vue-v3/
  dst-admin-dev/       Development configuration and data; not tracked by Git
```

If a production service already uses the ports, choose other ports and independent data directories. Do not run two management processes against the same saves.

## 1. Prepare isolated configuration

From the backend repository root:

```bash
mkdir -p ../dst-admin-dev/control ../dst-admin-dev/server ../dst-admin-dev/saves \
  ../dst-admin-dev/backups ../dst-admin-dev/maps \
  ../dst-admin-dev/workshop/steamapps/workshop/content/322330
cp deploy/systemd/local.conf.example ../dst-admin-dev/app.conf
chmod 600 ../dst-admin-dev/app.conf
```

Edit the copied configuration and set these values to **absolute paths** inside `workspace/dst-admin-dev`:

| Setting | Location inside the development directory |
| --- | --- |
| `[database] PATH` | `control/go-dont.db` |
| `[fleet] STATE_PATH` | `control/fleet` |
| `[paths] DST_SERVER_PATH`, `DST_SAVE_PATH` | `server`, `saves` |
| `[paths] DST_BACKUP_PATH`, `DST_MAP_PATH` | `backups`, `maps` |
| `[paths] DST_UGC_PATH` | `workshop/steamapps/workshop` |
| `[mod] WORKSHOP_MOD_PATH`, `WORKSHOP_CONTENT` | `workshop`, `workshop/steamapps/workshop/content/322330` |

Set SteamCMD, Lua, and Python paths to the actual executables. Keep `SETUP = incomplete` and the standalone role for a fresh development environment. Do not copy production databases, Cluster Tokens, or Agent keys.
For real game tests, use an isolated game copy or explicitly connect an existing installation, ensuring no other manager controls it at the same time.

## 2. Start the backend

From the backend repository root:

```bash
mkdir -p dist
CGO_ENABLED=0 go build -o dist/dst-map-renderer ./cmd/dst-map-renderer
DST_ADMIN_CONFIG="$PWD/../dst-admin-dev/app.conf" \
DST_ADMIN_MAP_RENDERER_PATH="$PWD/dist/dst-map-renderer" \
CGO_ENABLED=1 go run ./cmd/admin-api -addr 127.0.0.1:8000
```

Check it from another terminal:

```bash
curl -fsS http://127.0.0.1:8000/api/v2/auth/session
```

JSON from the unauthenticated session endpoint confirms API startup. The backend root may not serve a page because Vite serves development pages.
The `-addr` flag selects the listener; without it this entry point defaults to `127.0.0.1:18000`, not the development port.

## 3. Start the frontend

Open another terminal and enter the frontend repository:

```bash
cd ../dst-admin-vue-v3
npm ci
npm run dev -- --host 127.0.0.1 --port 5173 --strictPort
```

Open `http://127.0.0.1:5173`. `/api` defaults to a proxy at `http://127.0.0.1:8000`.
If the backend uses another port, such as `18080`, update the frontend proxy too:

```bash
VITE_API_PROXY_TARGET=http://127.0.0.1:18080 \
  npm run dev -- --host 127.0.0.1 --port 15173 --strictPort
```

`VITE_API_BASE_URL` defaults to `/api` and usually needs no change. Do not store passwords or keys in `VITE_*` variables: their values enter browser code. `npm run preview` only previews static output and does not replace the production management service.

## 4. Verify production serving from one origin

After building the frontend, serve its files through Go. Stop the earlier development API first, then run from the backend repository:

```bash
npm --prefix ../dst-admin-vue-v3 run build
CGO_ENABLED=1 go build -o dist/dst-admin ./cmd/admin-api
DST_ADMIN_CONFIG="$PWD/../dst-admin-dev/app.conf" \
DST_ADMIN_WEB_ROOT="$PWD/../dst-admin-vue-v3/dist" \
DST_ADMIN_MAP_RENDERER_PATH="$PWD/dist/dst-map-renderer" \
  ./dist/dst-admin -addr 127.0.0.1:8000
```

Visit `http://127.0.0.1:8000`. Verify administrator login, page refresh, game installation status, and job results.
Game processes inside independent tmux sessions may continue after the management service exits. Stop test worlds normally through room management before ending your test.

## Code checks

Run backend checks from its repository and select packages appropriate to your changes. Formal release requirements are in [deployment and rollback](deployment-and-rollback.en.md):

```bash
DST_ADMIN_CONFIG="$PWD/deploy/systemd/local.conf.example" go test ./...
go vet ./...
```

Frontend checks:

```bash
npm test
npm run lint
npm run build
```

Build the Agent from backend `./cmd/agent`. Use separate configuration and `-state` paths; do not reuse production Agent identity files. Development startup does not automatically add remote nodes. For integration tests, follow the [remote Agent guide](startup-guide.en.md#connect-a-remote-agent).

## Package the complete product

Image and native builds fetch the official frontend and embed it in the management binary; see [packaging](deployment-and-rollback.en.md). Plain `go run` builds have no embedded UI; use Vite or an explicit `DST_ADMIN_WEB_ROOT` during development. Private frontend source requires Git access. `--frontend PATH` uses only its committed HEAD.
