# Positions recorded by spawn commands

The homepage console and game workbench show spawn records for instrumented built-in commands. The page, OpenAPI and MCP share the same persisted command receipt, available again through command history after a page refresh.

Supported commands are `spawn_entity`, `spawn_domesticated_beefalo`, `give_item`, `give_blueprint` and `give_recipe_materials`. Records cover primary spawned objects; the beefalo command's saddles and bells are not separately inventoried. Raw and custom Lua remain available without automatically rewriting scripts or intercepting `SpawnPrefab`.

Each record includes its Prefab, quantity, available runtime entity identifier and destination:

- `world`: actual XYZ coordinates read after placing the entity.
- `inventory`: a confirmed inventory destination, without an independent ground position.
- `unresolved`: the destination cannot be confirmed, including consumed/merged source entities or mod handling. The player's position is never substituted for missing landing evidence.

`spawnAudit.origin` records the player's execution-time position separately. All coordinates describe their observation time, not a continuously tracked or current position. Entities may move afterward. `entityGuid` identifies a runtime instance only and must not be reused across restarts, world loads or rollbacks.

`spawnAudit.status` is `complete`, `partial` or `unavailable`. Details are limited to 40 primary objects per command; `omitted` reports additional objects. Failed commands, truncated output and older versions may have incomplete records. Missing evidence does not establish zero spawns. `total` counts objects that reached the recording step and is absent when the recording sequence did not finish; arbitrary mod side effects are not covered.

Records travel in the existing synchronous command receipt, without background sampling, world entity scans or per-entity network requests. Recording failures do not fail or replay the original action. A successful synchronous receipt does not guarantee later entity initialization or continued world health.
