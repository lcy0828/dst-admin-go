"""One private stateless connection; never registers privileged tools with AstrBot."""

import asyncio
import json
import re
import time
import uuid
from types import SimpleNamespace
from urllib.parse import urlsplit, urlunsplit

import httpx

from .configuration import fingerprint
from .gateway import QueryError, unpack

PRIVATE_NAME = "__dst_private_panel__"
LEGACY_MOVEMENT = frozenset({
    "workbench.teleport_player", "workbench.relocate_players",
    "workbench.teleport_to_nearest_entity",
})


class PrivatePanel:
    def __init__(self, config):
        self.config = config
        self.http = None
        self.protocol = 1
        self.full_access = False
        self.features = []
        self.tools = []
        self.session = self
        self.lock = asyncio.Lock()
        self.identity = ""
        self.until = 0

    @property
    def configured(self):
        return bool(str(self.config.get("panel_url", "")).strip())

    @property
    def _mcp_server_config(self):
        base = urlsplit(str(self.config.get("panel_url", "")).strip())
        if (
            base.scheme not in {"http", "https"}
            or not base.hostname
            or base.username
            or base.password
            or base.query
            or base.fragment
        ):
            raise QueryError("PANEL_ADDRESS_INVALID")
        path = base.path.rstrip("/")
        if not path.endswith("/mcp"):
            path += "/mcp"
        token = str(self.config.get("panel_token", ""))
        if not token.startswith("dstai_") or len(token) > 100 or any(c in token for c in "\r\n"):
            raise QueryError("PANEL_CREDENTIAL_REQUIRED")
        return {
            "url": urlunsplit((base.scheme, base.netloc, path, "view=bot", "")),
            "headers": {"Authorization": "Bearer " + token},
            "active": True,
        }

    async def rpc(self, method, params, timeout):
        config = self._mcp_server_config
        identity = fingerprint(config)
        request = uuid.uuid4().hex
        headers = {
            **config["headers"],
            "Accept": "application/json, text/event-stream",
            "MCP-Protocol-Version": "2025-06-18",
        }
        if self.http is None:
            self.http = httpx.AsyncClient(
                follow_redirects=False, limits=httpx.Limits(max_connections=4)
            )
        async with self.http.stream(
            "POST",
            config["url"],
            headers=headers,
            timeout=timeout,
            json={"jsonrpc": "2.0", "id": request, "method": method, "params": params},
        ) as response:
            if response.status_code != 200:
                raise QueryError(
                    "PANEL_CREDENTIAL_REJECTED"
                    if response.status_code in {401, 403}
                    else "PANEL_DISCONNECTED"
                )
            if "application/json" not in response.headers.get("content-type", ""):
                raise QueryError("MCP_RESPONSE_INVALID")
            chunks, size = [], 0
            async for chunk in response.aiter_bytes():
                size += len(chunk)
                if size > 2 << 20:
                    raise QueryError("RESULT_TOO_LARGE")
                chunks.append(chunk)
        value = json.loads(b"".join(chunks))
        if (
            not isinstance(value, dict)
            or value.get("id") != request
            or "error" in value
            or "result" not in value
        ):
            raise QueryError("MCP_RESPONSE_INVALID")
        if identity != fingerprint(self._mcp_server_config):
            raise QueryError("CONFIG_CHANGED")
        return value["result"]

    async def refresh(self):
        from mcp.types import Tool

        async with self.lock:
            identity = fingerprint(self._mcp_server_config)
            if identity == self.identity and time.monotonic() < self.until:
                return
            self.tools, self.until = [], 0
            self.protocol, self.full_access, self.features = 1, False, []
            rows, cursors, params = [], set(), {}
            async with asyncio.timeout(10):
                for _ in range(8):
                    result = await self.rpc("tools/list", params, 8)
                    page = result.get("tools")
                    if not isinstance(page, list) or len(page) > 512:
                        raise QueryError("CAPABILITY_INCOMPATIBLE")
                    rows.extend(Tool.model_validate(v) for v in page)
                    if len(rows) > 512 or len({v.name for v in rows}) != len(rows):
                        raise QueryError("CAPABILITY_INCOMPATIBLE")
                    cursor = result.get("nextCursor")
                    if not cursor:
                        break
                    if not isinstance(cursor, str) or len(cursor) > 2048 or cursor in cursors:
                        raise QueryError("CAPABILITY_INCOMPATIBLE")
                    cursors.add(cursor)
                    params = {"cursor": cursor}
                else:
                    raise QueryError("CAPABILITY_INCOMPATIBLE")
            if identity != fingerprint(self._mcp_server_config):
                raise QueryError("CONFIG_CHANGED")
            if any(t.name == "api_getBotProtocol" for t in rows):
                info = unpack(await self.call_tool("api_getBotProtocol", {}))
                versions = info.get("protocolVersions", [])
                common = [v for v in (1, 2) if v in versions]
                if not common or 1 not in info.get("capabilityContractVersions", []):
                    raise QueryError("CAPABILITY_INCOMPATIBLE")
                self.protocol = max(common)
                self.full_access = info.get("profile") == "panel-manage"
                self.features = [v for v in info.get("features", []) if isinstance(v, str)][:32]
            else:
                # Older panels have no handshake. Their own API authorization remains final.
                self.full_access = all(
                    any(t.name == n for t in rows)
                    for n in ("api_runBotCapability", "api_listRoomRuntimeEvents")
                )
            self.tools, self.identity, self.until = rows, identity, time.monotonic() + 60

    async def confirmation_request(self, name, arguments, timeout):
        # The master credential stays in this trusted plugin transport. Models
        # only see guarded facades; native MCP cannot call this approval route.
        from urllib.parse import quote
        config = self._mcp_server_config
        base = urlsplit(config["url"])
        path = arguments.get("path", {})
        room, preview = path.get("roomId"), path.get("previewId")
        if not isinstance(room, str) or not room or len(room) > 255:
            raise QueryError("PARAMETER_INVALID")
        body = arguments.get("body", {})
        if name == "api_runBotCapability" and (
            path.get("capabilityId") in LEGACY_MOVEMENT or
            isinstance(path.get("capabilityId"),str) and re.fullmatch(r"workbench\.[a-z][a-z0-9_]{0,63}",path["capabilityId"])
            and body.get("parameters",{}).get("phase") in {"execute","cancel"}
            and isinstance(body.get("parameters",{}).get("preview_id"),str)
            and re.fullmatch(r"[a-f0-9-]{36}",body["parameters"]["preview_id"])
        ):
            # Only the trusted engine reaches this adapter, after rechecking the
            # actual group member and consuming their confirmation message.
            suffix = "/bot/actions/" + path["capabilityId"]
            request_id = arguments.get("requestId")
            if not isinstance(request_id, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._:-]{7,127}", request_id):
                raise QueryError("PARAMETER_INVALID")
        elif name in {"api_executeRoomTeleport", "api_cancelRoomTeleport"}:
            if body or not isinstance(preview, str) or not re.fullmatch(r"[a-f0-9-]{36}", preview):
                raise QueryError("PARAMETER_INVALID")
            action = "execute" if name == "api_executeRoomTeleport" else "cancel"
            suffix = "/teleport-previews/" + preview + "/" + action
            request_id = "teleport-" + preview + "-" + action
        else:
            raise QueryError("PERMISSION_DENIED")
        url = urlunsplit((base.scheme, base.netloc, base.path.removesuffix("/mcp") + "/api/v2/rooms/" + quote(room, safe="") + suffix, "", ""))
        identity = fingerprint(config)
        if self.http is None:
            self.http = httpx.AsyncClient(follow_redirects=False, limits=httpx.Limits(max_connections=4))
        async with self.http.stream("POST", url, headers={**config["headers"], "Idempotency-Key": request_id}, json=body, timeout=timeout) as response:
            if response.status_code != 200:
                raise QueryError("PANEL_CREDENTIAL_REJECTED" if response.status_code in {401,403} else "API_FAILED")
            chunks, size = [], 0
            async for chunk in response.aiter_bytes():
                size += len(chunk)
                if size>65536: raise QueryError("RESULT_TOO_LARGE")
                chunks.append(chunk)
        value=json.loads(b"".join(chunks))
        if identity != fingerprint(self._mcp_server_config): raise QueryError("CONFIG_CHANGED")
        if value.get("error") or not isinstance(value.get("data"),dict): raise QueryError("API_FAILED")
        # Preserve the MCP transport envelope; the nested business result may
        # itself be partial, running or uncertain.
        data = {"status":"succeeded", "data":value["data"]}
        return {"content":[{"type":"text","text":json.dumps(data)}],"structuredContent":data}

    async def call_tool(self, name, arguments, read_timeout_seconds=None):
        from mcp.types import CallToolResult

        seconds = read_timeout_seconds.total_seconds() if read_timeout_seconds else 18
        if name in {"api_executeRoomTeleport", "api_cancelRoomTeleport"} or (
            name == "api_runBotCapability" and (
                arguments.get("path", {}).get("capabilityId") in LEGACY_MOVEMENT or
                str(arguments.get("path",{}).get("capabilityId","")).startswith("workbench.")
                and arguments.get("body",{}).get("parameters",{}).get("phase") in {"execute","cancel"}
            )
        ):
            return CallToolResult.model_validate(await self.confirmation_request(name, arguments, seconds))
        # No retry here, including transport loss after a management request.
        return CallToolResult.model_validate(
            await self.rpc(
                "tools/call",
                {"name": name, "arguments": arguments},
                seconds,
            )
        )

    async def close(self):
        if self.http is not None:
            await self.http.aclose()


class PrivateManager:
    """Plugin-only view over the host manager; private tools never enter host registration."""

    def __init__(self, host, private):
        self.host, self.private = host, private

    @property
    def mcp_server_runtime_view(self):
        if not self.private.configured:
            return self.host.mcp_server_runtime_view
        return {
            **self.host.mcp_server_runtime_view,
            PRIVATE_NAME: SimpleNamespace(client=self.private),
        }

    def __getattr__(self, name):
        return getattr(self.host, name)
