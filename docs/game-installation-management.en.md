# Game server management

[简体中文（默认）](game-installation-management.md) | **English**

Open **Game server management** (`/servers/releases`). The game installation section shows installation status, game version, game/save directories, and SteamCMD status for the local Runtime and each Agent.
Use the scope selector to view all machines or one machine. Multiple registered installations on a host appear separately, including installations with no rooms.
Offline nodes and failed inspections are not reported as an uninstalled game.

## Install and update

1. Configure SteamCMD on the target machine (executable, installation directory or command in PATH) and register its game and save directories.
2. Click **Install game server** or **Update / validate** for the intended installation and wait for read-only preflight.
3. Review affected worlds, choose whether to resume them afterwards, and confirm. Running worlds are labeled **Will stop**.
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

**Update / validate** uses the game release workflow: create and verify protection backups → stop affected worlds → run SteamCMD update and validation → optionally resume previously running worlds. Originally stopped worlds stay stopped. Shared installations and connected rooms across nodes are included in the preview; unrelated installations on the same host are excluded. A current build is still validated. Validation may download Steam's current build; it is not read-only.

Confirmation stays disabled while checking, when nodes are offline or state is unknown, or when unmanaged game processes use the installation. Review the reason and use **Check again**. The plan is rechecked before execution; scope or running-state changes require confirmation again. New installations and idle installations without managed worlds use the direct installer. Follow task progress or **View update history** after confirmation. Failed updates do not blindly restart worlds.

Explicit updates preserve recognized existing Linux x64 LuaJIT2 launchers, restore them against the new game executable, and verify availability. They do not download or upgrade the LuaJIT package. Failed maintenance can be retried; ordinary startup never repairs files.

If a game update fails and remains retryable, its pre-update protection backups cannot be deleted. A retry checks the original backup files and their integrity. Missing or damaged files block the update and require recovery; the system does not replace the original protection with data from after the update.

## Optional Steam account login

Choose **Steam login** on the selected node. Anonymous is the default. Select
**Steam account**, enter the login name and password, and choose **Save and verify login**.
If Steam Guard requests verification, enter the current email or authenticator code,
or approve the login in the Steam mobile app as prompted.
Once verified, install, update or retry the download.

Game installation, manual and unattended game updates, and mod downloads share
the node's login settings. Installations using the same SteamCMD executable share
one account. Agents perform their own login and downloads; the Controller does not
store remote passwords. Older Agents need `runtime.steam-login.v1` support.

Credentials are encrypted under `steam-auth/` next to the management or Agent
configuration. Keep the encryption key and configuration together. Passwords are
never returned to the UI or included in process arguments, task logs or Agent
command history. Leave the password blank to retain it for the same account.
Saving anonymous mode removes the panel's saved account and password.

Verification lasts at most four minutes. Only active login sessions are polled.
Unattended downloads requiring Steam Guard return a clear verification error;
they never wait indefinitely or silently switch to anonymous. Account login does
not fix network, storage or filesystem permission failures. The optional
pre-API `DST_ADMIN_BOOTSTRAP_DST=true` bootstrap remains anonymous; use the UI
installation flow for account login.

If stored credentials or their key cannot be read, the UI offers **Reset login
settings** and downloads stop without switching to anonymous automatically.
After administrator confirmation, only the panel login settings shared by this
SteamCMD are cleared. Use anonymous login or configure the account again.
Other SteamCMD accounts, game files and saves are preserved.

## Use an existing server on the machine

If a registered installation location is empty or absent, click **Use an existing server** and enter an absolute path on the **selected machine**.
The UI checks the executable and version before asking you to confirm. Changing the path requires another inspection.
The job also rejects confirmation if the inspected executable or version has changed.

For container deployments, the path must be visible inside the container. All-in-One mounts host `/opt/dst` at container `/opt/dst` by default; enter `/opt/dst/...` in the UI.
Explicitly mount other host directories first. Keep a separately mounted source directory mounted after container recreation so the connection remains valid.

Adoption creates a directory link at the registered game location pointing to the existing game directory.
Game files remain in their original location without copying, moving, or overwriting. The connection survives Agent restarts.
The UI shows both the registered location and actual game directory. Subsequent updates and LuaJIT installation affect that actual game copy, so stop every world using it before adoption.
Only POSIX directory links are supported. Steam client installations on macOS remain managed by Steam.

Selecting a game directory **does not import saves**. The system continues using the existing save directory shown in the UI.
Import existing saves separately through room management. Adoption does not overwrite a populated game location and rejects source directories overlapping saves or the registered location.
The low-level installer does not delete saves or stop/start worlds. Managed updates in the UI use the game release coordinator for lifecycle operations.

## API and workload

All endpoints are under `/api/v2`. Installation and adoption return background jobs; login settings return sanitized status directly:

| Endpoint | Purpose |
| --- | --- |
| `GET /runtime-targets/game-installations?targetId=…` | List registered installations within the selected scope |
| `POST /runtime-targets/game-installations/preview` | Read-only usage and impact preflight; return a plan or blockers |
| `POST /game/releases` | Submit confirmed installation selection, policy and plan hash for coordinated maintenance |
| `POST /runtime-targets/game-installations/probe` | Inspect an existing directory |
| `POST /runtime-targets/game-installations/actions/install` | Install or validate the registered location |
| `POST /runtime-targets/game-installations/actions/adopt` | Confirm adoption of an existing directory |
| `GET /runtime-targets/game-installations/steam-login` | Read the selected node's login method and verification state |
| `POST /runtime-targets/game-installations/steam-login` | Configure, verify, submit a code or cancel verification |

Operations require exact `targetId` and `installationId` values. Inspection also requires `path`; adoption additionally requires the returned `fingerprint`.
Installation does not accept arbitrary destination paths. Remote failures do not fall back to the local host.
Leases, Agent operation journals, and installation directory locks coordinate installation; retrying the same operation does not execute it twice.

Installation status is read only when opening the page, changing scope, or refreshing manually.
The service does not scan disks for games, check Steam's latest release, or poll while idle. Only unfinished jobs have progress polled every two seconds.

Preflight scans installation processes and checks related nodes only on explicit action or recheck; there is no new idle polling. For `release` mode, submit the plan's `selectedInstallations` as `installations`, `policy`, and `planHash` (also as `confirmation`) to the release endpoint. For `install` mode, call the low-level installer only when `ready=true`.
