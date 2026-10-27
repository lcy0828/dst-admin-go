# AI assistant and OpenAPI

[简体中文](ai-assistant.md)

Open the **AI assistant** button on any management page. **Operations → AI assistant** provides the full conversation, connection settings and activity history.

## Get started

1. Enable AI in Connections, save the model endpoint, model name and credential, then test the connection.
2. Select a room and describe the requested inspection or operation, such as “Enable daily backups at 04:00 and keep the latest seven snapshots.”
3. Answers appear first; execution details stay collapsed. Downloadable results have a separate button.

The model must support **OpenAI Chat Completions tool calling**. Use a base URL such as `https://api.example.com/v1` or the full `/chat/completions` endpoint. Trusted local models may use HTTP and omit a credential when none is required.

The assistant acts as the current administrator. Explicitly requested operations execute directly; ambiguous targets or missing parameters require clarification. Inspection alone does not request changes. The selected room is conversational context; an explicit request can target other rooms or all rooms.

The browser retains the latest 32 messages and draft per administrator account. Navigation, minimizing, switching rooms and reloading preserve the conversation. Clearing browser data or changing browsers loses that local history. Reloading interrupts waiting but does not replay an operation; submitted jobs may continue. Activity records remain on the Controller.

## Management capabilities

The browser assistant and external MCP clients share the published OpenAPI management operations: rooms and worlds, configuration, lifecycle, saves, mods, players, backups and automation, installation and updates, runtime nodes, notifications and system settings. New management operations enter the tool catalog through the same contract.

