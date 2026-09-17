# LuaJIT2 installation

[简体中文（默认）](luajit-installation.md) | **English**

Choose a version under **Game server management → LuaJIT2 acceleration** and install it.
The list distinguishes upstream releases, compatibility builds, and imported packages.
By default, the selected Agent or local Runtime manages versions, downloads, caching, validation, and installation. The Controller dispatches jobs and displays progress.
The local Runtime uses an in-process installer; it does not start or connect to an extra local Agent.

**Check upstream updates** asks the current node to read GitHub metadata and SHA-256 for the latest stable Linux package.
It does not download, install, or start background polling. Installation then downloads and verifies the selected version on that node.
Future upstream releases can be followed directly while the package layout and VM interface remain compatible, without adding a private capability file.
Changes to package structure, removed interfaces, or higher system requirements can still require adaptation or a suitable compatibility build.

You can also supply an HTTPS URL and SHA-256, then choose to download on the runtime node.
The Controller transfers a package only when you explicitly upload a ZIP and choose **Transfer from Controller / Transfer and install**. The receiving node stores it in its cache.
Offline nodes and download failures produce explicit errors; they do not automatically use the Controller or another node instead.

Stop all worlds sharing the DST installation before installing. Afterward, the startup dialog offers original Game Lua, LuaJIT, or experimental generational GC in advanced settings.
Startup uses only upstream `-lua_vm_type=game|jit|jit_gen`. JIT compilation follows upstream configuration without a separate override switch.
Ordinary startup does not download, repair, or replace resources. If a SteamCMD update overwrites the launcher, reinstall/repair LuaJIT.

Currently supported: Linux x64, 64-bit DST, and Native Runtime (including regular All-in-One).
Installation is not currently offered for Windows, macOS, Linux ARM, or independent-shard containers.
Remote Agents and Controllers must use the updated versions supporting `runtime.luajit.v2` and the simplified `luajit` startup mode.
Old JIT-on / JIT-off requests are rejected so their meaning cannot change silently.

## Validation and save protection

- Require an explicit `targetId + installationId`. Missing remote targets do not fall back to the local host.
- ZIP limit: 256 MiB; extracted limit: 1 GiB. Reject path traversal, symbolic links, duplicate files, non-Linux-x64 ELF binaries, and missing runtime files. No private mode contract file is required.
- Downloads require an HTTPS source, HTTPS redirects, and a specified SHA-256. Failed validation does not publish to cache.
- Before replacing runtime files, the target node checks ELF dynamic dependencies and required system-library versions. For example, official v3.0.0 needs newer GLIBC / GLIBCXX than Debian 12 provides, so use a compatibility build there.
- Optional Controller transfers use relative paths and short-lived credentials for one package. Credentials are not sent to third-party download sources.
- The original game executable is retained as `_1`. The Mod, bootstrap library, locator file, and launcher are backed up together and rolled back on failure. After an interrupted installation transaction, the next installation restores it first; unrestored backups are retained.
- Shared/exclusive installation directory file locks coordinate game processes, updates, and installation. Master and Caves can run concurrently.
- The installer does not modify room configuration, mod lists, saves, or tokens, and does not automatically stop or restart worlds.

`DST_ADMIN_LUAJIT_RELEASE_DIR` can set each node's cache directory.
The local Runtime defaults to `dst-admin/luajit-releases` under the system user's configuration directory; the Agent defaults to `luajit-releases` beside its operation state file.
The optional Controller upload repository uses a separate `controller-transfers/` directory.
The bundled compatibility ZIP is extracted into the node cache only for explicit installation; listing reads small metadata only.
The UI requests only the selected node's list and polls progress only while a job is unfinished.

API: `GET /runtime-targets/luajit` returns `installations` and optional `transfers`.
`GET /runtime-targets/luajit/status?targetId=…&installationId=…` reads the selected installation.
Only adding `refreshUpstream=true` causes that node to query GitHub. Package metadata can include `channel` (`upstream` / `compatibility`) and `sourceUrl`.
Downloads supply `targetId + installationId + url + sha256`; installation supplies `targetId + installationId + releaseId`.
`source` defaults to `runtime`; only explicit `controller` requests transfer a package.

## Upstream and compatibility builds

Upstream: <https://github.com/fesily/DontStarveLuaJIT2>.
On 2026-09-16, a node query found v3.0.0 as the latest stable release, at upstream commit `2d40d7bb1a00a0bf7ed8cc5bd8b4433d58b1ee89`.
The original `linux_Mod.zip` is 45,159,683 bytes, SHA-256:
`58374ccd18e6e13225e1a3c980a92fb06bfa26a17e7338fe4c78fc7a5ec9b1f3`.

Local merge commit: `a621a55`; Linux startup compatibility fix: `bfcc646`; removal of custom JIT flags and the private contract: `fcdff97`.
The compatibility build retains Linux automatic-signature fixes, atomic signature writes, Debian 12 runtime dependencies, and required plugin resources.
It falls back to ordinary replacement only when Frida's fast replacement of libc `chdir` returns `GUM_REPLACE_WRONG_SIGNATURE`.
This condition was reproduced on the native Linux test host, where ordinary replacement succeeded, so the fix is retained.

The bundled compatibility ZIP is in `internal/luajit/packages/`, version 3.0.0, source `fcdff97`, 12,534,607 bytes, SHA-256:
`49a6278e35db7caaf535f239a7154398e4c6a16aab955a7d8848fdd121a7a6e0`.
Build it with the source repository's `tools/linux/build-debian12.sh`, place Frida `COPYING` under `Mod/licenses/frida/`, package it with `tools/linux/package-admin.py`, and update `packages/manifest.json`.
The ZIP includes provenance and third-party licenses, with no test shims or tokens.

## Verification on 2026-09-16

- All 53 CMake tests, 669 frontend tests, the production frontend build, and relevant Go package tests passed.
- Browser checks cover node selection, upstream checks, node downloads, default installation, optional Controller transfers, mobile layout, and startup choices after removing the JIT switch.
- On native Debian 12 amd64 at `192.168.2.23`, the official package failed the dependency check without changing the original game executable digest. The new compatibility package installed successfully into an independent game copy.
- An independent offline test world (DST 747465) on that host started with `-lua_vm_type=jit`, ran for **180.8 seconds** after readiness, passed periodic world-state queries, and exited with code 0. A separate short startup called upstream `GameInjector.DS_LUAJIT_get_vm_type_name(0)` to confirm VM `jit`.
- The updated management service ran in an isolated local environment for 180.3 seconds. Session and LuaJIT version-list endpoints worked, and exit code was 0.
- Agent tests on the same host cover node downloads, SHA failures, cached installation, idempotent retries, and optional Controller transfer. Fixtures can use `DST_ADMIN_TEST_STEAM_API` to reference the game's Steam library or a C compiler to create a test library that is not executed. Real startup verification used actual game dependencies.
- Test root: `/opt/dst-admin-luajit-review-20260916`. The original production Agent, rooms, and `/opt/dst/saves` were not replaced or cleaned. The original save backup is `preserved/original-saves.tar` below that test root, 157,962,240 bytes, SHA-256: `902702567956fb28e5eb430cb27955a72c812937230d9b3e0b4d7109459fc1c1`.

Static `ready` status means installation checks such as file layout and version passed. Actual VM startup is established by the process, console queries, and runtime logs.
These tests cover startup and three minutes of operation for an independent world. They do not claim verification of every mod in the user's original saves, long-term operation, or performance gains.
