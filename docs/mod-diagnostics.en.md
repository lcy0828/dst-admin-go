# Game and mod analysis

For a reported failure after a game update, an incomplete start or an unexpected world exit, open **Room diagnostics → Mods and versions** and select the world. Failed worlds on the dashboard also link to this page. Opening the panel reads evidence and generates a report. Neither AI configuration nor a healthy in-game DST Admin Lua module is required.

## Evidence

The report shows installed game versions and Steam Builds separately, per-installation mod versions and manifests, enabled mod file states, and the author's `dst_compatible` / `api_version` declarations. Missing declarations, parser limitations and older Agent responses remain unknown. Cached dependency declarations may be incomplete or optional.

The bounded current log supplies the earliest visible error and adjacent stack context. Workshop and local mod references are identified as leads. **A mod appearing in a stack is not proof that it caused the error.** Logs preceding the latest recorded start are excluded from current-start attribution. Missing, partial and unmatched logs remain explicit limitations. Current files are not presented as a proven snapshot of the process when the error occurred.

A comparison uses the most recent summary captured before a game update executed: same-AppID game versions and Builds, mod versions/manifests, enabled lists and configuration digests. The summary excludes configuration contents and does not establish historical compatibility.

## Update integration and limits

Game update previews and maintenance confirmations display nonblocking advisories. They neither guarantee compatibility with the target game version nor change update readiness or plan hashes. A summary is saved before game files are written. Retrying the same update does not replace it. Summary persistence failures appear in update details without removing normal backup protection.

Analysis does not disable mods, edit configuration or saves, issue Lua commands, restart worlds or run isolation tests. Reproduction belongs in a separate test environment.

There is no new background scanner or polling loop. The service permits 2 concurrent read groups, coalesces simultaneous requests for the same world and bounds each group to 20 seconds. Preflight reuses room mod facts and does not fetch new Steam metadata or scan mod source.

Each report covers at most 512 enabled mods and 1000 log lines, with up to 4 error contexts of 40 lines each. Update batches cover at most 128 worlds and report incomplete coverage when capped. Each world retains its latest 5 pre-update summaries, pruned on new writes. No findings is not a compatibility guarantee.

Use **Export report** to save JSON evidence. Saves and configuration contents are excluded. Common credentials and identifiers are masked in log excerpts; review all excerpts for other sensitive mod output before sharing.

## AI / MCP / OpenAPI

The UI and AI share this read-only operation:

```http
GET /api/v2/rooms/{roomId}/worlds/{worldId}/mod-diagnostics
```

Discover `analyzeWorldModCompatibility` through MCP using mod-conflict or game-update-failure keywords. Respect `findings[].kind`, `logCorrelation`, `limitations` and `evidence`. Missing observations are not empty mod lists, and suspected involvement is not confirmed causation.
