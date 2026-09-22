# Game workbench

[简体中文](game-workbench.md)

Press **T** in the player actions or open the game workbench to operate on that player's room and world. The standalone Game tools page also lets you select a target.

## Finding resources

The entity browser defaults to **All**, with categories for items/materials, food, tools/equipment, structures, creatures, bosses, nature and other resources. Search supports Chinese and English names, Prefab IDs and mod names. Filtering and ranking happen across the complete directory before pagination.

- **Frequently used** combines successful actions, selections after searching and curated common resources, favoring recent activity. Exact name/Prefab matches take precedence over usage frequency. Recently used and alphabetical ordering are also available.
- History stays in this browser, with at most 200 entries expiring after 90 days. Search text is never stored. Failed, canceled or unconfirmed actions do not count as successful usage. Clear history under More options to restore recommended ordering.
- The embedded text directory has 6,121 entries from DST 747465, available without the artwork pack or a running world. Opening the entity section also reads the selected running world's vanilla and mod registrations. Prefabs are deduplicated; mod overrides retain their provenance without borrowing vanilla categories or images.
- Unknown categories remain under Other and All, including internal entities. Classification is for browsing and does not establish inventory compatibility.
- More options contains source/mod filters and Refresh catalog. Enter a valid unlisted Prefab in the search box to use it with the existing give, spawn, remove, nearby-action and teleport tools.

World reads are demand-driven, without background polling, spawning, prefab constructors or save changes. Complete reads use internal batches of at most 2,048 entries and 128 KiB of encoded item JSON. The vanilla directory is displayed while the first complete world read runs. Subsequent searches, sorting and pagination run in the browser without game calls. The panel coalesces concurrent reads of the same world and allows at most two active collections and two cached snapshots, with a five-minute cache lifetime. The actual observation time remains visible. Loading and bounded retries have explicit status feedback; transient failures retry at most twice before showing a direct retry button. Stopped, unselected and failed worlds are distinct states. A failed manual refresh keeps the last complete directory for the same world and labels it as potentially stale. Changing worlds or closing the workbench cancels pending requests; incomplete reads and unreachable worlds show an error rather than presenting a partial directory as complete.

Game collection yields between batches targeting about 3 ms, including during empty-world auto-pause. This is a scheduling target, not a hard real-time guarantee. Initial index construction has a ten-second deadline; complete paged reads have a 30-second deadline, a 50,000-entry cap and an 8 MiB metadata budget. Retry or use a Prefab directly if a limit is reached. The cache does not automatically follow newly installed mods; refresh after updating mods and restarting the world.

It requires a running world with DST Admin Runtime **2.4.8** or newer. For existing worlds, install the Runtime update and reload its modules through Runtime management. For remote worlds, first update the Agent to a version containing this Runtime. Unavailable targets report an error without falling back to another machine.

Mod attribution comes from the game's loaded registrations. Names use the game's language, falling back to the Prefab code. Some mods modify existing behavior without adding entities. Registration cannot establish inventory compatibility; actual components are checked when giving an item. Mod-specific abilities can be added under Lua / Commands.

## Lua and the command library

**Lua / Commands** exposes existing built-in and custom commands with parameter forms. Write server-side Lua, execute it directly, or save it to the server command library for reuse in other browsers and Command settings. Saving never executes the script.

Examples cover world information, loaded mods, player components and giving items. Insert the selected player's `player` variable when needed. This inserts an explicit KU ID; a saved script does not silently retarget another player.

Execution shows the room and world and asks for confirmation. Changing code, parameters or target clears that confirmation. Existing administrator authentication, run records and DST receipts remain in use; uncertain results are never automatically replayed. The workbench displays synchronous `print` output, status and errors for each run, with the latest 10 runs and their executed code available for review. Output is stored with its run ID, limited to 16 KiB with an explicit truncation notice; output before a script error is preserved. Original output also remains in world logs. This requires in-game Runtime **2.4.10**; older versions or missing receipts show output as unavailable rather than empty. Delayed callbacks, scheduled tasks and other previously cached print functions are outside the synchronous capture scope; use world logs for their output.

Code executes in the **DST server Lua environment**, including loaded mod interfaces. Client UI, mouse selection and client-only functions are not server interfaces. Scripts are limited to 4096 UTF-8 bytes. They run on the game main thread: infinite loops, broad scans and mass spawning can stall the game. A request timeout stops the controller from waiting; it cannot terminate Lua that is already running. Split expensive work into batches scheduled by the game.

New plain Lua scripts use `scriptMode: literal`, preserving braces inside tables and strings. Existing parameterized definitions retain `template` mode. Import and export preserve the mode.

## Entity artwork

World catalog cards prefer the installed vanilla artwork pack, then bundled artwork. Other thumbnails are read lazily from the selected runtime installation's game or mod atlases. Mod overrides never use the vanilla pack. Reads never send game commands, scan saves, or query external image sites. Remote nodes need an updated Agent advertising `runtime.entity-artwork.v1`.

Supported sources are vanilla inventory/minimap atlases and mod XML/KTEX atlases under `images` or `minimap` with an exact `<Prefab>.tex` or `<Prefab>.png` element. Mod overrides never borrow vanilla pictures. Effects, dynamic image aliases, animation-only entities, missing files, and assets beyond the limits keep a placeholder without affecting entity operations.

Indexes and thumbnails expire after five minutes; idle caches are released. Each process decodes at most one atlas at a time, retains at most one decoded texture and 8 MiB of PNG thumbnails, and accepts textures up to 2048×2048. A mod scan is limited to 512 XML atlases and 8192 directory entries.

## Optional artwork pack

Open **Artwork pack** beside the entity catalog to download/install or import an official `.tar.gz` file. Choose GitHub or the ghfast.top China proxy (default); the dialog shows progress, speed, version and size. If a source is unavailable, switch sources or import the downloaded archive.

The current pack comes from **DST build 747465**: **6,121 catalog entries**, **2,441 illustrated entities** and 2,414 unique WebP images. It occupies about **11.3 MiB**, with a **10.8 MiB** download. All 1,883 official scrapbook entries have artwork; some registered entities and mod images remain uncovered. Artwork and game data originate from [Klei Entertainment's Don't Starve Together](https://www.klei.com/games/dont-starve-together).

This is a shared panel display resource for local and Agent rooms, stored beside the panel's `app.conf` under `resource-packs/entity-artwork/`. Container deployments persist it with their panel configuration directory. It is optional and is not bundled into the binary or image, nor sent to game nodes.

Installation, replacement and uninstall take effect immediately without restarting the panel or game. Uninstall removes only this pack and restores existing artwork sources; games, mods and saves are untouched. Downloads are explicit and use a pinned SHA-256, bounded extraction and path/type checks. Failed replacements retain the installed pack. Offline import accepts the official version advertised by the panel; new trusted versions ship with panel updates.
