# MCP tool discovery and results

[中文](mcp-tool-discovery.md)

The default `/mcp` catalog stays compact. Both the page assistant and external
clients use `find_api` with Chinese requests, symptoms, English keywords or an
exact operation ID, then invoke `api_<operationId>` or `call_api`. Discovery
does not execute a management operation.

```json
{"query":"automatic room backups","limit":5}
```

Results include the English `summary`, Chinese `purpose`, classifications,
runtime requirements and `resultSummary`. Add `includeSchema: true` for input
and receipt schemas. Request complete business result schemas for one exact ID:

```json
{"query":"getRoomPlayerInventory","includeResultSchema":true}
```

`includeResultSchema` includes input/output schemas and `resultContract` for one
operation. A broad query with this option is rejected: discover first, then use
the returned ID. Existing management validation and runtime guards still apply.

## Search metadata

Canonical descriptions contain a concise Chinese purpose, the original English
description, and a result summary. Detailed search phrases are under
`_meta["io.github.lcy0828.dst-admin/tool"]`:

| Field | Meaning |
| --- | --- |
| `cn_purpose` | Purpose and capability boundaries in Chinese |
| `actions` | Explicit actions and synonymous requests |
| `symptoms` | Applicable requests and problem descriptions |
| `slang` | DST terminology, technical terms and abbreviations |
| `resultSummary` | Key fields and interpretation notes |
| `locale` / `semanticVersion` | Metadata language and format version |
| `catalogRevision` | Content fingerprint covering OpenAPI and semantic data |

Existing names, classifications and deprecation metadata remain compatible.
Backups, notifications and other domains are ranked by relevance, without a
blanket domain penalty. Runtime hints do not establish current readiness or add
approval steps.

Clients building vector indexes can paginate `/mcp?view=all` using `nextCursor`
and index this metadata. Cache by `catalogRevision`; rebuild when it changes.
Do not send the whole catalog to a model on every turn. Default panel search is
in memory and uses no embedding API, resident model, vector database or idle
background polling.

## Result interpretation

MCP `outputSchema` describes the actual call receipt. `structuredContent` and
the text fallback carry the same data. This abbreviated receipt represents an
accepted backup task, not a completed backup:

```json
{
  "callId": "example-receipt",
  "status": "succeeded",
  "data": {
    "operationId": "createRoomBackup",
    "httpStatus": 202,
    "data": { "id": "example-job", "status": "queued" }
  }
}
```

- Outer `status` describes the tool call. HTTP 202 or a queued/running job
  requires checking the actual job result.
- API business data is normally at `data.data`. `resultContract.responses`
  documents complete business schemas per successful HTTP status. References
  are self-contained `$defs`.
- Redaction and truncation can remove fields or shorten values. `_aiTruncated`
  means partial evidence, never proof that missing data is empty or healthy.
  Complete business schemas are documentation, not validators for excerpts.
- Failed calls may return `status: failed` with `data.error`. For `uncertain`,
  inspect real tasks or records before retrying a mutation. Protocol, credential
  or argument errors can also produce MCP errors.
- Download and stream tools return authenticated `data.resource` links.
  `fetched: false` means the contents have not been inspected or downloaded and
  no persistent subscription has been established.
- Historical player observations are not current presence; file modification
  time is not logout time; saved Mod settings are not proof a process loaded them.

## Calling and tracking game updates

Query an exact operation ID with `find_api` and `includeSchema: true` to receive
a complete arguments example. Canonical `api_` tool descriptions include the same
example. Replace sample IDs and hashes with real values. `path` is a nested object;
`confirmation` is the hash string returned by preview.

For example, preview the local installation through `call_api`:

```json
{"operationId":"previewGameRelease","arguments":{"body":{"installations":[{"targetId":"local","installationId":"default"}],"policy":{"restartRunning":true,"loadConfirmation":"logs","timeoutSeconds":300}},"requestId":"game-preview-001"}}
```

Preview refreshes affected installation inventories on demand, without an open
browser or manual fleet refresh. Once ready, pass the returned exact scope,
policy and `planHash` to `publishGameRelease`, using the same hash string for
`confirmation`. Use different `requestId` values for different operations; reuse
one only when safely resending the same request.

| Operation | ID and purpose |
| --- | --- |
| `getGameRelease` | Original `releaseId`; backups, updates, shard restoration and recovery guidance |
| `getJob` | Returned `jobId` for this attempt; job status and target results |
| `retryGameRelease` | Original `path.releaseId`; returns a new retry Job |
| `getGameUpdateRun` | Legacy `game.update` only, not `game.release` or its retries |

The first Job ID is also the Release ID. After a retry, these are different IDs.
These `call_api` examples show the nested arguments; replace the sample IDs:

```json
{"operationId":"getGameRelease","arguments":{"path":{"releaseId":"24a00000-0000-4000-8000-000000000001"}}}
{"operationId":"retryGameRelease","arguments":{"path":{"releaseId":"24a00000-0000-4000-8000-000000000001"},"requestId":"game-retry-001"}}
{"operationId":"getJob","arguments":{"path":{"jobId":"24a00000-0000-4000-8000-000000000002"}}}
```

`stage=protecting` means maintenance protection backups are running. A temporary
stop during maintenance does not establish a separate crash. Progress 100%,
`outcome=none` or an absent legacy update log does not prove success or rollback.
Check the current Job and original Release for actual versions, validation and
restoration results.

## Provenance and maintenance

[ai-tool-semantics.json](ai-tool-semantics.json) is keyed by OpenAPI operation ID.
Initial vocabulary was contributed by [hxyinan in issue #23](https://github.com/lcy0828/dst-admin-go/issues/23),
from [291 tool metadata entries](https://gist.github.com/hxyinan/70eaf31ce16c63e9b45f257b2d8cbfbd).
Purposes and scenarios were corrected against platform behavior and extended
for newer operations. The contributor's retrieval benchmarks are not measured
performance of the panel's default search.

New callable operations need maintained purposes, actions, example requests and
terms. Keep parameters and response schemas in OpenAPI. CI checks coverage,
obsolete entries, schema references, Chinese discovery scenarios and MCP receipt
compatibility. Deprecated aliases with explicit replacements inherit their
replacement's vocabulary and retain deprecation notices.
