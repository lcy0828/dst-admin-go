# DST Admin deployment profiles

[简体中文（默认）](deployment-profiles.md) | **English**

DST Admin uses one codebase and one product model. Deployment packaging and
management roles are independent choices; neither introduces separate APIs,
room models, or feature forks.

For step-by-step first installation, dependencies, native builds, and Agent
enrollment, use the [installation and startup guide](startup-guide.en.md). This
document explains packaging, roles, and support boundaries.

## Deployment model

Packaging describes how the same application is delivered:

- `native`: the application runs directly on Linux or macOS.
- `all_in_one`: the application, Vue UI, SteamCMD, tmux, and DST run in one OCI
  container without a Docker socket.
- `container`: one management container directly controls one container per
  local Shard. It uses no local Agent container and all Shards share one
  host-mounted DST installation.
- `control_plane`: the application is installed without a local DST executor.

The management role describes which machines this instance controls:

- `standalone`: manages only the local DST installation.
- `controller_worker`: manages the local installation and accepts outbound
  WebSocket connections from additional Fleet Members.
- `managed_worker`: manages its local installation on behalf of one upstream
  Controller. Its local UI remains readable, while mutations are performed at
  the Controller to prevent two writers.
- `controller_only`: manages Fleet Members and has no local DST executor.

An instance cannot be both an upstream Controller and a downstream Member.
Native, All-in-One, and container-Shard installations can use `standalone`,
`controller_worker`, or `managed_worker`. Agent is therefore a transport
capability, not a deployment profile.

## Local and Agent equivalence boundary

`local` and `agent:<id>` are the same product-level Runtime target. Room
placement, lifecycle, console commands, logs, player/world observation,
backups, Mods, game updates, maps, network probes, CPU policy, and node resource
presentation must resolve through a target-aware contract and must not silently
fall back to the Controller host.

They are not the same transport. `local` uses an in-process adapter and can
read trusted Controller-host state directly. A remote Agent owns its
authentication, heartbeat, reconnect, installation registry, path validation,
operation journal, and observed/received timestamps. Controller process, Go
runtime, and database diagnostics remain Controller-only and are exposed by
`/system/status`; comparable host CPU, memory, disk, load, uptime, freshness,
and availability are exposed for every node by `/system/resources`.

The goal is identical user-facing contracts and failure semantics, not a
loopback WebSocket from the Controller to itself. Any new Runtime feature must
enter through the shared Driver or node-resource boundary; a local-only direct
file path is acceptable only for Controller diagnostics and must not appear as
a remote-capable operation.

## Install native local

Follow the [Linux/macOS startup guide](startup-guide.en.md) for dependencies,
service users, empty data directories, a native package with embedded UI, and
configuration. The Linux installer starts from `local.conf.example`, but an
upgrade must use a separate copy of the current installed configuration.

Linux uses `dst-admin-local.service`, listens on `0.0.0.0:8000`, and explicitly
loads `/var/lib/dst-admin/app.conf`. The macOS LaunchAgent runs as the signed-in
user, listens on `127.0.0.1:8000`, and loads its configuration from
`~/Library/Application Support/DST Admin`. Both installers serve built Vue
assets through the same Go service and use the local in-process Runtime.

After configuring SteamCMD and registering game/save paths, use **Game server
management** to install before creating a Room or connect an existing game
installation. The configured save directory stays active; importing saves is
a separate Room action. See [game installation management](game-installation-management.en.md).

## Shared ownership model

```text
Vue UI -> HTTP/SSE -> Control plane -> Runtime Driver -> tmux -> DST Shard
                                  -> SteamCMD / files / backups
```

The control plane remains the source of desired Room, Shard, configuration,
Job, and audit state. The Runtime Driver supplies observed process and file
state. Every response that represents live state must retain `observedAt`,
freshness, source, and runtime state instead of silently substituting cached
data.

Set `TZ` once for the deployment, defaulting to `Asia/Shanghai`. The control
plane, Agent, and every DST Runtime must use the same timezone because DST's
`Current time` log marker does not include an offset. API and database
timestamps remain RFC 3339/UTC values and are converted only for display.

## Native

