# Native and container deployment

[简体中文（默认）](container-and-native-deployment.md) | **English**

> Prefer native or All-in-One for new installations. If each world needs its own container, the management container provides the local Runtime directly, without an additional local Agent container. Agents are for remote machines.

For first deployment, follow the [installation and startup guide](startup-guide.en.md), which covers dependencies, users, directories, connection keys, configuration, and acceptance checks.
This document covers runtime mechanisms and advanced troubleshooting. During upgrades, retain active configuration and Agent identity instead of reapplying templates.

## Supported simple models

| Scenario | Local control | Remote extension |
| --- | --- | --- |
| Native | Management service directly controls local tmux and DST files | Enable Controller role and add Agents |
| All-in-One | Management service and all DST shards share one container | Enable Controller role and add Agents |
| Independent Docker worlds | Management container directly controls shard containers through the Docker socket | Enable Controller role and add Agents |
| Controller-only | No local DST execution | Manage Agents only |

Each machine has one local manager. Do not let a management container and another local Agent control the same saves, containers, or tmux sessions.
Remote Agents use typed operations, Placement, leases, and auditing; they do not accept arbitrary shell commands.

## Independent Docker worlds

`deploy/docker/compose.yaml` runs only one persistent `dst-admin` management container.
`dst-runtime-image` is a build placeholder, not a persistent service. Compose does not hard-code rooms or start Master/Caves when starting the management service.

```bash
deploy/scripts/build-all-in-one-image.sh --tag dst-admin/all-in-one:dev
docker compose -f deploy/docker/compose.yaml --profile build build dst-runtime-image
docker compose -f deploy/docker/compose.yaml up -d dst-admin
```

Open `http://HOST:8080`. If DST is absent, the overview offers game server installation as a normal job, so a failed SteamCMD download does not prevent the web service from starting.
For unattended deployments, set `DST_ADMIN_BOOTSTRAP_DST=true` to finish downloading before the API starts.

The first start of any world creates its container. Subsequent start, stop, console, and CPU-policy operations reuse it.
Containers are identified only by these four labels:

```text
com.dst-admin.managed=true
com.dst-admin.installation=default
com.dst-admin.cluster=<Cluster directory>
com.dst-admin.shard=<Shard directory>
```

The management service does not infer ownership from container names or control containers with missing/duplicate labels.
Each Runtime uses a private tmux socket. Console commands use fixed `docker exec ... tmux` arguments without exposing a host shell to the web UI.

## Data directories

The default host root is `/opt/dst`:

| Path | Contents | World-container access |
| --- | --- | --- |
| `/opt/dst/control` | Configuration, SQLite database, and management state | Not mounted |
| `/opt/dst/server` | One shared DST binary installation and `mods` | Read-only |
| `/opt/dst/saves` | Shared room configuration and each shard's saves | Read-write |
| `/opt/dst/workshop` | SteamCMD Workshop content | UGC root read-only |
| `/opt/dst/backups` | Save backups and system snapshots | Not mounted |
| `/opt/dst/maps` | Rendered map artifacts | Not mounted |

Master, Caves, and other worlds share one server installation. SteamCMD updates only `/opt/dst/server`; restarted worlds read the same version.
The management container writes mods into shared server/Workshop directories. World containers consume them through read-only mounts and `-ugc_directory`.
Each shard writes only its own save directory. The application still coordinates all worlds for room backups; separate container copies do not replace that process.

Klei distributes Linux dedicated-server binaries only for amd64, so both management and world-runtime images are built as `linux/amd64`.
Debian/Ubuntu x86_64 is recommended. macOS continues to default to its native Runtime.
On Apple Silicon, this container profile is for compatibility testing, not high-performance game hosting.

## Security and resource boundaries

- The Docker socket grants host root-level control. Use only trusted servers, and do not pass it to remote Agents or third-party containers.
- On startup, the management container reads the Docker socket's actual GID, then runs as non-root UID `10000` with that GID. No manual `DOCKER_GID` setting is required.
- World containers use UID/GID `10000:10000`, a read-only root filesystem, all capabilities dropped, and `no-new-privileges`.
- Ordinary startup skips save-write preflight and creates/starts the world container using current disk files. Permission, configuration, and mod problems appear in that shard's job and DST logs. Restart still checks writes before stopping an existing world, so an unusable target can be caught before shutdown.
- Server files and UGC are read-only; only saves are writable. tmux state lives in the container's temporary filesystem.
- Worlds use host networking. Player, Steam, and Master shard ports follow generated `cluster.ini`/`server.ini`. Port checks must pass before creation.
- Allow at most one active shard per physical core. For 2C4G, recommend only Master+Caves without default CPU pinning. Consider separate CPU policies on 4C8G or larger hosts as needed.
- Restarting the management container does not deliberately kill running world containers. It rediscovers them by managed labels after recovery. Normal world shutdown sends `c_shutdown(true)` first; timeout triggers an audited container-stop fallback.

