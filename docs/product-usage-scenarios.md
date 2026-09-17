# DST Admin product usage scenarios

This document is the product baseline for architecture, performance, API, and
UI decisions. New work must preserve the primary scenario before optimizing
less common distributed deployments.

## Scenario priority

### S1: one 2 vCPU / 4 GiB All-in-One host

This is the dominant deployment.

- One All-in-One instance contains the UI, API, SteamCMD, tmux, and DST.
- One managed Room normally contains Master and Caves.
- Master and Caves launch concurrently; neither waits for the other to report
  ready before its process is created.
- Both Shards run on the same host without default CPU pinning.
- Starting Master plus Caves on a healthy 2C4G host must not require a CPU risk
  confirmation. Low memory may still warn, and critically low memory may
  require confirmation.
- The local Runtime uses an in-process adapter. It must not start a second
  loopback Agent or require WebSocket connectivity to itself.

### S2: multiple All-in-One hosts

Some users install All-in-One independently on several machines. When they
enable centralized management, one instance is the Controller and the other
instances expose their local Runtime through the embedded Fleet Member.

- Every machine keeps one straightforward All-in-One installation.
- A managed worker must not have two independent writers for the same Runtime.
- Machine switching must preserve the selected node, while Controller-only
  settings remain clearly identified as Controller settings.

### S3: one All-in-One Controller with remote Agents

One All-in-One instance stores control-plane state and also runs local Shards.
Additional machines run only the Agent and their registered DST Runtime.

- Local and Agent targets share product contracts and UI behavior.
- Remote authentication, heartbeat, reconnect, report freshness, and trusted
  path resolution remain Agent-only transport concerns.
- Adding an Agent must not increase the steady-state work performed by the
  local Runtime when no Room is placed on that Agent.

### S4: one Room split across machines

A less common but supported deployment places Master and Caves, or additional
worlds, on different Runtime targets.

- Placement is recorded per world as `targetId + installationId`.
- Room start, stop, save, backup, configuration, Mods, and game update must use
  the applied Placement for every world and must never fall back to `local`.
- Cross-machine Master connectivity, ports, partial failure, and stale Agent
  observations must be visible before an operation is treated as complete.

## Decision rules

1. Optimize S1 first. Features must remain usable on 2C4G without an extra
   local Agent process, heavy message broker, or aggressive polling.
2. Distributed behavior extends the same Room and Runtime model; it must not
   create a separate remote-only product workflow.
3. Product contracts, errors, operation results, and resource presentation are
   shared. In-process and WebSocket transports are intentionally different.
4. Capacity is advisory. Up to two effective CPUs do not reserve a complete
   CPU; hosts with three or more effective CPUs reserve one CPU for the OS and
   maintenance work. Memory risk is evaluated separately.
5. Comparable storage metrics use the managed DST data volume. Controller
   process, Go runtime, and database diagnostics remain Controller-only.
6. New background work must be demand-driven, coalesced, bounded, and disabled
   when its feature or target is not active.
7. Mod convergence is explicit and event-driven. Ordinary Room start uses the
   files currently present on the Runtime and must not inspect, publish, repair,
   or overwrite Mod, Room configuration, `customcommands.lua`, or managed Lua
   Runtime assets. A missing or `none` CPU policy also adds no CPU
   prepare/apply phase.
   User-triggered Mod actions and world migration retain their own validation
   and transactional repair.
8. Runtime configuration files are the source of truth. Room, world, and Mod
   pages read the latest files from the applied Runtime Placement. An explicit
   save changes only fields owned by that editor and preserves unknown values;
   if an operator edits a file after the page was loaded, the stale revision
   must fail with a visible conflict instead of overwriting the disk change.
   INI unknown keys remain on each target during cross-machine publication.
   Lua unknown values are preserved semantically, but comments and original
   formatting are not guaranteed when a Lua table is actually modified.
9. Ordinary Runtime target lookups resolve only the selected Room. They do not
   synchronize the global Room catalog or infrastructure database. Explicit
   topology planning, discovery, and migration retain their fleet-wide checks.
10. A small room/world configuration save to one installation uses
    `runtime.configuration.apply.v1`: one Runtime call performs the existing
    node-local publication, revision check, verification, and cleanup. The
    payload is limited to 256 KiB. Multi-target saves and older Agents retain
    the staged protocol and cross-machine rollback; Mod toggles/configuration
    keep their existing direct per-world writes. No save restarts DST.

## Acceptance matrix

| Change area | Required regression |
| --- | --- |
| Room lifecycle | S1 Master+Caves and S4 split placement |
| Resource/capacity | 2C4G local, stale/offline Agent, and multi-node totals |
| Configuration/Mods/backup | all-local Room and split Room |
| Game update | local installation, Agent installation, and mixed fleet plan |
| UI management scope | one node, selected node, all nodes, Controller-only page |
| Deployment | All-in-One standalone, All-in-One Controller, managed worker, Agent |

## Automated regression map

| Scenario | Required behavior | Test packages |
| --- | --- | --- |
| S1 2C4G All-in-One | Master + Caves starts without CPU overcommit confirmation; local writes carry fencing; evidence survives Controller restart | `internal/topology`, `internal/runtimedriver` |
| S2 multiple All-in-One | Controller and managed-worker roles are exclusive; one Runtime has one control-plane writer | `internal/deploymentprofile`, `routers`, `internal/fleetmember` |
| S3 Controller + Agent | Disconnect never falls back locally; reconnect wakes inventory collection; downgraded capabilities replace the old snapshot | `internal/agents`, `internal/shards`, `internal/runtimedriver` |
| S4 split Room | Master and Caves can use different machines; ordinary starts preserve current Runtime files; migration synchronizes Mods before restoring the target world; partial publication rolls back | `internal/roomprovision`, `internal/placementmigration`, `internal/modcontrol`, `internal/modpublication`, `internal/distributedbackup`, `internal/gameupdate` |
| Runtime reads | Selected-room resolution avoids global sync; missing first telemetry sample differs from read failure; previous-boot files are rejected | `internal/topology`, `internal/runtimefiles`, `internal/dstruntime` |
| Small configuration saves | Node-side revision conflicts preserve manual edits; single-target calls remain fenced/idempotent; older Agents retain staged publication | `internal/configpublication`, `internal/configuration`, `internal/runtimedriver`, `agent` |

`deploy/scripts/smoke-deployment.sh` runs these contract packages for both the
Docker and native deployment entry points.