The management service and every managed DST process run as the same dedicated
OS user. The local `NativeDriver` talks directly to the trusted save and server
roots and uses its private tmux dispatcher. In `standalone` mode no Agent key,
WebSocket hop, Docker socket, or Placement migration is involved. The same
binary can later become a Controller or a Member without changing its local
execution model.

Example paths on Linux (existing installations may remain where they are):

```text
/var/lib/dst-admin/       database and management state
/opt/dst/server/          DST dedicated server
/opt/dst/saves/           Cluster and Shard data
/opt/dst/workshop/        SteamCMD Workshop download root
/opt/dst/backups/         backups
```

On macOS, the service runs as the signed-in user so it can access the user's
Steam and `DoNotStarveTogether` directories. UGC content must use the writable
Steam Workshop path; the application bundle must not be treated as a writable
Mod staging area.

## All in one

The All-in-One image contains the Go API, built Vue assets, SteamCMD, Lua, tmux,
and the DST runtime. Go serves both `/api` and the SPA, and directly uses the
local `NativeDriver`. It does not put an Agent and DST into separate containers.
Master, Caves, and additional Shards are separate
processes inside the same container and remain individually controllable.
The current Klei Linux dedicated server distribution is packaged as
`linux/amd64`; Apple Silicon hosts run this profile through Docker's amd64
emulation rather than producing an unusable ARM image.

All persistent state lives below `/opt/dst`, bind-mounted from
`${DST_ADMIN_DATA_ROOT:-/opt/dst}` on the host. Container replacement must not lose
the database, saves, Workshop content, Mod releases, backups, or server files.
The container has no Docker socket and does not need privileged mode. On
container shutdown, the entrypoint sends a save-and-shutdown command to every
managed DST pane before stopping the API.

This profile optimizes installation and local operation. Its explicit limits
are:

- Updating or replacing the container restarts all Shards in that container.
- Container-level CPU limits apply to the whole installation. The first
  All-in-One release does not advertise per-Shard CPU enforcement because the
  current Linux executor requires delegated writable cgroup v2 controllers.
  A future container-specific taskset/cpuset driver must be verified before
  that option is exposed.
- Its local executor is one failure and storage domain. Fleet roles can attach
  other nodes, but they do not make the local `/opt/dst` volume distributed.