## Remote Agents

Remote Linux/macOS machines still use standalone Agents because the Controller cannot directly access their files, tmux, or Docker Engine.
Enable the local + Controller role on this instance, then connect remote Agents to `ws://`/`wss://HOST/agent`.
Agent configuration registers only trusted installation paths on its own host.

No local Agent container is required. An external local Agent remains an advanced compatibility option only when explicitly separating management-service access from local Docker privileges; it is not the default installation path.

## Debian 12 native Agent

Prepare the remote environment and `../dst-agent-local/agent.conf` using the startup guide, then build the Agent:

```bash
CGO_ENABLED=0 go build -trimpath \
  -o dist/dst-admin-agent ./cmd/agent
sudo deploy/scripts/install-native-agent.sh \
  --binary "$PWD/dist/dst-admin-agent" \
  --config "$PWD/../dst-agent-local/agent.conf" \
  --user dst
```

Before installation, `dst` must be the account that actually runs DST and must have a working shell. tmux cannot start worlds for a `nologin` or `false` account.
After installation, edit `/var/lib/dst-admin-agent/agent.conf` and optional `/etc/dst-admin/agent.env`, check ownership of save/runtime directories, then start:

```bash
sudo systemctl enable --now dst-admin-agent
sudo systemctl status dst-admin-agent
journalctl -u dst-admin-agent -n 100 --no-pager
```

In native mode, Agent and DST must run as the same user or have explicitly configured tmux/file permissions.
Do not make the entire save directory world-writable to resolve permission errors.

Agent configuration lives in the state directory, owned by the service account with mode `0600`, so node IDs and rotated keys can be persisted securely.
Installers do not create, move, or copy DST data directories. `/opt/dst` is only an example; replace it with the machine's existing save, server, and Workshop paths.
systemd keeps `/usr`, `/boot`, and `/etc` read-only. Unix permissions and the Agent's locally registered trusted paths jointly constrain data access.
The runtime user must own cache/state and have the reads/writes required by enabled features for `server/mods` and target-shard `modoverrides.lua`.

Each native installation's `STEAMCMD_PATH` (legacy alias `STEAM_CMD_PATH`) must be absolute.
Before service startup, the installer validates it. If the configured stable entry point is absent, it discovers an executable through `PATH`, `/usr/games/steamcmd`, `/usr/bin/steamcmd`, `/opt/steamcmd/steamcmd.sh`, and `/opt/dst/steamcmd/steamcmd.sh`, then creates a symlink.
Installation fails immediately if SteamCMD cannot be found.
Package upgrades may move the real file, but do not casually change the registered stable entry point: game updates and Workshop downloads would see a different Runtime configuration.

Native Runtime derives a stable, private tmux socket from normalized `SAVE_PATH`.
The local Runtime and Agent use the same rule. Moving Agent state or changing an installation ID does not change the control channel for existing worlds.
Different save roots use different sockets; a single physical save root must not be split between two independent writers.

Since Agent `2.14.0`, an optional Runtime Peer HTTP Range service lets target Agents reuse exact, verified mods from a source node on a trusted network.
It is disabled by default. Configuration keys remain `MOD_PEER_LISTEN_ADDR` and `MOD_PEER_ADVERTISE_URL` for compatibility.
Docker Agents must explicitly publish the TCP port; firewalls should permit only trusted Controller/Agent networks.
Without it, synchronization still follows node Steam, Controller HTTP Range, and legacy-protocol fallback, without affecting the single-host path.
Explicit Publication and Placement Migration may use one-time `-skip_update_server_mods` for a controlled recovery startup after exact synchronization.
Ordinary room starts do not pass that flag or read logs back to confirm mod consistency.
Since Agent `2.16.0`, the same controlled port also transfers immutable save ZIPs from explicit Placement Migration.
Authorization binds the target Agent, migration ID, size, SHA256, and expiry. Unreachable targets or old Agents fall back to Controller relay.

Each native `SAVE_PATH` also holds a host-kernel exclusive lock on `.dst-admin/runtime/owner.lock`.
The lock releases automatically when the Controller/Agent process exits, without relying on a stale PID file.
tmux and DST do not hold it, allowing a restarted control service to reacquire ownership while retaining game processes.
If another local All-in-One or Agent targets the same saves, the second writer receives `RUNTIME_OWNER_CONFLICT` with the current holder, PID, and host.
One Agent configuration cannot register two installations with the same `SAVE_PATH`, or two native installations with the same host `CONSOLE_SOCKET`.

