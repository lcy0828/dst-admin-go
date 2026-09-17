# DST Admin

[简体中文（默认）](README.md) | **English**

First-time access opens the setup wizard. Create an administrator, then prepare the environment, game, and rooms.
Progress survives refreshes and restarts; see [first game startup](docs/startup-guide.en.md#first-game-startup-and-existing-saves).

DST Admin is a dedicated server management panel for Don't Starve Together. Server owners can create rooms, start worlds, install mods, manage players, inspect logs, and back up saves in a browser.

Two repositories make up the project. In production, one management service serves both the API and the built frontend:

| Repository | Purpose |
| --- | --- |
| `dst-admin-go` (this repository) | Management service, DST processes, saves, mods, and jobs |
| [`dst-admin-vue`](https://git.luocaiyi.top/dst/dst-admin-vue) | Browser interface, built and served by the management service |

> **This is a source preview.** Current backend development is on `feature/v2-rebuild`; the frontend is on `master`. There are no stable tags, installation packages, or prebuilt images published by this repository. It is not recommended for important servers that cannot tolerate downtime or manual recovery.

## Start here

| Goal | Guide |
| --- | --- |
| Open your first server on one Linux host | [Docker quick start](#docker-quick-start) below; All-in-One is recommended |
| Install natively or add a remote Agent | [Installation and startup](docs/startup-guide.en.md) |
| Connect existing game files and saves | [Game installation management](docs/game-installation-management.en.md); import saves separately through room management |
| Install LuaJIT2 and follow upstream releases | [LuaJIT installation](docs/luajit-installation.en.md) |
| Develop the frontend and backend locally | [Development setup](docs/development.en.md) |
| Upgrade, migrate, or roll back an existing service | [Deployment and rollback](docs/deployment-and-rollback.en.md) |

For an existing service, follow the upgrade procedure and preserve its active configuration, database, Agent identity, and save directories. Do not reapply first-install templates.

## Recommended hardware

On an x86_64 Linux host with Docker, use **All-in-One**. The web interface, management service, SteamCMD, tmux, and DST run in one container, with persistent data on the host.

| Host | Recommendation |
| --- | --- |
| 2 CPU cores, 4 GB RAM | Master + Caves; avoid adding more worlds |
| 4 CPU cores, 8 GB RAM | Master + Caves, with capacity for the OS and maintenance |
| Multiple hosts | Complete one local installation first, then add Agents as needed |

Capacity advice counts all running worlds on a host. Up to two effective CPUs have no whole-core reservation; hosts with three or more reserve one CPU for the OS. Memory is checked separately. CPU pinning is not recommended by default on shared VPS hosts.

The [product usage scenarios](docs/product-usage-scenarios.md) define the architecture, performance, and UI baseline: prioritize one Master+Caves room on a 2-core/4-GB All-in-One host, then extend to multiple All-in-One hosts, All-in-One plus Agents, and shards on different machines.

## Docker quick start

### Prerequisites

- Docker Engine and the Compose plugin ([official installation guide](https://docs.docker.com/engine/install/)).
- At least 2 CPU cores, 4 GB RAM, and 20 GB free disk space.
- Access to firewall and cloud security group settings.
- A DST Cluster Token from the [Klei server page](https://accounts.klei.com/account/game/servers?game=DontStarveTogether).

Klei currently ships the Linux DST dedicated server for `amd64`. ARM hosts require emulation and are not recommended for ordinary server deployments.

### 1. Get matching backend and frontend sources

```bash
git clone --branch feature/v2-rebuild --single-branch \
  https://git.luocaiyi.top/dst/dst-admin-go.git
git clone --branch master --single-branch \
  https://git.luocaiyi.top/dst/dst-admin-vue.git dst-admin-vue-v3
cd dst-admin-go
```

Keep the repositories next to each other:

```text
your-directory/
├── dst-admin-go/
└── dst-admin-vue-v3/
```

### 2. Build and start

Check Docker from the backend repository root:

```bash
docker version
docker compose version
```

Build the combined image and save the deployment settings:

```bash
./deploy/scripts/build-all-in-one-image.sh \
  --tag dst-admin/all-in-one:preview

cp deploy/docker/all-in-one.env.example deploy/docker/.env
```

Edit `deploy/docker/.env` to confirm the image tag, host data directory, and ports, then start:

```bash
docker compose --env-file deploy/docker/.env \
  -f deploy/docker/compose.all-in-one.yaml up -d
```

The first build downloads base images and dependencies; duration depends on your network.
Run subsequent commands from the repository root with the same `--env-file` so restarts retain the chosen image and data directory.
Do not copy the template over an existing `.env`. This file is ignored by Git.

### 3. Check the management service

```bash
docker compose --env-file deploy/docker/.env \
  -f deploy/docker/compose.all-in-one.yaml ps
curl -fsS http://127.0.0.1:8080/api/v2/auth/session
```

The container should become `healthy`, and the session endpoint should return JSON. Inspect logs if it fails:

```bash
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml \
  logs --tail=200 dst-admin
```

### 4. Open your first game server

Visit `http://SERVER_IP:8080` (use your configured port if different), then:

1. Create an administrator account.
2. Open **Game server management**, choose the host, and click **Install game server**. Wait for the job to finish, or inspect and connect an existing game directory on that host.
3. Enter your Cluster Token.
4. Create a room or import existing saves.
5. Add Master and Caves.
6. Check ports and start the room.
7. Wait until both worlds show **Running**.
8. Join using the address or direct-connect command shown in the UI.

A working management page does not mean DST is installed. Complete step 2 before starting worlds.

The same page shows installation status, versions, and paths for the local Runtime and Agents. Native deployments must configure SteamCMD and game/save paths first. Connecting existing game files does not import or delete saves. See [game installation management](docs/game-installation-management.en.md).

## Successful installation checklist

- The `dst-admin` container is `healthy` in Compose.
- You can log in and save settings.
- The game server is shown as installed.
- Master and Caves transition from **Starting** to **Running**.
- Live logs show world loading and server registration.
- External players can connect through the Master's player port.

Deployment is not complete until every applicable check succeeds.

## Ports and player connections

Default All-in-One mappings:

| Purpose | Default range |
| --- | --- |
| Management page | `8080/tcp` |
| DST player ports | `10999-11020/udp` |
| Steam authentication | `8766-8790/udp` |
| Steam server listing | `27016-27040/udp` |

Players connect to the Master's player port, for example:

```lua
c_connect("SERVER_PUBLIC_IP", 10999)
```

Check cloud security groups, the host firewall, and router port forwarding. Access to the web page proves only that the management TCP port is reachable, not the game's UDP ports.

## Persistent data

All-in-One stores persistent data under `/opt/dst` on the host by default:

| Directory | Contents |
| --- | --- |
| `/opt/dst/control` | Management configuration, accounts, and database |
| `/opt/dst/saves` | Rooms, worlds, and game saves |
| `/opt/dst/server` | DST dedicated server |
| `/opt/dst/workshop` | Steam Workshop mods |
| `/opt/dst/backups` | Manual, scheduled, and protective backups |
| `/opt/dst/maps` | Rendered map artifacts |

Replacing or removing the container does not automatically remove this bind-mounted directory. When migrating, copy the entire data root and keep an additional offline backup.

With a custom `DST_ADMIN_DATA_ROOT`, host paths change accordingly; the container path remains `/opt/dst`. Enter container-visible paths in the UI. To connect an existing game outside the default mount, mount its source directory into the container first.

Do not manually move saves, replace the database, or edit managed configuration files while worlds are running.

## Common maintenance commands

### Status and logs

```bash
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml ps
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml \
  logs --tail=200 dst-admin
```

### Stop and start again

```bash
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml down

docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml up -d
```

On shutdown, the container first asks managed worlds to save and exit. Stopping it does not delete `/opt/dst`.

### Update the source preview

Before updating, back up and stop rooms through the panel, stop management writes, and back up the host data root and `.env`. Retain the current image tag or image ID and use a different tag for the new image. Resolve any uncommitted source changes before pulling updates.

```bash
git pull --ff-only
git -C ../dst-admin-vue-v3 pull --ff-only

./deploy/scripts/build-all-in-one-image.sh \
  --tag dst-admin/all-in-one:preview-next
```

Set `DST_ADMIN_IMAGE` in `.env` to the new tag while keeping `DST_ADMIN_DATA_ROOT` unchanged:

```bash
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml up -d
```

Check container health, login, and the room list, then start the rooms you need and verify their logs. Use a new tag for each subsequent update. Check database compatibility before switching to an older image; see [deployment and rollback](docs/deployment-and-rollback.en.md).

### Uninstall while keeping data

```bash
docker compose --env-file deploy/docker/.env -f deploy/docker/compose.all-in-one.yaml down
```

Deleting source directories or images does not delete `/opt/dst`. Handle that data directory separately only after confirming that backups are recoverable and the server is no longer needed.

## Usage notes

- A room represents one DST Cluster and may contain Master, Caves, and other worlds.
- Master is the room's unique primary shard role; it does not necessarily mean a forest world.
- Downloading a mod only stores its files. Add it to a room and enable it for the intended worlds.
- Rooms and worlds can use different mod configurations.
- Restart affected worlds after enabling, disabling, or configuring mods.
- DST continuously writes saves; backups restore, download, or migrate a complete room.
- Season/day snapshots retained after a world stops are marked stale; they are not live state.

## Troubleshooting

### The page opens, but a world will not start

Check game installation, the Cluster Token, free disk space, and the failed job's error and request ID.

### Players cannot connect

Use the Master's player port and check UDP firewall rules, cloud security groups, and public port forwarding.

### Can two rooms use the same ports?

The same ports can be saved in configuration, but the rooms cannot run simultaneously on the same machine. Assign different ports if they must run together.

### Why did a downloaded mod not take effect?

DST reads enabled mods and `modoverrides.lua` when a world starts. Restart affected worlds after changing them.

### What should a useful issue report include?

Include:

- The request ID shown in the UI.
- The failed job's error code and target.
- Output from `docker compose ... ps`.
- The most recent 200 management-service log lines.
- Names of affected rooms and worlds.

Do not publish Cluster Tokens, Steam API keys, administrator passwords, or Agent keys.

## Other deployment options

All-in-One is recommended for most server owners. These options require more system administration knowledge:

- [Linux/macOS native installation and remote Agents](docs/startup-guide.en.md)
- [Deployment models and roles](docs/deployment-profiles.en.md)
- [Multi-machine architecture and boundaries (Chinese)](docs/distributed-room-management.md)
- [Mod management (Chinese)](docs/mod-management.md)
- [Backup and recovery (Chinese)](docs/distributed-room-management.md)
- [LuaJIT installation and upstream updates](docs/luajit-installation.en.md)
- [Platform capabilities and limitations (Chinese)](docs/dst-platform-matrix.md)

Native deployment requires building the backend, map renderer, and frontend from matching sources using the startup guide. No stable downloadable installation package is currently published.

## Security recommendations

- Set a strong administrator password on first installation.
- Use HTTPS for public access and restrict management-page access by IP.
- Do not directly expose an unencrypted management port.
- Regularly copy save backups to another machine or object storage.
- Never commit server credentials to the repository.
- The independent-shard container profile requires the Docker socket and is intended only for trusted single-user hosts.
