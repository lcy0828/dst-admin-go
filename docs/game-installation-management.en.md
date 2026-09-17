# Game server management

[简体中文（默认）](game-installation-management.md) | **English**

Open **Game server management** (`/servers/releases`). The game installation section shows installation status, game version, game/save directories, and SteamCMD status for the local Runtime and each Agent.
Use the scope selector to view all machines or one machine. Multiple registered installations on a host appear separately, including installations with no rooms.
Offline nodes and failed inspections are not reported as an uninstalled game.

## Install and update

1. Configure SteamCMD on the target machine and register its game and save directories.
2. Confirm that every world using that installation has stopped.
3. Click **Install game server** or **Update / validate** for the intended machine and installation, then wait for the job to finish.
4. After first installation, create a room, import saves, or continue with LuaJIT2 installation below.

For native local deployments, create the configured directories before starting the management service. Empty directories are sufficient; downloading the game first is unnecessary.
All-in-One prepares persistent directories automatically.

The selected Agent performs its own downloads and validation. Local operations run through the in-process Runtime.
The Controller dispatches jobs and displays results; it does not relay Steam game files. No room needs to exist first.
After the job, the installer checks the game executable and version. Incomplete files still cause failure even if SteamCMD exits successfully.

Installation requires Linux x64, a Native Runtime (including regular All-in-One), working SteamCMD, and at least 6 GiB free space.
It does not install OS dependencies or SteamCMD. Remote hosts need a recent Agent with `runtime.game-install.v1`.
Agents without registered installation paths show configuration guidance. Register them in Agent configuration first; the UI does not create arbitrary trusted directories.
The independent-shard container driver is not supported yet.

**Update / validate** here requires manually stopping worlds and is intended for first installation or explicit repair.
Use the existing game update workflow when you need automatic backup, shutdown, and resumption of running rooms.
SteamCMD validation can overwrite the LuaJIT launcher. Reinstall/repair LuaJIT below if needed; ordinary game startup does not repair it automatically.

## Use an existing server on the machine

If a registered installation location is empty or absent, click **Use an existing server** and enter an absolute path on the **selected machine**.
The UI checks the executable and version before asking you to confirm. Changing the path requires another inspection.
The job also rejects confirmation if the inspected executable or version has changed.

For container deployments, the path must be visible inside the container. All-in-One mounts host `DST_ADMIN_DATA_ROOT` at container `/opt/dst` by default; enter `/opt/dst/...` in the UI.
Explicitly mount other host directories first. Keep a separately mounted source directory mounted after container recreation so the connection remains valid.

Adoption creates a directory link at the registered game location pointing to the existing game directory.
Game files remain in their original location without copying, moving, or overwriting. The connection survives Agent restarts.
The UI shows both the registered location and actual game directory. Subsequent updates and LuaJIT installation affect that actual game copy, so stop every world using it before adoption.
Only POSIX directory links are supported. Steam client installations on macOS remain managed by Steam.

Selecting a game directory **does not import saves**. The system continues using the existing save directory shown in the UI.
Import existing saves separately through room management. Adoption does not overwrite a populated game location and rejects source directories overlapping saves or the registered location.
The installer does not delete saves or automatically stop/start worlds.

## API and workload

All endpoints are under `/api/v2`. Mutations return background jobs:

| Endpoint | Purpose |
| --- | --- |
| `GET /runtime-targets/game-installations?targetId=…` | List registered installations within the selected scope |
| `POST /runtime-targets/game-installations/probe` | Inspect an existing directory |
| `POST /runtime-targets/game-installations/actions/install` | Install or validate the registered location |
| `POST /runtime-targets/game-installations/actions/adopt` | Confirm adoption of an existing directory |

Operations require exact `targetId` and `installationId` values. Inspection also requires `path`; adoption additionally requires the returned `fingerprint`.
Installation does not accept arbitrary destination paths. Remote failures do not fall back to the local host.
Leases, Agent operation journals, and installation directory locks coordinate installation; retrying the same operation does not execute it twice.

Installation status is read only when opening the page, changing scope, or refreshing manually.
The service does not scan disks for games, check Steam's latest release, or poll while idle. Only unfinished jobs have progress polled every two seconds.