- Compact tools cover common status, per-shard CPU/RSS, real players, configured mods, backups, automation, logs, jobs and command receipts.
- Prefer built-in commands for spawning entities or giving items. Their receipts include confirmed [spawn positions and destinations](command-spawn-records.en.md); incomplete records do not establish zero spawns.
- Use `find_api` with a domain or `operationId` to discover other operations, then call the typed `api_<operationId>` tool. Browser conversations load matching definitions as needed.
- Tool descriptions explain scope, prerequisites and side effects; parameter descriptions come from the published OpenAPI. Compatibility operations carry `deprecated` and `x-dst-replaced-by`: prefer `listCommands` and `listWorldStates`. Existing `listRoomCommands` and `refreshWorldStates` calls remain supported, while intent searches favor their replacements.
- Installed local mods support inspection, per-world enablement and configuration using `local:<directory name>`. Workshop operations retain numeric IDs. Local details require a target and installation; Steam cannot download local content. See [mod management](mod-management.md#本地模组) for directory and Agent requirements.
- `configureRoomBackupSchedule` enables, changes or pauses backups while preserving unspecified settings. A new plan defaults to 04:00 daily, `Asia/Shanghai`, keeping seven snapshots. Multiple plans require an explicit selection. Results include effective enablement and the next local execution time.
- Supply passwords, keys and Steam codes with **Private input** beside the composer. Values are encrypted; the model sees names and references only. Actual values are validated at execution time.
- Attach files beside the composer for import and upload operations. Limits are 2 GiB per file and 4 GiB total temporary attachments. References belong to the caller, expire after 24 hours and are pruned on later uploads.
- Downloads return authenticated temporary links; preparing a link does not inspect its contents. Sensitive API results, such as newly created credentials, are encrypted temporarily for the administrator to download; model input and chat history receive only redacted data.

Operations use the same authentication, deployment-role boundaries, validation, revision checks, capacity checks, Placement and Runtime behavior as the UI. Local execution stays in process; remote and split rooms follow applied placement without falling back to the Controller.

Job submission, execution and game readiness are different states. Check actual job or command results before reporting completion. Interrupted operations with unknown outcomes require verification before retrying. Stopped worlds normally have no live telemetry; projected capacity is not evidence of a current failure.

A transient model connection failure or temporary provider outage retries the model request once, without replaying platform operations. If the model cannot finish replying, actual receipts remain available and submitted jobs are checked when possible. Check their results before retrying. Individual model requests honor the configured turn timeout; active conversations send keepalives.

## External AI and API clients

Create a named, expiring credential in Connections. Its plaintext is shown once. It represents the administrator for **both MCP and REST API**. Expired or revoked credentials, and credentials used while AI is disabled, cannot start new requests.

MCP uses **Streamable HTTP**:

```json
{
  "mcpServers": {
    "dst-admin": {
      "type": "http",
      "url": "https://panel.example.com/mcp",
      "headers": { "Authorization": "Bearer <ACCESS_TOKEN>" }
    }
  }
}
```

Client fields vary. Select HTTP / Streamable HTTP transport. External clients use their own model; no panel model configuration is required. Management IP and origin checks still apply. Use HTTPS across networks.

Download the authenticated contract at `GET /api/v2/system/openapi`, or read [OpenAPI](openapi-v2.yaml).

```bash
curl -H 'Authorization: Bearer <ACCESS_TOKEN>' \
  https://panel.example.com/api/v2/rooms

curl -X POST \
  -H 'Authorization: Bearer <ACCESS_TOKEN>' \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: backup-schedule-001' \
  -d '{"enabled":true}' \
  https://panel.example.com/api/v2/rooms/<ROOM_ID>/backup-schedules/actions/configure
```

REST writes require `Idempotency-Key`. Cookie-authenticated writes also require `X-CSRF-Token`. MCP mutation tools accept `requestId`. Reuse a key only when retrying the same operation; different parameters conflict. Browser chat manages these fields automatically.

External clients upload with `POST /api/v2/system/ai/attachments`, multipart field `file`, using the same Bearer credential. Pass the returned `id` as `body.file.attachmentId` in a management upload tool. Downloads use that same identity.

Client operation mappings are generated from OpenAPI: run `go run ./cmd/openapi-manifest`, and generate the frontend copy with `-out <frontend>/src/api/openapiOperations.json`. CI checks route parity, operation identifiers, request and response schema references, and generated-file freshness.

## Conversation and resource settings

Connections → Browser chat limits controls tool rounds, tool calls and the total turn timeout. Defaults are **8 / 12 / 150 seconds**; **0** disables the corresponding limit. Saved changes apply to subsequent turns without restarting. Reaching a limit still allows a best-effort summary. External MCP clients manage their own conversation loops.

AI starts no idle collectors, model polling or background jobs. At most two AI requests and two actual tool executions run concurrently. Context, logs and results have size limits; large results preserve excerpts and receipt identifiers. Full records remain on their management pages.

Necessary state, logs and tool results are sent to the configured model. Common credentials are redacted, but arbitrary free text cannot be fully recognized; do not paste passwords or keys into ordinary chat. The model credential and temporary private results use `ai/provider.key` under the configuration directory; retain this key when backing up management data. External credentials are stored only as digests.

### MCP catalog views

The default `/mcp` connection exposes a compact set of common tools plus complete
API discovery and execution. Use `find_api` with keywords or `tags`, `access`,
`risk`, `runtime`; pass `includeSchema: true` for the input schema. Then call
`call_api` with `operationId` and matching `arguments` (including `requestId` for
writes). The original validation, credential scope, idempotency and Runtime guards
still apply.

For typed tools, connect to `/mcp?tags=backup&access=read`,
`/mcp?tags=mod,player`, or `/mcp?view=all`. `view=legacy` includes compatibility
helpers and deprecated aliases. Lists use standard MCP `nextCursor` pagination,
up to 64 tools per page; keep the same endpoint query for subsequent pages.
Tags match by OR; other dimensions match by AND. Listing filters do not restrict
execution authority, and existing tool names remain callable.

Standard annotations include titles and read-only, idempotent, destructive and
open-world hints. Namespaced `_meta["io.github.lcy0828.dst-admin/tool"]` contains
multi-domain tags, access, risk and runtime requirements. These describe behavior
and do not add approvals. Runtime `any` does not override operation-specific
requirements such as stopping a world. The generated `docs/openapi-operations.json`
shares this metadata; new operations inherit their declared domains.

## Current evidence and operation scope

Room identities and conversational context may be reused between turns. Current
world state, logs, players, performance and configuration require the matching
query in the current turn. Within the configured tool budget, the page assistant
requests one corrective lookup when this evidence is missing. If it remains
unavailable, the answer says it cannot confirm the result. Explanations and target
clarifications do not require tools. This check does not validate every model
claim; the returned timestamps, scope and partial-data markers remain authoritative.

Read-only queries run directly. Single-world lifecycle operations use
`runRoomAction` with an explicit `body.worldIds` selection. MCP clients receive
the same tool guidance, but their final answers are generated externally and are
not subject to the page assistant's response checks.

For Chinese search metadata, result schemas and client indexing, see [MCP tool discovery and results](mcp-tool-discovery.en.md).

For reported update failures or mod conflicts, use [Game and mod analysis](mod-diagnostics.en.md). The page and MCP share read-only evidence, error context and pre-update summary comparisons.

Use [Player activity records](player-activity.en.md) for persisted actions, positions, combat and collection settings. AI and the UI share this evidence; missing or expired history cannot be reconstructed.
