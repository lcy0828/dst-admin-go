"""Optional room chat SSE; public-only forwarding, durable per-group outboxes."""

import asyncio
import copy
import time

from .broadcast import READ_TOOLS, timestamp
from .gateway import QueryError
from .streams import MCPReadError, resource_request, sse_frames

CHAT_STREAM_TOOL = "api_streamRoomChatLogs"
NOTICE_READ_TOOLS = READ_TOOLS | {CHAT_STREAM_TOOL}


def supports_chat_stream(hub):
    # This capability is optional: never add it to the required connection set.
    plugin = hub.plugin
    if not hasattr(plugin, "connections") or not hasattr(plugin,"manager"):
        return False
    try:
        server = plugin.connections.broadcast(frozenset({"api_listRooms", "api_listRoomWorlds", "api_listWorldStates", "api_listRoomPlayers"}))
    except QueryError:
        return False
    runtime = plugin.manager.mcp_server_runtime_view.get(server)
    return bool(runtime and any(
        tool.name == CHAT_STREAM_TOOL
        and tool.annotations is not None
        and tool.annotations.readOnlyHint is True
        for tool in runtime.client.tools
    ))


async def accept_chat(engine, room_id, kind, cursor, payload):
    if kind not in {"chat.cursor", "chat.record", "chat.gap", "chat.reset"}:
        return
    if not isinstance(cursor, str) or not cursor or len(cursor) > 160:
        raise MCPReadError("Invalid chat resume point")
    if not isinstance(payload, dict):
        raise MCPReadError("Invalid chat frame")
    operation = payload.get("operation")
    if kind == "chat.record" and (
        operation not in {"created", "updated", "deleted"}
        or not isinstance(payload.get("id"), str) or not payload["id"]
        or (operation == "created" and not isinstance(payload.get("entry"), dict)
            and payload.get("truncated") is not True)
    ):
        raise MCPReadError("Invalid chat record")
    async with engine.lock:
        previous = copy.deepcopy(engine.state)
        try:
            room = engine.room(room_id)
            if room.get("chat_cursor") == cursor:
                return
            if kind == "chat.record":
                entry = payload.get("entry") or {}
                if entry and entry.get("id") != payload["id"]:
                    raise MCPReadError("Chat record identity mismatch")
                # Updates/deletes are history changes, not a second public message.
                item = {
                    **entry, "id": payload["id"],
                    "_historical": payload.get("historical") is True or operation != "created",
                    "_truncated": payload.get("truncated") is True,
                }
                if payload.get("kind") != "say" or entry.get("kind") != "say":
                    item["_historical"] = True
                engine.record_chat(room_id, [item])
                if payload.get("truncated") is True:
                    room["chat_stream_warning"] = "truncated"
                    engine.log("聊天推送内容过大，本条未转发；可在面板查看原记录")
            elif kind in {"chat.gap", "chat.reset"}:
                room["chat_stream_warning"] = kind
                engine.log("聊天续读范围发生变化，历史覆盖可能不完整；继续接收可用消息")
            room["chat_cursor"] = cursor
            await engine.persist()  # Outbox, seen IDs and cursor commit together.
        except BaseException:
            engine.state = previous
            raise


async def chat_stream_session(hub, room_id, engines):
    cursors = [e.room(room_id).get("chat_cursor") for e in engines]
    common = cursors[0] if cursors and all(c and c == cursors[0] for c in cursors) else None
    args = {"path": {"roomId": room_id}, "query": {"kind": "say"}}
    cached = hub.chat_resources.get(room_id)
    if cached and cached[0] > time.time():
        resource = cached[1]
    else:
        data = await hub.read(CHAT_STREAM_TOOL, args)
        resource = data.get("resource", {})
        expires = timestamp(resource.get("expiresAt")) or time.time() + 300
        hub.chat_resources[room_id] = (expires - 15, resource)
    plugin = hub.plugin
    server = plugin.connections.broadcast(frozenset({"api_listRooms", "api_listRoomWorlds", "api_listWorldStates", "api_listRoomPlayers"}))
    url, headers = resource_request(plugin.connections.configuration(server), resource)
    # Reuse the authenticated resource; create no new link for every reconnect.
    # The resource has no fixed cursor, so each GET can resume independently.
    headers = {key: value for key, value in headers.items() if key.lower() != "last-event-id"}
    if common:
        headers["Last-Event-ID"] = common
    # Fail an opening handshake within 20 seconds, but leave a healthy stream open.
    async with asyncio.timeout(20) as deadline:
        async with hub.http.stream("GET", url, headers=headers) as response:
            if response.status_code in {404, 410}:
                hub.chat_resources.pop(room_id, None)
            if response.status_code != 200 or "text/event-stream" not in response.headers.get("content-type", ""):
                raise MCPReadError("Chat stream unavailable")
            deadline.reschedule(None)
            initial = True
            async for kind, cursor, payload in sse_frames(response.aiter_lines()):
                if not isinstance(payload, dict) or kind == "chat.error":
                    raise MCPReadError("Chat stream error")
                current = [e for e in hub.receivers(room_id) if e.options.get("chat", False)]
                if (
                    hub.source != plugin.connection_identity()
                    or not plugin.catalog.valid()
                    or current != engines
                ):
                    raise MCPReadError("Chat subscriptions changed")
                if kind == "chat.reconnect":
                    return
                if initial and not common:
                    if kind != "chat.cursor" or not cursor:
                        raise MCPReadError("Missing initial chat cursor")
                    # Capture the stream tail BEFORE bounded per-group catch-up.
                    # Messages written during history reads are replayed from this
                    # tail and deduped. A new group seeds history without broadcasting it.
                    for engine in engines:
                        was_seeded = engine.room(room_id)["chat_seeded"]
                        await engine.poll_chat(room_id)
                        if was_seeded:
                            engine.log("聊天订阅重新对齐，已补查近期公屏；超过 2 分钟或 400 条的记录不补播")
                    initial = False
                for engine in engines:
                    await accept_chat(engine, room_id, kind, cursor, payload)
                hub.wakeup.set()  # All acknowledged outboxes are durable.
    raise MCPReadError("Chat stream disconnected")
