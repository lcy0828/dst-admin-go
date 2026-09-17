<p align="center">
  <img src="docs/assets/readme/banner.svg" alt="DST Admin — Don't Starve Together server management" width="100%">
</p>

<p align="center">
  <a href="https://github.com/lcy0828/dst-admin-go/stargazers"><img src="docs/assets/readme/metrics/stars.svg" alt="GitHub Stars"></a>
  <a href="https://github.com/lcy0828/dst-admin-go/forks"><img src="docs/assets/readme/metrics/forks.svg" alt="GitHub Forks"></a>
  <a href="https://github.com/lcy0828/dst-admin-go/actions/workflows/ci.yml"><img src="docs/assets/readme/metrics/ci.svg" alt="CI"></a>
  <a href="https://github.com/lcy0828/dst-admin-go/actions/workflows/package.yml"><img src="docs/assets/readme/metrics/package.svg" alt="Package"></a>
</p>

<p align="center"><strong>From your first server to your next adventure.</strong></p>
<p align="center">
  Manage Don't Starve Together worlds, mods, players, and saves in your browser.<br>
  Start on one machine. Connect more with Agents when you need them.
</p>

<p align="center">
  <a href="README.md">简体中文</a> · <strong>English</strong>
</p>
<p align="center">
  <a href="#docker-quick-start">Quick start</a> ·
  <a href="docs/startup-guide.en.md">Deployment guide</a> ·
  <a href="docs/luajit-installation.en.md">LuaJIT2</a> ·
  <a href="https://github.com/lcy0828/dst-admin-go/issues">Report an issue</a>
</p>

<p align="center">
  <a href="docs/assets/readme/dashboard.en.webp"><img src="docs/assets/readme/dashboard.en.webp" alt="Service overview: Master and Caves status, seasons, online players, and backup controls" width="100%"></a>
  <br><sub>Current application UI · Demo data · Click to enlarge</sub>
</p>

## Everything you need to run your worlds

| Getting started | Day-to-day management |
| --- | --- |
| **Install the game and LuaJIT2**<br>Install or update the server from the panel, or connect an existing installation. | **Manage rooms and worlds**<br>Create rooms, start or stop Master and Caves, and monitor their status. |
| **Configure worlds and mods**<br>Edit room and world settings. Enable, configure, and update mods. | **Back up and restore saves**<br>Create backups, import existing saves, and restore your worlds when needed. |
| **Connect remote machines**<br>Manage other hosts through Agents and check each node's installations and resources. | **See what's happening**<br>Check players, seasons, logs, and chat. Run commands from the console. |

Images and native packages include the web UI. Chinese is the default; English and dark mode are available.

## Docker quick start

Run the panel and game together on one machine. Recommended: **Linux x86_64 · 2 CPU cores / 4 GB RAM or more · 20 GB free disk space**. Install [Docker with Compose](https://docs.docker.com/engine/install/) first.

**1. Get the deployment files**

```bash
git clone https://github.com/lcy0828/dst-admin-go.git
cd dst-admin-go/deploy/docker
cp all-in-one.env.example .env
```

**2. Edit `.env` to set the image and data directory**

```ini
DST_ADMIN_IMAGE=ghcr.io/lcy0828/dst-admin-go/all-in-one:preview
DST_ADMIN_DATA_ROOT=/opt/dst
```

**3. Start the panel**

```bash
docker compose --env-file .env -f compose.all-in-one.yaml up -d
```

Open **`http://SERVER_IP:8080`** and follow the setup wizard:

**Create an administrator → Install or connect the game → Add a [Klei Token](https://accounts.klei.com/account/game/servers?game=DontStarveTogether) → Create a room or import saves → Start your worlds**

> Data lives in `/opt/dst` by default: `saves/` holds game saves; `control/` holds configuration and the database. Keep this directory and the existing `.env` when upgrading.

<details>
<summary><strong>Which ports should I open?</strong></summary>

Allow the ports you use through the host firewall and cloud security group. Default mappings:

| Purpose | Ports |
| --- | --- |
| Web UI | `8080/tcp` |
| Player connections | `10999-11020/udp` |
| Steam communication | `8766-8790/udp`, `27016-27040/udp` |

</details>

## Choose your deployment

| Your setup | Deployment |
| --- | --- |
| One machine for everything | **All-in-One**, using the steps above |
| Run binaries directly | [Native installation](docs/startup-guide.en.md) · [Download packages](https://github.com/lcy0828/dst-admin-go/actions/workflows/package.yml) |
| Manage more machines | [Connect a remote Agent](docs/startup-guide.en.md#connect-a-remote-agent) |
| Separate the panel and games | [Deployment profiles](docs/deployment-profiles.en.md) |

<details>
<summary><strong>Available Docker images, including Agent</strong></summary>

Image prefix: `ghcr.io/lcy0828/dst-admin-go/`. Current tag: `preview`.

| Image | Purpose |
| --- | --- |
| `all-in-one` | Web UI, Controller, and local games |
| `control-plane` | Web UI and Controller |
| `agent` | Remote container Runtime management |
| `dst-runtime` | Separate world runtime |

For remote machines running native game processes, use the native Agent package. See the [Agent setup guide](docs/startup-guide.en.md#connect-a-remote-agent).

</details>

## Explore further

- **Run and maintain**: [Game server installation](docs/game-installation-management.en.md) · [LuaJIT2 installation](docs/luajit-installation.en.md) · [Upgrades and rollback](docs/deployment-and-rollback.en.md)
- **Contribute**: [Development guide](docs/development.en.md) · [Frontend source](https://github.com/lcy0828/dst-admin-vue) · [Issue tracker](https://github.com/lcy0828/dst-admin-go/issues)

## Star history

<p align="center">
  <a href="https://github.com/lcy0828/dst-admin-go/stargazers"><img src="docs/assets/readme/metrics/stars.en.svg" alt="GitHub Stars over time" width="100%"></a>
</p>