Every status read and pre-start check examines the managed socket, the current user's default tmux socket, and actual DST process arguments.
Default-socket conflicts return `LEGACY_TMUX_SOCKET_CONFLICT`; other unmanaged processes return `UNMANAGED_DST_PROCESS_CONFLICT`; duplicates return `DUPLICATE_DST_PROCESS_CONFLICT`.
All three block further startup. Stop old processes normally, then start through the UI to establish unique ownership.
The system does not automatically kill or take over processes of unknown origin.

systemd uses `KillMode=process`, so UI upgrades or Agent-process restarts do not terminate tmux/DST.
Retain the same service user and `SAVE_PATH` across restarts.
After a full host reboot, tmux and DST have exited; recover through ordinary room startup.
Game updates stop, update, and resume through the same Runtime without creating separate tmux sessions.

The first unreleased version using stable sockets requires one test-environment cutover: confirm zero players, issue `c_shutdown(true)` through the old channel, confirm neither the default nor old private socket has that world session, then restart through the UI.
After the initial public release, the socket derivation rule must remain stable. Later binary upgrades, configuration moves, and installation renames require no repeated cutover.

For local troubleshooting, use controlled attach in the same Agent binary. It is read-only by default and does not pause automatic commands:

```bash
sudo -u dst /var/lib/dst-admin-agent/bin/dst-admin-agent \
  -attach -state /var/lib/dst-admin-agent/runtime-state.json \
  -installation native -cluster Cluster_1 -shard Master
```

Writable attach requires an operator and a maintenance lease of 1 minute to 1 hour.
The dispatcher rejects automatic commands while leased. Process exit, disconnection, or timeout ends the lease:

```bash
sudo -u dst /var/lib/dst-admin-agent/bin/dst-admin-agent \
  -attach -write -owner "$USER" -lease 10m \
  -state /var/lib/dst-admin-agent/runtime-state.json \
  -installation native -cluster Cluster_1 -shard Master
```

A writable tmux client opened outside the Agent CLI marks ConsoleHealth as `external_writer` and blocks subsequent automatic input until a new Runtime instance is established.
The web UI exposes neither a host shell nor an attach entry point.

Re-running the installer copies the binary, configuration, and service in place.
For upgrades, save a separate copy of the active configuration and use it as `--config` so a template cannot overwrite the Agent ID/key.
Retain `runtime-state.json` and restart after installation.
Uninstall preserves configuration, Agent ID, highest fencing token, and idempotency state by default, keeping operation history across reinstalls.
Purge state only after confirming the node is no longer managed by the control plane:

```bash
sudo deploy/scripts/uninstall-native-agent.sh
sudo deploy/scripts/uninstall-native-agent.sh --purge-state
```

## macOS launchd Agent

