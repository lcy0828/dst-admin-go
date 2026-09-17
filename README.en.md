# DST Admin

[简体中文（默认）](README.md) | **English**

A Don't Starve Together dedicated server panel for rooms, Master/Caves, mods, players, logs, backups, and remote hosts.

**This repository is the installation, download, and upgrade entry point.** All-in-One images, Controller images, and native packages include the web UI inside the management binary. No separate frontend checkout, Node.js installation, or Vite service is needed to run a package. First access opens the setup wizard to create an administrator username and password. Chinese is the default language; English is available.

This is a preview version. Download native artifacts from this repository's [Package workflow](https://github.com/lcy0828/dst-admin-go/actions/workflows/package.yml); tagged versions are published to [Releases](https://github.com/lcy0828/dst-admin-go/releases). Actions downloads require GitHub sign-in. Private repositories and images require access.

## Installation options

| Need | Guide |
| --- | --- |
| One Linux x86_64 game host, recommended | Docker All-in-One below |
| Native Linux/macOS or remote Agents | [Installation and startup](docs/startup-guide.en.md) |
| Install DST or adopt existing game files | [Game server management](docs/game-installation-management.en.md) |
| Install or update LuaJIT2 | [LuaJIT installation](docs/luajit-installation.en.md) |
| Multiple hosts or separate world containers | [Deployment profiles](docs/deployment-profiles.en.md) |
| Upgrades, backups, rollback, source packaging | [Deployment and rollback](docs/deployment-and-rollback.en.md) |
| Source development | [Development guide](docs/development.en.md) |

Start with one Master+Caves room on a 2-core/4-GB host. A single host uses the in-process Runtime and needs no extra Agent. Klei's Linux game server is amd64; ARM game hosts require emulation and are not recommended.

## Docker quick start

Install [Docker Engine and Compose](https://docs.docker.com/engine/install/), reserve at least 20 GB of disk space, and obtain a [Klei Cluster Token](https://accounts.klei.com/account/game/servers?game=DontStarveTogether).

Clone only this repository:

```bash
git clone --branch feature/v2-rebuild --single-branch https://github.com/lcy0828/dst-admin-go.git
cd dst-admin-go
cp deploy/docker/all-in-one.env.example deploy/docker/.env
```

Set `DST_ADMIN_IMAGE=ghcr.io/lcy0828/dst-admin-go/all-in-one:preview` in `.env`. Check `DST_ADMIN_DATA_ROOT` (default `/opt/dst`) and ports. Keep the existing `.env` on upgrades. Pin a version tag or image digest for stable deployments.

For private images, log in to `ghcr.io` with GitHub credentials that have `read:packages`:

```bash
docker login ghcr.io
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml pull
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml up -d
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml ps
curl -fsS http://127.0.0.1:8080/api/v2/auth/session
```

Open `http://SERVER_IP:8080`. Create an administrator, verify environment and paths, install or adopt DST, enter a Token, and create a room or import saves. Explicitly start Master/Caves, inspect their status and logs, then test a player connection.

A working page does not mean DST is installed. Install it from **Game server management**; LuaJIT2 is optional. Adopting game files does not move or delete saves. Save import is a separate operation.

To build an image yourself, install Node.js 22+, Git, and Docker Buildx on the build machine:

```bash
./deploy/scripts/build-all-in-one-image.sh --tag dst-admin/all-in-one:preview
```

The script fetches the official frontend `master` and embeds it in the binary. No prior frontend checkout is needed; private source requires Git access. Set `.env` to this local image tag. See [packaging](docs/deployment-and-rollback.en.md).

## Published images

| Image (prefix `ghcr.io/lcy0828/dst-admin-go/`) | Purpose |
| --- | --- |
| `all-in-one` | UI, Controller, and local game Runtime |
| `control-plane` | UI and Controller; remote nodes run games |
| `agent` | Standalone remote Agent for registered container Runtimes |
| `dst-runtime` | Base environment for separate world containers |

Main-repository CI builds and pushes all four images with a `preview` tag. Agents have no standalone web UI. Use the native Agent for native DST; see [Agent installation](docs/startup-guide.en.md#connect-a-remote-agent). Persist the Agent image's `/var/lib/dst-admin-agent` directory for identity, configuration, and operation state.

## Ports and persistence

| Purpose | Default All-in-One ports |
| --- | --- |
| Management UI | `8080/tcp` |
| Player connections | `10999-11020/udp` |
| Steam authentication | `8766-8790/udp` |
| Steam listing | `27016-27040/udp` |

Connect to the Master's player port, for example `c_connect("PUBLIC_IP", 10999)`. Check cloud security groups, firewalls, and port forwarding. Web access does not prove UDP access. Split rooms also require connectivity to the Master's shard communication port.

| Default host path | Contents |
| --- | --- |
| `/opt/dst/control` | Configuration, accounts, database |
| `/opt/dst/saves` | Rooms and saves |
| `/opt/dst/server` | DST game files |
| `/opt/dst/workshop` | Mods |
| `/opt/dst/backups` | Backups |
| `/opt/dst/maps` | Maps |

A custom `DST_ADMIN_DATA_ROOT` changes the host path; container paths remain `/opt/dst`. Mount other existing directories before adopting them. Keep the same data mount when upgrading. Do not remove data directories or purge volumes. Stop rooms gracefully and back up configuration, databases, Agent identities, and saves before migration.

## Maintenance

```bash
# Logs
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml logs --tail=200 dst-admin
# Stop; keep bind-mounted data
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml down
# Start with the same configuration
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml up -d
```

Before upgrading, save and stop rooms, back up data, and retain the old image. Pull or build the new image, update its tag in `.env`, keep the data directory unchanged, and run `up -d`. Check login, rooms, installation status, and game logs afterward. Check database compatibility before rollback; see [deployment and rollback](docs/deployment-and-rollback.en.md).

Use HTTPS and strong passwords for public access. Issue reports should include versions, error codes, request IDs, and redacted logs. Never upload passwords, Tokens, Agent keys, or real saves.

## Development and version provenance

This repository releases the complete product. The official frontend is [GitHub dst-admin-vue](https://github.com/lcy0828/dst-admin-vue), branch `master`. The old Vue 2 version is preserved on `legacy/vue2-original` and is excluded from current packages.

Each build resolves one frontend commit; all images and native packages in a CI run use that same revision. Check `dst-admin -version`, native `manifest.json`, or the `io.dst-admin.frontend.commit` image label. Rebuild to include subsequent frontend updates; running installations never replace their pages automatically.