Use the [README Docker instructions](../README.en.md#docker-quick-start) to build
and start the image. They keep the image tag, host data root, and exposed ports
in `deploy/docker/.env`; use the same explicit `--env-file` on every Compose
operation, including upgrades and restarts.

In regions where the official Debian mirror is slow, the build script accepts
`--debian-mirror` and `--debian-security-mirror`, or the equivalent
`DST_ADMIN_DEBIAN_MIRROR` and `DST_ADMIN_DEBIAN_SECURITY_MIRROR` environment
variables. The management UI starts before DST is installed. **Game server
management** can install the game with a background Job before any Room exists.
Agent installations download on that Agent. Set
`DST_ADMIN_BOOTSTRAP_DST=true` only when an unattended deployment must finish
the SteamCMD download before the API starts.

Open `http://HOST:8080` after the health check passes. Capacity estimates count
all running Shards on each host. Up to two effective CPUs have no whole-core
reservation; three or more reserve one CPU for the OS and maintenance. Memory
risk is evaluated independently. See [product scenarios](product-usage-scenarios.md).

For the common 2 vCPU / 4 GiB host, keep the default All-in-One packaging and
run only Master plus Caves. Do not pin either Shard by default: shared and
oversubscribed VPS CPU topology is often not trustworthy, and affinity can
reduce the scheduler's ability to use short idle periods. The capacity preview
does not reserve a complete CPU on a two-CPU host, but it warns when projected
free memory drops below 768 MiB and requires confirmation below 384 MiB.

## Container Shards

The `container` profile keeps the UI, API, SteamCMD, files, and scheduling in
one management container. It does not run a second local Agent. The management
container uses the Docker socket only to discover and control containers with
the fixed `com.dst-admin.*` labels.

No Room is hard-coded in Compose. The first Start action for a Shard creates its
container from the configured trusted image; later actions start or stop the
same container. Every Shard receives the shared server and Workshop roots
read-only and its shared save root read-write. Runtime-specific tmux state stays
inside that Shard container. This permits separate CPU policy and failure
isolation without duplicating the DST binary.

Klei distributes the Linux dedicated server for amd64 only, so both images in
this profile are built for `linux/amd64`. Debian or Ubuntu x86_64 is the
recommended host. Apple Silicon can emulate the profile for compatibility
testing, but should use the native macOS Runtime for actual gameplay.

Build and start the local container profile with:

```sh
deploy/scripts/build-all-in-one-image.sh --tag dst-admin/all-in-one:dev
docker compose -f deploy/docker/compose.yaml --profile build build dst-runtime-image
docker compose -f deploy/docker/compose.yaml up -d dst-admin
```

The default host data root is `/opt/dst`. Open `http://HOST:8080`, install the
game server from the dashboard, create a Room, then start Master and Caves. The
Docker socket grants host-level control, so this profile is only appropriate on
a trusted single-user server. Remote machines still connect through the Agent
protocol; they never receive this host's Docker socket.

For a 2 vCPU / 4 GiB server, All-in-One remains the simplest default. Use
container Shards when independent world restart or CPU isolation is worth the
extra image and Docker-socket boundary. In both profiles, run no more active
Shards than the host can support; Master plus Caves is the practical default.

## Fleet roles

Enabling Controller starts the Gateway at the same origin's `/agent` WebSocket
endpoint. Enabling Member starts an embedded Agent that connects outbound to
one `ws://` or `wss://` Controller URL and advertises the local trusted
RuntimeInstallation. Its identity and in-flight operation state are persisted
below `fleet.STATE_PATH`.

Fleet keeps the existing typed Agent protocol, RuntimeInstallation registry,
Placement, lease, fencing, idempotency, and persistent recovery. Members never
accept arbitrary shell commands. Execution destinations come from registered
installations; explicit game inspection/adoption can validate a source path
on the selected node. The first
release retains the existing shared Gateway key. Per-node enrollment codes and
independent credentials remain a future hardening item and must not be claimed
as implemented.

Role changes are saved from the deployment role panel in Machine Management
and apply without a process restart once local worlds and background tasks are stopped.
The deployment packaging is read-only at runtime because changing it requires
reinstalling or replacing the deployment artifact.

The embedded roles use separate environment variables from the compatible
external Agent process:

| Purpose | Configuration | Environment |
| --- | --- | --- |
| Enable local execution | `fleet.LOCAL_EXECUTOR_ENABLED` | `DST_ADMIN_LOCAL_EXECUTOR_ENABLED` |
| Enable the Controller Gateway | `fleet.CONTROLLER_ENABLED` | `DST_ADMIN_FLEET_CONTROLLER_ENABLED` |
| Enable the embedded Member | `fleet.MEMBER_ENABLED` | `DST_ADMIN_FLEET_MEMBER_ENABLED` |
| Upstream Controller WebSocket | `agent.SERVER_URL` | `DST_ADMIN_FLEET_CONTROLLER_URL` |
| Embedded Member credential | `agent.SECURITY_KEY` | `DST_ADMIN_FLEET_MEMBER_KEY` |
| Stable embedded Member ID | `fleet.NODE_ID` | `DST_ADMIN_FLEET_NODE_ID` |

The Controller Gateway credential remains `server.SECURITY_KEY` or
`DST_ADMIN_AGENT_SECURITY_KEY`. It is intentionally not reused as the local
Member profile. `DST_ADMIN_AGENT_SERVER_URL` and
`DST_ADMIN_AGENT_SECURITY_KEY` remain inputs for the separate Agent binary and
are not fallback variables for the embedded Member. A deployment may provide
the same secret value to both ends of a connection, but it must do so through
the role-specific variable on each process; secrets never belong in the
WebSocket URL.

Fleet is considered ready only after the same operation succeeds through the
complete UI -> Job -> Agent -> Runtime -> observation -> UI path. Unit tests or
an Agent capability advertisement alone are not completion evidence.

## Release gates

Native local must pass first:

- create/import a Master+Caves room;
- start, stop, restart, save, console, logs, players, and world state;
- Mod download, configuration, update, and restart-required state;
- backup, restore, game update, and disk-space protection;
- macOS and Debian 12 smoke tests.

All-in-One then repeats the same matrix without a separate Agent container or
Docker socket. Only after both profiles pass should Fleet-specific development add remote
Placement, transfer, and distributed consistency cases.
