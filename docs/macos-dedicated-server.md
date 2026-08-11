# macOS Dedicated Server Support

DST Admin supports the two macOS layouts used by Don't Starve Together:

- Steam game bundle: `Don't Starve Together/dontstarve_steam.app`
- Legacy dedicated server bundle: `dontstarve_dedicated_server_nullrenderer.app`

`DST_SERVER_PATH` may point to the installation directory, the `.app` bundle, `Contents/MacOS`, or the executable itself. DST Admin resolves the executable and related directories before readiness checks, shard startup, Mod setup changes, and version inspection.

## Recommended Settings

```ini
[paths]
DST_SERVER_PATH = /path/to/steamapps/common/Don't Starve Together
DST_SERVER_MODE = 64
DST_UGC_PATH = /path/to/steamapps/workshop

[mod]
STEAM_CMD_PATH = /opt/homebrew/bin/steamcmd
WORKSHOP_MOD_PATH = /path/to/steamcmd-library
WORKSHOP_CONTENT = /path/to/steamcmd-library/steamapps/workshop/content/322330
```

The two Workshop settings are one library: `WORKSHOP_CONTENT` must be the App content directory derived from `WORKSHOP_MOD_PATH`. The desktop Steam Workshop directory is only valid when SteamCMD is also configured to use that same Steam library root.

The Steam game layout resolves to:

- executable: `dontstarve_steam.app/Contents/MacOS/dontstarve_dedicated_server_nullrenderer`
- working directory: `dontstarve_steam.app/Contents/MacOS`
- Mod setup directory: `dontstarve_steam.app/Contents/mods`
- install root: the directory containing `dontstarve_steam.app`

## Runtime Requirements

- Install tmux with `brew install tmux` for shard lifecycle and console control.
- On Apple Silicon, install Rosetta 2 because the current DST macOS executable is x86_64.
- SteamCMD is optional for starting shards and remains available for Workshop downloads.
- `dontstarve_steam.app` is owned by the desktop Steam client (App `322330`). The admin reads its installed version but does not run anonymous SteamCMD App `343050` over that bundle; update it through Steam instead.
- The legacy `dontstarve_dedicated_server_nullrenderer.app` remains SteamCMD-managed as App `343050`.
- If SteamCMD initialization waits while the desktop Steam client is active, quit Steam once and run `steamcmd +quit`.

The readiness endpoint reports the resolved executable, layout, and content root. A missing SteamCMD remains a warning and does not make local shard control unavailable.
