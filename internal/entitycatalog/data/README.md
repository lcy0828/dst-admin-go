# Vanilla display metadata

`vanilla.json` contains display-only Prefab IDs, Chinese/English names and
categories from [Klei Entertainment’s Don't Starve Together](https://www.klei.com/games/dont-starve-together),
build **759671**. It contains **8,025** registered vanilla Prefabs and no game
scripts, constructors, textures, mods or saves. World registration still decides
whether an operation is supported; this directory does not spawn entities.

`GET /api/v2/entity-catalog/directory` returns `gameVersion`, `builtAt`, source
and SHA-256 identifiers. These describe the embedded directory, independently of
the selected game installation. Pagination and `q` search use the same in-memory
directory, loaded once; HTTP requests never extract game archives.

Names retain their existing fallback behavior for older clients. Check
`translated` before treating `nameZhCN` as a Chinese translation. The
`translationSource` distinguishes official names, official scrapbook aliases
(for generated variants), unique shared official names, curated names and
fallbacks. Unknown internal entities can legitimately have no localized name.
Artwork remains a separate optional pack and may target an earlier game build.

## Rebuild after a game update

Use a disposable, unmodded test world from the intended game build. Do not
export a player's world or load third-party mod code. In its administrator Lua
console, export display metadata without spawning anything:

```lua
local rows = {}
local book = require("screens/redux/scrapbookdata")
for id in pairs(Prefabs) do
    local v = book[id] or {}
    rows[#rows + 1] = {
        id = id, nameEn = STRINGS.NAMES[string.upper(id)] or "",
        category = v.type or "", subcategory = v.subcat or "", nameKey = v.name or ""
    }
end
table.sort(rows, function(a, b) return a.id < b.id end)
TheSim:SetPersistentString("mod_config_data/dst-admin/vanilla-export.json",
    json.encode_compliant({ gameVersion = APP_VERSION, items = rows }), false,
    function(ok) print("Vanilla metadata export:", ok, #rows) end)
```

Copy the generated JSON from the test shard's `save/mod_config_data/dst-admin/`
and `data/databundles/scripts.zip` from that same game installation. If the
export contains an engine header before its JSON, remove only that header.
Then run (pin the timestamp to keep the build reproducible):

```bash
python3 deploy/scripts/rebuild-entity-directory.py \
  --registered vanilla-export.json --scripts scripts.zip \
  --game-version 759671 --built-at 2026-10-26T00:00:00Z \
  --categories internal/entitycatalog/data/vanilla.json \
  --output /tmp/vanilla.json
```

The builder validates the exported game version, IDs and duplicates, reads only
the official Chinese PO file from the archive, and preserves reviewed resource
categories. Review the generated display metadata before replacing the embedded
file and run `go test ./internal/entitycatalog`. Game content remains the
property of its respective owners; do not commit the input scripts archive.