Install the Agent as a LaunchAgent for the macOS user running DST and tmux, not as a root LaunchDaemon.
Copy `deploy/systemd/agent.conf.example` outside the repository to `../dst-agent-local/agent.conf`, then fill in the upstream URL, connection key, and local absolute paths.
Put `MOD_CACHE_PATH` and `MOD_STATE_PATH` under the current user's `~/Library/Application Support/DST Admin Agent`, expanded to real absolute paths in configuration.
Check game, save, and Workshop paths against the [macOS startup guide](startup-guide.en.md#macos-local-deployment).
SteamCMD may stay empty if mod downloading is not needed; otherwise set its actual entry point. Build and install:

```bash
go build -trimpath \
  -o dist/dst-admin-agent ./cmd/agent
deploy/scripts/install-macos-agent.sh \
  --binary "$PWD/dist/dst-admin-agent" \
  --config "$PWD/../dst-agent-local/agent.conf"
launchctl print "gui/$(id -u)/top.luocaiyi.dst-admin-agent"
```

Configuration, keys, and idempotency state live in `~/Library/Application Support/DST Admin Agent`; logs are in `~/Library/Logs/DST Admin Agent`.
Uninstall preserves state by default here too.

The installer immediately restarts the LaunchAgent. For upgrades, use a separate copy of active configuration, not the initial template, and retain the state directory.
Native Agent builds use the version in source; do not override it with a fixed version from old documentation.

macOS attach needs no `sudo`. Point `-state` to `~/Library/Application Support/DST Admin Agent/runtime-state.json`.
Because macOS has a tighter Unix-socket path-length limit, native Runtime uses a short path under `/tmp/dst-admin-runtime-<uid>` based on the normalized `SAVE_PATH` hash. Directory permissions are fixed at `0700`.

```bash
deploy/scripts/uninstall-macos-agent.sh
deploy/scripts/uninstall-macos-agent.sh --purge-state
```

## Verification and troubleshooting

```bash
deploy/scripts/smoke-deployment.sh
DST_ADMIN_SMOKE_BUILD=1 deploy/scripts/smoke-deployment.sh
```

Checks cover container-label filtering, first-creation arguments, fixed tmux sockets, typed remote Agent operations, and Compose parsing.
If a container is running but console health is starting, inspect it using the commands below.

Since Agent `2.13.0`, native Runtime derives a stable tmux socket from `SAVE_PATH` and returns lifecycle results verified against process ownership through `shard.control.v2`.
Agent `2.13.1` also handles default tmux sockets left in an old mount namespace after systemd `PrivateTmp` restarts.
Worlds started by an older Agent and still running during upgrade are recognized as an old channel.
After upgrading the Agent through the UI, issue one stop action. A shutdown command is sent only if the session name, unique DST process, and `SAVE_PATH/room/world` all match.
The next start automatically uses the stable channel; no manual socket deletion or PID termination is needed.
Old Agents can still report read-only status, but the Controller rejects their start, stop, restart, and save requests to avoid presenting unreliable acknowledgements as success.

```bash
runtime_container=replace-with-world-container-id
docker exec "$runtime_container" tmux -S /run/dst-admin/tmux/tmux.sock has-session -t =dst
docker logs "$runtime_container"
```

Stop sends `c_shutdown(true)` through the console so DST saves and exits itself.
Do not use generic `docker kill` for normal shutdown. Forced termination must show the unsaved-data risk and enter abnormal-exit auditing.

The local container Runtime waits for actual container exit. On timeout, it uses Engine `stop`, then `kill --signal KILL`.
Exit observation distinguishes normal exit, nonzero exit, SIGKILL, dead, and `CONTAINER_OOM_KILLED`.
Once fallback is used, eventual termination is not presented as confirmed saving.

## Backup boundaries

Room backups contain shared Cluster files and each shard's private saves in `dst-saves`, coordinated through save/shutdown consistency first.
Separate Docker-volume copies must not be represented as complete room backups.

`/opt/dst/control` belongs to platform disaster recovery and needs protection separate from room backups.
Remote Agent runtime state determines fencing and idempotency and must survive node recovery.
Local containers are rediscovered from the database, managed labels, and fixed installation ID.
Mod cache, Workshop content, and DST binaries can generally be rebuilt, but retaining cache supports exact-version rollback and recovery while Steam is unavailable.

Kubernetes PVC/CSI snapshots replace only single-volume copying, not cross-shard save barriers, manifests, or centralized verification.
The current implementation offers disabled-by-default Provider status, read-only REST observation, typed preflight API/UI, namespace RBAC, and an experimental safety core.
However, `applyAllowed=false` is fixed, with no Apply route, lease-aware supervisor, Console, mod distribution, backup, or restore workflow. It is not a production installation option.

## Historical Debian 12 host evidence

On 2026-08-15, the following remote Agent flows ran on a fresh Debian 12 / Docker environment with a test workspace isolated from existing DST installations.
This evidence covers the remote protocol and cross-node data flows; it does not replace acceptance of the current management-container-to-local-shard model:

- Non-root control-plane first startup, migration of old `control-data` permissions, control-plane health checks, and reconnection of two Agents.
- Simultaneous native/container Agent registration, correct Runtime inventory, physical-core capacity, and managed-container label identification.
- Successful cross-node/cross-volume shard apply: target Placement became `aligned`, and the source retained a recoverable migration directory.
- SteamCMD downloaded Workshop `1392778117`: about 110 MB and 1433 files, with tree SHA verification.
- A distributed protection backup preceded publication. The mod-cache bundle uploaded in 256 KiB chunks; cross-node tree checks and local manifest checks on both ends preceded atomic publication and target `modoverrides.lua` readback.
- On 2026-08-16, further tests covered a migrated shard whose directory no longer existed on the Controller. Room mod lists, configuration files, and `modinfo.lua` schemas aggregated from current Placement without falling back to local Controller files.
- Workshop `1392778117` was disabled, enabled, configured with `AutoStackedLoot=true`, then removed. All four jobs and Publications were `succeeded/full`, with target-file readback after each step. After removal, `modoverrides.lua` was `return {}`, the managed setup section contained no `ServerModSetup`, and immutable node cache remained available for reuse and rollback.

Final job IDs: `c23bcdb2-09fc-43ba-bbb8-04e6eba38f9d`, `a908e856-8df7-4ed9-90cb-87d5e926b87d`, `f8233414-f253-4e6c-9bb5-bba4a2a4aca8`, `0bce17e5-0801-4755-a7cb-6f3fc1a094c0`.
Publication IDs: `3e7dd0ad-a2b4-4948-b738-0dd311057050`, `321d6b7b-3e2e-42ba-bfc3-ddc85c138245`, `7ddc78bc-1299-44cf-9da5-0d63bd6ffc84`, `2dfe6b50-3baf-42e0-80e1-7d72d41703a2`.

This evidence covers a Debian 12 Docker/native combination. It does not establish production compatibility with Podman, macOS containers, or Kubernetes.
