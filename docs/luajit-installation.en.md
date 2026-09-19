# LuaJIT2 installation

[简体中文（默认）](luajit-installation.md) | **English**

Choose a version under **Game server management → LuaJIT2 acceleration** and install it.
The list distinguishes upstream releases, compatibility builds, and imported packages. A compatibility build is selected by default when available; you can choose an upstream release or imported package instead.
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

Automatic restarts after cold backups, save restores, game updates, and mod updates preserve each world's previous Lua runtime mode.
Remote nodes require Agent 2.16.14 or later. If the mode cannot be determined, maintenance asks you to wait or upgrade before stopping worlds.
If a game update makes the previous mode unavailable, startup reports a failure instead of silently switching to Game Lua.

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

## Upstream and compatibility

The target node queries [DontStarveLuaJIT2 releases](https://github.com/fesily/DontStarveLuaJIT2/releases) and downloads the selected version when the user requests installation or an update. Downloading, caching, validation, and application happen on the target host; a Controller-provided package is also available.

Compatibility depends on architecture, the dynamic loader, GLIBC/GLIBCXX/CXXABI, and package contents. A new release is not automatically compatible: dependency checks run on the target, preserve the existing installation on failure, and explain the reason. Older distributions can use the supplied compatibility build.

Upstream packages need no extra launch-mode declaration or private launch switch. Verify game startup after updates. A game update may replace LuaJIT files; reinstall when the page reports this. See [bundled package provenance and licenses](../internal/luajit/packages/README.md).

## Downloads and proxies

The selected runtime node connects to GitHub directly first. If the request fails, release metadata uses `gh-proxy.com` and package downloads use `ghfast.top`; GHFast does not support the GitHub API. Only public LuaJIT upstream URLs use these fallbacks. Agent keys, controller transfer credentials and custom download URLs are never forwarded to them. SHA-256, archive and system dependency checks still apply.

No configuration is required. To select a fixed route, optionally set `DST_ADMIN_GITHUB_ACCESS=direct` or `proxy` on the actual runtime node; the default is `auto`. This does not proxy SteamCMD downloads. Bundled compatibility packages and explicit ZIP uploads remain available offline.
