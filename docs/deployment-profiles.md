# DST Admin deployment profiles

DST Admin uses one codebase and one product model. Deployment packaging and
management roles are independent choices; neither introduces separate APIs,
room models, or feature forks.

## Deployment model

Packaging describes how the same application is delivered:

- `native`: the application runs directly on Linux or macOS.
- `all_in_one`: the application, Vue UI, SteamCMD, tmux, and DST run in one OCI
  container without a Docker socket.
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
Both native and All-in-One installations can use `standalone`,
`controller_worker`, or `managed_worker`. Agent is therefore a transport
capability, not a deployment profile.

## Install native local

Build the Go binaries and Vue application for the target host, then install
them with the host-specific installer. The supplied configuration must use
paths owned by the same OS account that runs DST.

Linux requires a dedicated user plus tmux, Lua, SteamCMD, and the SteamCMD
i386 runtime dependencies. After reviewing `deploy/systemd/local.conf.example`:

```sh
sudo deploy/scripts/install-native-local.sh \
  --binary ./dst-admin \
  --renderer ./dst-map-renderer \
  --config ./local.conf \
  --web-root ../dst-admin-vue-v3/dist \
  --user dst
sudo systemctl enable --now dst-admin-local
```

The service listens on port `8000`. The installer does not silently move an
existing save directory; import or configure the intended `/srv/dst` data
before starting production Rooms.

On macOS, run the installer as the signed-in desktop user. Its configuration
should continue to point at that user's Steam, Workshop, and
`DoNotStarveTogether` paths:

```sh
deploy/scripts/install-macos-local.sh \
  --binary ./dst-admin \
  --renderer ./dst-map-renderer \
  --config ./app.conf \
  --web-root ../dst-admin-vue-v3/dist
```

The generated LaunchAgent listens only on `127.0.0.1:8000`. New native
installations start as `standalone`; the Gateway or Member connection is only
activated after the operator changes the management role and restarts the
service.

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

Recommended paths on Linux:

```text
/var/lib/dst-admin/       database and management state
/srv/dst/server/          DST dedicated server
/srv/dst/saves/           Cluster and Shard data
/srv/dst/workshop/        SteamCMD Workshop download root
/srv/dst/backups/         backups
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

All persistent state lives below `/data`, bind-mounted from
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
  other nodes, but they do not make the local `/data` volume distributed.

Build and run it with:

```sh
deploy/scripts/build-all-in-one-image.sh --tag dst-admin/all-in-one:dev
DST_ADMIN_IMAGE=dst-admin/all-in-one:dev \
  docker compose -f deploy/docker/compose.all-in-one.yaml up -d
```

In regions where the official Debian mirror is slow, the build script accepts
`--debian-mirror` and `--debian-security-mirror`, or the equivalent
`DST_ADMIN_DEBIAN_MIRROR` and `DST_ADMIN_DEBIAN_SECURITY_MIRROR` environment
variables. The first start downloads the DST dedicated server into the named
`/data` volume. `DST_ADMIN_BOOTSTRAP_DST=false` is only for smoke tests or a
volume that already contains `/data/server`.

Open `http://HOST:8080` after the health check passes. Allocate at least one
physical CPU core per concurrently running Shard; placing multiple active
worlds on one core is supported by the OS scheduler but is expected to cause
game stalls.

For the common 2 vCPU / 4 GiB host, keep the default All-in-One packaging and
run only Master plus Caves. Do not pin either Shard by default: shared and
oversubscribed VPS CPU topology is often not trustworthy, and affinity can
reduce the scheduler's ability to use short idle periods. The capacity preview
does not reserve a complete CPU on a two-CPU host, but it warns when projected
free memory drops below 768 MiB and requires confirmation below 384 MiB.

Running each Shard in its own container is an advanced isolation profile, not
the default installation. It can enforce a separate cpuset, CPU quota, memory
limit, and OOM boundary for every Shard, and it allows replacing one Shard
container without replacing the others. It also adds images, volumes,
networking, port mapping, health reconciliation, and backup coordination that
make first-time operation harder. Prefer it only on 4+ CPU hosts when explicit
resource isolation is more valuable than the single-container experience.

## Fleet roles

Enabling Controller starts the Gateway at the same origin's `/agent` WebSocket
endpoint. Enabling Member starts an embedded Agent that connects outbound to
one `ws://` or `wss://` Controller URL and advertises the local trusted
RuntimeInstallation. Its identity and in-flight operation state are persisted
below `fleet.STATE_PATH`.

Fleet keeps the existing typed Agent protocol, RuntimeInstallation registry,
Placement, lease, fencing, idempotency, and persistent recovery. Members never
accept arbitrary shell commands or controller-selected paths. The first
release retains the existing shared Gateway key. Per-node enrollment codes and
independent credentials remain a future hardening item and must not be claimed
as implemented.

Role changes are written through System Settings and require a process restart.
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
