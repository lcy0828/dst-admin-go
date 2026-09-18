# Installation and startup guide

[简体中文（默认）](startup-guide.md) | **English**

This guide follows the current source: prepare the environment, start the management service, install the game, then start your worlds.
Release binaries embed the web UI and serve it alongside the API. No frontend build or Vite process is needed on the deployment host.
First installation and upgrades use different steps. For an existing deployment, read [upgrades and data protection](#upgrades-and-data-protection) first.

## Choose a deployment

| Need | Instructions | Default management address |
| --- | --- | --- |
| Host games on one Linux server; recommended | [README: Docker quick start](../README.en.md#docker-quick-start) | `http://SERVER_IP:8080` |
| Run directly on Linux with systemd | [Linux native deployment](#linux-native-deployment) | `http://SERVER_IP:8000` |
| Use local Steam game files on macOS | [macOS local deployment](#macos-local-deployment) | `http://127.0.0.1:8000` |
| Add a remote machine to an existing service | [Connect a remote Agent](#connect-a-remote-agent) | Existing management UI |
| Centrally manage several All-in-One instances | [Join an existing management instance to a Controller](#join-an-existing-management-instance-to-a-controller) | Controller UI |
| Modify frontend/backend source | [Development setup](development.en.md) | Vite `5173`, API `8000` |

The default for a 2-core/4-GB host is one room with Master and Caves, without CPU pinning or reserving a whole core for the system.
A single host uses the embedded Runtime directly; it does not need an additional local Agent. Choose the Controller-only role when managing only remote machines.
For advanced options such as independent world containers, see [deployment models](deployment-profiles.en.md). Their installation capabilities differ from the Native/All-in-One setup here.

## Linux native deployment

These commands target **Debian 12 amd64**. Download the complete package from this repository and run installation commands from its extracted directory.
For an existing DST installation, keep its service user and data directories. Skip user creation, directory creation, and SteamCMD download steps that are already complete.
Do not recursively change permissions or move directories containing active saves.

### 1. Install dependencies and prepare a user

```bash
sudo apt-get update
sudo apt-get install -y ca-certificates curl tar tmux lua5.1 python3 \
  lib32gcc-s1 lib32stdc++6 libcurl3-gnutls build-essential sqlite3 git
```

For a fresh installation, create a dedicated user. tmux requires a working shell; do not use `nologin` or `false`:

```bash
sudo useradd --system --user-group --create-home --home-dir /var/lib/dst --shell /bin/bash dst
sudo install -d -o dst -g dst -m 0750 \
  /opt/dst /opt/dst/server /opt/dst/saves /opt/dst/backups /opt/dst/maps \
  /opt/dst/workshop /opt/dst/workshop/steamapps/workshop/content/322330 \
  /opt/dst/steamcmd
```

Create the configured directories before installing. The game directory can be empty. Game and save directories must be separate and must not contain each other.
Do not repeat `useradd` for an existing user. Check ownership of existing directories, then replace `dst` and the paths in later commands with your actual values.

### 2. Prepare SteamCMD

If SteamCMD already works, record its absolute path. For a fresh deployment, follow Valve's [SteamCMD instructions](https://developer.valvesoftware.com/wiki/SteamCMD):

```bash
sudo -u dst sh -c 'cd /opt/dst/steamcmd && curl -fL https://steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz -o steamcmd_linux.tar.gz && tar -xzf steamcmd_linux.tar.gz'
sudo -u dst /opt/dst/steamcmd/steamcmd.sh +quit
```

The second command should finish self-updating and exit. Fix missing dynamic loaders or 32-bit libraries before continuing.
This step does not download DST. Install the game through the UI later. Set the SteamCMD configuration path to `/opt/dst/steamcmd/steamcmd.sh`, replacing the example default of `/usr/games/steamcmd`.

### 3. Obtain the native package

Choose a version from the [stable release page](https://github.com/lcy0828/dst-admin-go/releases/latest). Download, verify and extract it below. In mainland China, prefix `https://github.com/` with `https://ghfast.top/`.

```bash
DST_ADMIN_VERSION=v1.0.1
DST_ADMIN_DOWNLOAD=https://github.com/lcy0828/dst-admin-go/releases/download
curl -fL "$DST_ADMIN_DOWNLOAD/$DST_ADMIN_VERSION/dst-admin-$DST_ADMIN_VERSION-linux-amd64.tar.gz" -o "dst-admin-$DST_ADMIN_VERSION-linux-amd64.tar.gz"
curl -fL "$DST_ADMIN_DOWNLOAD/$DST_ADMIN_VERSION/dst-admin-$DST_ADMIN_VERSION-linux-amd64.tar.gz.sha256" -o "dst-admin-$DST_ADMIN_VERSION-linux-amd64.tar.gz.sha256"
sha256sum -c "dst-admin-$DST_ADMIN_VERSION-linux-amd64.tar.gz.sha256" && \
  tar -xzf "dst-admin-$DST_ADMIN_VERSION-linux-amd64.tar.gz" && \
  cd "dst-admin-$DST_ADMIN_VERSION-linux-amd64" && ./dst-admin -version
```

Set `DST_ADMIN_VERSION` to the stable release you want to install. The output must contain `embeddedWebUI: true` and `frontendCommit`. The package includes the UI, Agent, map renderer, helpers, and installation templates; the runtime host needs no Go or Node.js. Execute the remaining commands from the extracted directory.

To build instead, use this repository's [packaging guide](deployment-and-rollback.en.md). Only the build machine needs Go, a C compiler, Git, and Node.js.

### 4. Configure and install

Copy the template outside the repository. Do not use the actual repository `conf/app.conf`:

```bash
mkdir -p ../dst-admin-local
cp deploy/systemd/local.conf.example ../dst-admin-local/app.conf
chmod 600 ../dst-admin-local/app.conf
```

Edit `../dst-admin-local/app.conf` and check each setting:

| Setting | Initial value or meaning |
| --- | --- |
| `[deployment] PACKAGING` | `native` |
| `[fleet] LOCAL_EXECUTOR_ENABLED` | `true` |
| `[fleet] CONTROLLER_ENABLED`, `MEMBER_ENABLED` | Both `false` for a standalone host |
| `[database] PATH` | `/var/lib/dst-admin/go-dont.db` |
| `[fleet] STATE_PATH` | `/var/lib/dst-admin/fleet` |
| `[paths] DST_SERVER_PATH` | `/opt/dst/server`, or the existing game directory |
| `[paths] DST_SAVE_PATH` | `/opt/dst/saves`, the parent directory of the Clusters |
| `[paths] DST_BACKUP_PATH`, `DST_MAP_PATH` | `/opt/dst/backups`, `/opt/dst/maps` |
| `[paths] DST_UGC_PATH` | `/opt/dst/workshop/steamapps/workshop` |
| `[mod] STEAM_CMD_PATH` | Actual absolute path to SteamCMD |
| `[mod] WORKSHOP_MOD_PATH` | `/opt/dst/workshop` |
| `[mod] WORKSHOP_CONTENT` | `/opt/dst/workshop/steamapps/workshop/content/322330` |
| `[map] RENDERER_PATH` | `/usr/local/bin/dst-map-renderer` |

For first installation, keep `SETUP = incomplete` from the template and create an administrator in the UI.
Existing services must retain their configuration and database; do not reset them with the initial template.
The room's Cluster Token and the Agent connection key serve different purposes.

```bash
sudo deploy/scripts/install-native-local.sh \
  --binary "$PWD/dst-admin" \
  --renderer "$PWD/dst-map-renderer" \
  --config "$PWD/../dst-admin-local/app.conf" \
  --user dst
sudo systemctl enable --now dst-admin-local
sudo systemctl status dst-admin-local --no-pager
curl -fsS http://127.0.0.1:8000/api/v2/auth/session
```

The service should be `active`, and the endpoint should return JSON. The default listener is `0.0.0.0:8000`.
systemd explicitly sets `DST_ADMIN_CONFIG=/var/lib/dst-admin/app.conf` and serves the embedded UI. Future configuration changes belong in this installed location.
`HTTP_PORT` does not override a startup `-addr` argument. To change the listener, use a systemd override and retain the other service settings.

```bash
journalctl -u dst-admin-local -n 100 --no-pager
sudo systemctl restart dst-admin-local
```

Open the UI and follow [first game startup](#first-game-startup-and-existing-saves). For a reverse proxy example, see [deployment and rollback](deployment-and-rollback.en.md#nginx-same-origin-proxy).

## Connect a remote Agent

In **Machines & connections → Agent security → Linux**, copy the installer to download and verify the latest stable Agent while preserving existing configuration and identity. Choose GHFast or GitHub direct; Go is not required. Docker uses `agent-latest`. For a new game node, deploy All-in-One and join the management center.

Releases also provide `dst-admin-agent-linux-amd64.tar.gz` and `dst-admin-agent-darwin-arm64.tar.gz`, containing the Agent and a configuration example. Use the full native package below for system service installers.

The Controller provides the UI. The Agent downloads, installs, and operates rooms on the remote machine; it has no separate management page.
The Agent connects outbound to the Controller's `/agent`. Ordinary commands do not require an additional inbound TCP port on the Agent.
Game UDP ports must still be open on the machine running each world.

### 1. Enable the Controller

1. Open **Machine Management** (`/agents/list`), then **Deployment role**. Choose the local + Controller role, or Controller-only if it will not run local worlds. Before switching roles, stop worlds on any local Runtime target that will be disabled.
2. Stop local worlds and wait for background tasks, then save. The role applies within the current process without restarting the management service.
3. Open **Agent security settings** (`/agents/security`). For first enrollment with no existing Agents, generate a new key and save the plaintext shown this time for the remote `[agent] SECURITY_KEY`.
4. Check connectivity from the remote host: on a LAN, use `ws://CONTROLLER_IP:8080/agent` (native deployments default to `8000`); behind an HTTPS proxy, use `wss://DOMAIN/agent`.

For existing Agents, reuse the securely saved connection key. Do not rotate it just to add a node.
The UI only shows a masked existing key. If the key is lost, follow [key rotation](deployment-and-rollback.en.md#key-rotation) for all affected nodes.
Write keys to a configuration file with mode `0600`; do not include them in WebSocket URLs or command lines.

### 2. Prepare the remote environment

The remote host needs the same tmux, game libraries, SteamCMD, service user, and data directories described in [Linux native deployment, steps 1 and 2](#linux-native-deployment).
An Agent-only host does not need Node.js, frontend files, or a management database. You can build the Agent elsewhere for the target OS/architecture and copy it over.

The native package includes `dst-admin-agent` and `deploy/`. Copy a package matching the remote system and architecture, extract it there, and run:

```bash
mkdir -p ../dst-agent-local
cp deploy/systemd/agent.conf.example ../dst-agent-local/agent.conf
chmod 600 ../dst-agent-local/agent.conf
```

Edit the configuration, keeping at least the connection and Runtime sections:

```ini
[agent]
SERVER_URL = wss://dst.example.com/agent
SECURITY_KEY = REPLACE_WITH_CONTROLLER_CONNECTION_KEY

[runtime.native]
DRIVER = native
SAVE_PATH = /opt/dst/saves
SERVER_PATH = /opt/dst/server
STEAMCMD_PATH = /opt/dst/steamcmd/steamcmd.sh
UGC_PATH = /opt/dst/workshop/steamapps/workshop
WORKSHOP_CONTENT_PATH = /opt/dst/workshop/steamapps/workshop/content/322330
MOD_CACHE_PATH = /var/lib/dst-admin-agent/mod-cache
MOD_STATE_PATH = /var/lib/dst-admin-agent/mod-state
SERVER_MODE = 64
```

All these paths belong to the **Agent machine**. `[runtime.native]` registers installation ID `native`.
Configuring only `[agent]` establishes a connection but cannot manage an unregistered game location.
Use different `[runtime.NAME]` sections for multiple installations. Save roots must not overlap or be controlled by multiple writers.

### 3. Install, start, and verify

```bash
sudo deploy/scripts/install-native-agent.sh \
  --binary "$PWD/dst-admin-agent" \
  --config "$PWD/../dst-agent-local/agent.conf" \
  --user dst
sudo systemctl enable --now dst-admin-agent
sudo systemctl status dst-admin-agent --no-pager
journalctl -u dst-admin-agent -n 100 --no-pager
```

The installed configuration is `/var/lib/dst-admin-agent/agent.conf`. Operation state is `runtime-state.json` in the same directory, and the binary is under `bin/dst-admin-agent`.
`/etc/dst-admin/agent.env` is optional. Its connection URL and key override the configuration file; check for stale values when troubleshooting.

Back on the Controller, verify:

1. The Agent is online in **Machine Management**, with installation paths matching the remote configuration.
2. A single registered installation is normally selected automatically. If there are several, select the correct one in the machine's Runtime configuration.
3. Select the machine in **Game server management**, install the game or connect an existing directory, and check that the job succeeds and reports a version.
4. Create or import a room, place its worlds on that Agent, and check world status and logs after starting it.

An online Agent proves connectivity; it does not prove game installation or world startup.
Game installation management requires `runtime.game-install.v1`; LuaJIT management requires `runtime.luajit.v2`.
If prompted to upgrade, build the Agent from sources matching the Controller.
Stopping an Agent usually leaves native worlds running. Stop worlds through room management and confirm completion.

## Join an existing management instance to a Controller

If the remote host already runs All-in-One or a full native management service, switch its deployment role to join a management center, enter the upstream `ws(s)://…/agent` URL and connection key, then stop local worlds, wait for background tasks, and save to apply.
Its embedded Member connects upstream and continues managing its existing files. Do not install a second standalone Agent to control the same saves.
After joining, perform writes through the Controller; the remote UI remains available for reading.

When environment variables lock the role, the UI indicates that the environment manages it. Change the deployment environment and restart.
Embedded Members and standalone Agents use different variable names; see the [role variable table](deployment-profiles.en.md#fleet-roles).

## macOS local deployment

macOS uses the signed-in user's Steam game files. Linux one-click downloads and LuaJIT installation are not available on macOS.

1. Install tmux with `brew install tmux`.
2. Install DST through Steam. Apple Silicon needs Rosetta 2 for the x86_64 game.
3. Download the macOS package (`darwin-arm64` for Apple Silicon), verify it with `shasum -a 256 -c FILE.tar.gz.sha256`, and extract it. Run the following commands there. Intel Macs can build `darwin-amd64` locally using the packaging guide.
4. Copy `local.conf.example` outside the repository and edit it. Use actual absolute paths writable by the current user; do not use `~` or `$HOME` inside INI values.

For first installation, prepare configuration from the extracted package directory:

```bash
mkdir -p ../dst-admin-local
cp deploy/systemd/local.conf.example ../dst-admin-local/app.conf
chmod 600 ../dst-admin-local/app.conf
```

| Setting | macOS example (replace `alice` with your username) |
| --- | --- |
| `[database] PATH` | `/Users/alice/Library/Application Support/DST Admin/go-dont.db` |
| `[fleet] STATE_PATH` | `/Users/alice/Library/Application Support/DST Admin/fleet` |
| `[paths] DST_SERVER_PATH` | `/Users/alice/Library/Application Support/Steam/steamapps/common/Don't Starve Together` |
| `[paths] DST_SAVE_PATH` | `/Users/alice/Documents/Klei/DoNotStarveTogether`, adjusted to the actual save location |
| `[paths] DST_BACKUP_PATH`, `DST_MAP_PATH` | Backup and map directories created by the user |
| `[paths] DST_UGC_PATH` | `steamapps/workshop` inside a user-created SteamCMD library |
| `[mod] WORKSHOP_MOD_PATH`, `WORKSHOP_CONTENT` | That SteamCMD library root and its `steamapps/workshop/content/322330` |
| `[mod] STEAM_CMD_PATH` | Absolute path to installed SteamCMD; may stay empty if not downloading mods yet |
| `[mod] LUA_BINARY`, `PYTHON_BINARY` | Absolute paths to installed interpreters; the external Lua parsing fallback requires a separate Lua 5.1 installation |
| `[map] RENDERER_PATH` | `/Users/alice/Library/Application Support/DST Admin/bin/dst-map-renderer` |

Create configured directories that do not exist, then install **without sudo**:

```bash
deploy/scripts/install-macos-local.sh \
  --binary "$PWD/dst-admin" \
  --renderer "$PWD/dst-map-renderer" \
  --config "$PWD/../dst-admin-local/app.conf"
launchctl print "gui/$(id -u)/top.luocaiyi.dst-admin-local"
curl -fsS http://127.0.0.1:8000/api/v2/auth/session
```

The installer immediately starts the LaunchAgent. Installed configuration is `~/Library/Application Support/DST Admin/app.conf`; logs are in its `logs/` subdirectory.
Update Steam-managed game files through Steam. See [macOS game support (Chinese)](macos-dedicated-server.md) for more path details.
For an Agent-only macOS host, see [macOS launchd Agent](container-and-native-deployment.en.md#macos-launchd-agent).

## First game startup and existing saves

An instance without an administrator automatically opens `/setup`: **Administrator → Management role → Environment → Game server → Prepare rooms → Final checks**.
Account creation asks for a username, password, and password confirmation, using the server's configured password policy. Chinese is the default, with an English switch.

Progress is stored in the database and survives page refreshes, login, and service restarts.
Role, directory, and runtime tool settings saved in the UI apply without restarting the management service. Stop local worlds and wait for background tasks first; failed application rolls back the configuration. Changing paths does not move, import, or delete saves. Environment-controlled settings still require a deployment update and restart.
The game step supports installation or adoption of an existing directory, with optional LuaJIT. Save imports create new rooms; replacement of existing rooms is unavailable in the wizard.
For remote rooms, use **Configure runtime nodes** to place and provision worlds, then return from the page header to recheck. A catalog entry alone does not count as a ready room.
Save uploads in the wizard are for local hosting. A Controller-only instance discovers existing saves from the Agent’s configured save directory; provisioning a new room does not transfer local Session saves.
Existing administrators are not forced through initialization after upgrades. Reopen it from **Initial setup** in the page header.

Finishing setup does not mean the game is running. Final checks list outstanding tasks, with options to host later or open room controls and explicitly start worlds.
Completing steps does not automatically download games, start/stop worlds, regenerate rooms, or clean saves.

1. Open the management page and create an administrator through the setup wizard.
2. Open **Game server management** and confirm the selected machine and installation. For a new machine, click **Install game server**. For existing game files, configure their path directly, or choose **Use an existing server** on an empty registered location, inspect it, and confirm.
3. If needed, follow [LuaJIT installation](luajit-installation.en.md) after installing the game. The original game does not require LuaJIT.
4. Create a room or import existing saves, enter a valid Cluster Token, and configure Master/Caves and ports.
5. Start the worlds, confirm that they show as running and logs indicate a completed world load, then test a real player connection.

Connecting game files and importing saves are separate actions. Selecting a game directory keeps the configured save directory and does not move or delete saves.
See [game installation management](game-installation-management.en.md) for existing-directory and online-installation requirements.
Use paths visible inside the container; arbitrary host paths do not automatically become accessible there.

Web connectivity does not prove game connectivity. Allow the player, Steam authentication, and Steam listing UDP ports configured for each world.
For worlds on different machines, secondary shards must also reach the Master's shard communication address and port. See the README for All-in-One's default port ranges.

## Upgrades and data protection

| Deployment | Normal restart and logs | Retain during upgrades |
| --- | --- | --- |
| All-in-One | `docker compose restart` from the deployment directory | Host data directory, Compose file, and old image |
| Linux local | `systemctl restart dst-admin-local`; `journalctl -u dst-admin-local` | `/var/lib/dst-admin` and every configured data directory |
| Linux Agent | `systemctl restart dst-admin-agent`; `journalctl -u dst-admin-agent` | `/var/lib/dst-admin-agent`, optional `agent.env`, game and save directories |
| macOS local | `launchctl kickstart -k "gui/$(id -u)/top.luocaiyi.dst-admin-local"` | Configuration/state in Application Support and external game/save directories |

Before updating, create recoverable backups of saves, the database, and configuration. Retain the old binaries/frontend or image.
Stop affected worlds normally before copying saves. Stopping a native management service or Agent does not stop the game.
For SQLite, copy after stopping writes or use `.backup`; copying only an actively written main database file is insufficient.

Installers copy the supplied configuration into the installed location. For upgrades, first back up the **currently active configuration** to another file, review it, and install using that copy.
Do not supply the initial template again or use the same file for source and destination.
Preserve the Agent identity and key stored in configuration, and `runtime-state.json`.
After installation, explicitly restart existing Linux services. macOS installers immediately restart the corresponding LaunchAgent.

A save root must have only one local controller. Do not delete `owner.lock`, tmux sockets, or save files to resolve conflicts.
Stop the previous manager normally, retain the service user and save paths, then start the new management process.
See [deployment and rollback](deployment-and-rollback.en.md) for a complete release procedure.

## Startup troubleshooting

| Symptom | Check and resolution |
| --- | --- |
| `app.conf` not found | Point `DST_ADMIN_CONFIG` at an existing configuration; WorkingDirectory alone does not load its `app.conf` |
| `go-sqlite3 requires cgo` | Rebuild the management service with `CGO_ENABLED=1` and install a C compiler |
| Missing data directory or Permission denied | Check absolute paths, service user, and parent-directory access; create configured empty directories before first local startup |
| SteamCMD unavailable | Run its registered absolute path as the service user; check 32-bit libraries, write permissions, and download connectivity |
| Management API works but the page returns 404 | Check for a built `index.html` under `DST_ADMIN_WEB_ROOT` |
| Agent offline | Check that the Controller role has been applied, the `/agent` URL, keys on both ends, environment overrides, and proxy WebSocket support |
| Agent online but installation unavailable | Check `[runtime.NAME]`, SteamCMD, Agent capabilities, and the installation selected in the UI |
| `RUNTIME_OWNER_CONFLICT` | Check whether a local management service and another Agent both control the saves; stop the extra manager normally |
| Installation/validation rejected while games run | Stop every world using that game installation through room management, confirm it stopped, then retry |
| LuaJIT system-library incompatibility | Choose a compatible build for the node's OS; see the LuaJIT guide. Ordinary startup does not repair it |
| Worlds run but players cannot connect | Check the Master's player UDP port, public address, firewall, and router forwarding |

[Scheduled tasks and room backups](automation.en.md)
