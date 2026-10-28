# Player activity records

Open **Players → Player activity**, or **Activity records** in a player's menu. Historical records remain available while the player is offline. AI is optional.

Recording is enabled for all players by default. Worlds need a compatible DST Admin Runtime; remote worlds also need a compatible Agent. The UI separates saved settings from each node's applied revision, and shows output/sync timestamps, dropped events and gaps. Stopped or disconnected worlds apply settings after their Runtime becomes available.

## Coverage

Records joins, leaves, deaths, respawns, sampled positions, pickup/drop/harvest/eat/build/work events, incoming/outgoing combat and kill events. Repeated actions and combat are grouped. Coordinates in a grouped action describe its latest action, not every individual action. Mod objects use their actual Prefabs; custom mod behavior that bypasses standard game events may not be observed.

Boss participation is separate from kills. Only resolved damage reported by the game counts as confirmed damage; unknown damage is not zero. GUIDs identify objects only within a Runtime instance. There is no historical backfill.

## Dynamic settings

Settings apply to all players in the room without restarting running worlds. Positions, interactions and combat can be toggled separately; lifecycle events follow the master switch. Already queued records can still be synced after disabling.

| Setting | Default | Range |
| --- | --- | --- |
| Position interval | 15 s | 5–300 s |
| Minimum movement | 10 game units | 0–100 |
| Combat grouping window | 10 s | 5–120 s |
| Action grouping / node flush | 5 s | 2–30 s |
| Controller sync | 10 s | 5–60 s |
| Retention | 7 days | 1–90 days |
| Logical record budget per room | 512 MiB | 16–2048 MiB |

Position records require both sufficient elapsed time and movement. Combat publication may take an additional flush interval. Shorter intervals increase record volume. Paths are schematic and only cover the current page, separated by world, Runtime instance and gaps; they are not an exact replay or map. Gap timestamps describe discovery, not the exact missing interval; durable range identifiers preserve breaks even across pages. Legacy records with unknown continuity remain isolated points.

## Resource bounds

The Runtime listens to game events and samples online players. It never scans all world entities, hooks every frame, or performs per-event disk/network I/O. Player sampling and heartbeat timers stop with no online players. Controller polling backs off for idle, disabled or unavailable worlds.

Other players joining or leaving do not reset the existing sampling cadence. Before a Runtime hot update, pending records are written in batches. A failed or timed-out write defers replacement and leaves the original collector running; the update does not forcibly clear its queue.

Each world has a 512-event pending queue, batches of at most 128 events / 64 KiB, and 64 reusable spool segments. Per-player action groups and combat targets are each capped at 32. Critical events have reserved queue capacity but can still be lost under extreme load. The controller fetches up to 8 new segments at a time, with at most 2 concurrent worlds. Longer outages can exceed the spool window.

Drops, missing segments and instance changes are visible. Missing records are not evidence of inactivity. Crashes may lose unflushed records; an instance boundary is not proof of continuous coverage.

Retention and logical size cleanup only delete activity records, never game saves, backups or unrelated data. Cleanup is bounded and can temporarily lag the budget. SQLite may retain reusable pages; the budget is not the database file size.

## API and MCP

Authenticated `/api/v2/rooms/{roomId}/player-activity/` endpoints expose `settings`, `status`, `events`, `trajectory` and `combat`. GETs query persisted evidence only. Settings updates require the latest `revision`; conflicts return 409. Check node-applied revisions before claiming recording is active.

MCP and the built-in assistant use the same OpenAPI operations. UI JSON exports contain only the displayed page or bounded combat summary, not the entire history.

## Synchronization and recovery

Configuration acknowledgement, node output, synchronization and retained-history coverage are separate observations. A healthy Runtime does not prove complete history; an old coverage gap does not mean current recording has stopped. Inspect `syncState`, `coverage` and bounded metadata diagnostics. Segment counts never imply exact missing times or action counts.

A problematic segment retains a descriptor for deferred recovery while independently validated records continue to import. Configuration application runs before historical reads. Recovery shares the existing bounded scheduler, the 64-slot source window and retention policy. It cannot restore records intentionally removed by retention or capacity cleanup. Unknown write completion stays unconfirmed; a repeated hash does not prove a publisher committed the file.

**Re-sync** wakes the same coordinator without resetting cursors, replaying gameplay commands or restarting worlds. Acceptance is not completion: check subsequent status. Offline nodes, unsupported protocols, unavailable disks and writes without a terminal callback remain explicitly blocked until their underlying cause is resolved.

### MCP arguments

Read actual IDs from `listRooms` and `listRoomWorlds`. `path` is an object of API identifiers, never a filesystem directory. Put filters in `query`. `find_api` supports `includeSchema: true`; refresh stale client tool catalogs when this option is missing.

```json
{
  "operationId": "listRoomPlayerActivity",
  "arguments": {
    "path": { "roomId": "ID returned by listRooms" },
    "query": { "userId": "KU_EXAMPLE", "limit": 10 }
  }
}
```

Use `getRoomPlayerTrajectory` with required `query.userId`, `projection: "compact"` and a small `limit` to retrieve coordinate points without full combat fields. Omit `projection` to retain the legacy event response. Only connect matching nonempty `continuousRangeId` values within the same world, producer and session. Legacy records with unknown continuity remain isolated points; upgrades do not manufacture historical paths.

Use `resyncRoomPlayerActivity` with `path.roomId` and a unique `requestId`, then `getRoomPlayerActivityStatus` to check results. `accepted: true` only confirms scheduling. Follow `hasMore` and `nextCursor` using identical filters/time bounds. Coverage is independent of the current page; `_aiTruncated` means the AI excerpt is incomplete.
