# DST Admin

[简体中文](README.md) | **English**

A Don't Starve Together server panel for rooms, mods, players, logs, save backups, and remote Agents. Images and native packages include the web UI, with Chinese as the default language and English available.

## Docker quick start

Recommended: Linux x86_64, at least 2 CPU cores, 4 GB RAM, 20 GB free disk space, and [Docker with Compose](https://docs.docker.com/engine/install/).

```bash
git clone https://github.com/lcy0828/dst-admin-go.git
cd dst-admin-go/deploy/docker
cp all-in-one.env.example .env
```

Edit `.env` to set the image and data directory:

```ini
DST_ADMIN_IMAGE=ghcr.io/lcy0828/dst-admin-go/all-in-one:preview
DST_ADMIN_DATA_ROOT=/opt/dst
```

Start:

```bash
docker compose --env-file .env -f compose.all-in-one.yaml up -d
```

Open `http://SERVER_IP:8080`. Follow the setup wizard to create an administrator, install or adopt the game server, and enter a [Klei Token](https://accounts.klei.com/account/game/servers?game=DontStarveTogether). Create a room or import saves, then start Master/Caves.

Default ports: `8080/tcp` (web UI), `10999-11020/udp` (players), and `8766-8790/udp` plus `27016-27040/udp` (Steam). Allow these ports through the firewall.

Data is stored under `/opt/dst` by default: `saves/` contains game saves; `control/` contains configuration and the database. Keep this directory and the existing `.env` when upgrading.

## Other deployments

| Option | Guide |
| --- | --- |
| Native Linux / macOS | [Download packages](https://github.com/lcy0828/dst-admin-go/actions/workflows/package.yml) · [Installation](docs/startup-guide.en.md) |
| Remote Agent | [Agent setup](docs/startup-guide.en.md#connect-a-remote-agent) |
| Controller / separate world containers | [Deployment profiles](docs/deployment-profiles.en.md) |

Image prefix: `ghcr.io/lcy0828/dst-admin-go/`. Current tag: `preview`.

| Image | Purpose |
| --- | --- |
| `all-in-one` | Web UI, Controller, and local games |
| `control-plane` | Web UI and Controller |
| `agent` | Remote container Runtime management |
| `dst-runtime` | Separate world runtime |

## Documentation

- [Game server management](docs/game-installation-management.en.md) · [LuaJIT2](docs/luajit-installation.en.md)
- [Upgrades, rollback, and source packaging](docs/deployment-and-rollback.en.md)
- [Development](docs/development.en.md) · [Frontend source](https://github.com/lcy0828/dst-admin-vue)
