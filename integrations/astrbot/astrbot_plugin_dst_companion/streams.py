"""Bounded, same-panel SSE resources; never forward credentials cross-origin."""

import json
import re
import uuid
from urllib.parse import parse_qsl, urljoin, urlsplit


class MCPReadError(RuntimeError):
    pass


def closed_transport(error):
    """AnyIO can wrap a closed SDK session in an exception group."""
    return type(error).__name__ in {
        "ClosedResourceError",
        "BrokenResourceError",
        "EndOfStream",
    } or any(closed_transport(child) for child in getattr(error, "exceptions", ()))


async def stateless_read(http, config, name, arguments, allowed):
    """Read-only recovery for this panel's stateless JSON MCP endpoint.

    Reuses the explicitly configured endpoint/credentials, never reconnects or
    replaces AstrBot's shared client, and never accepts arbitrary tool names.
    """
    if name not in allowed or not endpoint_valid(config):
        raise MCPReadError("Read recovery is not authorized")
    from mcp.types import CallToolResult

    request_id = uuid.uuid4().hex
    headers = {
        key: value
        for key, value in config.get("headers", {}).items()
        if key.lower() not in {"mcp-session-id", "accept", "content-type", "mcp-protocol-version"}
    }
    headers.update(
        {"Accept": "application/json, text/event-stream", "MCP-Protocol-Version": "2025-06-18"}
    )
    async with http.stream(
        "POST",
        config["url"],
        headers=headers,
        follow_redirects=False,
        json={
            "jsonrpc": "2.0",
            "id": request_id,
            "method": "tools/call",
            "params": {"name": name, "arguments": arguments},
        },
        timeout=18,
    ) as response:
        if response.status_code != 200 or "application/json" not in response.headers.get(
            "content-type", ""
        ):
            raise MCPReadError("Read recovery endpoint unavailable")
        chunks, size = [], 0
        async for chunk in response.aiter_bytes():
            size += len(chunk)
            if size > 2 << 20:
                raise MCPReadError("Read recovery response too large")
            chunks.append(chunk)
    try:
        data = json.loads(b"".join(chunks))
        if not isinstance(data, dict) or data.get("id") != request_id or "error" in data:
            raise ValueError()
        return CallToolResult.model_validate(data["result"])
    except (ValueError, TypeError, KeyError):
        raise MCPReadError("Invalid read recovery response") from None


def endpoint_valid(config):
    try:
        url = urlsplit(str(config.get("url", "")))
        # These parameters only select the panel's MCP listing view. Keep the
        # configured URL intact, including AstrBot's pagination compatibility
        # switch; never accept credentials or arbitrary routing parameters.
        query = parse_qsl(url.query, keep_blank_values=True, strict_parsing=True, max_num_fields=2)
        allowed = {"view": {"compact", "all", "legacy", "bot"}, "pagination": {"on", "off"}}
        return (
            url.scheme in {"http", "https"}
            and bool(url.hostname)
            and not (url.username or url.password or url.fragment)
            and len({key for key, _ in query}) == len(query)
            and all(value in allowed.get(key, ()) for key, value in query)
            and url.path.rstrip("/").endswith("/mcp")
        )
    except ValueError:
        return False


def unpack(result):
    if getattr(result, "isError", False):
        raise MCPReadError("MCP tool failed")
    texts = [item.text for item in result.content if getattr(item, "type", None) == "text"]
    try:
        value = json.loads(texts[0]) if texts else getattr(result, "structuredContent", None)
    except (TypeError, ValueError):
        raise MCPReadError("Invalid MCP response") from None
    if not isinstance(value, dict) or value.get("status") in (
        "failed",
        "error",
        "denied",
        "rejected",
    ):
        raise MCPReadError("MCP operation failed")
    data = value.get("data", value)
    if isinstance(data, dict) and "httpStatus" in data:
        code = data["httpStatus"]
        if not isinstance(code, int) or code < 200 or code >= 300:
            raise MCPReadError("MCP API returned an unsuccessful status")
        data = data.get("data")
    if not isinstance(data, dict):
        raise MCPReadError("MCP returned no business data")
    return data


def resource_request(config, resource):
    """Follow only a resource issued by this same configured MCP server."""
    origin = urlsplit(str(config.get("url") or ""))
    target = urlsplit(urljoin(config.get("url", ""), str(resource.get("url") or "")))
    if origin.scheme not in ("http", "https") or origin.username or origin.password:
        raise MCPReadError("MCP server must have an HTTP endpoint without URL credentials")
    if (target.scheme, target.hostname, target.port) != (
        origin.scheme,
        origin.hostname,
        origin.port,
    ):
        raise MCPReadError("Refusing a resource from a different origin")
    if (
        not re.fullmatch(
            re.escape(origin.path.removesuffix("/mcp").rstrip("/"))
            + r"/api/v2/system/ai/resources/[A-Za-z0-9_-]+",
            target.path,
        )
        or target.fragment
        or target.username
        or target.password
    ):
        raise MCPReadError("Invalid MCP resource path")
    if resource.get("kind") != "stream":
        raise MCPReadError("Expected a streaming resource")
    return target.geturl(), {**config.get("headers", {}), "Accept": "text/event-stream"}


async def sse_frames(lines):
    """Parse complete SSE frames, including multiline data and heartbeat comments."""
    event, event_id, data, size = "message", "", [], 0
    async for raw in lines:
        line = raw.rstrip("\r\n")
        if not line:
            if data:
                yield event, event_id, json.loads("\n".join(data))
            event, event_id, data, size = "message", "", [], 0
            continue
        if line.startswith(":"):
            continue
        size += len(line.encode("utf-8"))
        if size > 128 * 1024:
            raise MCPReadError("SSE event exceeds the size limit")
        field, _, value = line.partition(":")
        value = value[1:] if value.startswith(" ") else value
        if field == "event":
            event = value
        elif field == "id" and "\x00" not in value:
            event_id = value
        elif field == "data":
            data.append(value)
    # An incomplete frame at EOF is deliberately not committed.
