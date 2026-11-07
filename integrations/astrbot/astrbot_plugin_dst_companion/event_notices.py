"""Optional durable room facts. No model calls, Lua, or implicit game actions."""

import asyncio
import copy
import time

from .broadcast import timestamp
from .display import time_text
from .gateway import QueryError
from .streams import MCPReadError, resource_request, sse_frames

BASE_TOOLS = frozenset({"api_listRooms", "api_listRoomWorlds", "api_listWorldStates", "api_listRoomPlayers"})
READ_TOOL = "api_readRoomNotificationEvents"
STREAM_TOOL = "api_streamRoomNotifications"
SYSTEM_TOOL = "api_readNotificationEvents"
EVENT_TOOLS = frozenset({READ_TOOL, STREAM_TOOL, SYSTEM_TOOL})
CATEGORIES = {"maintenance", "season", "updates", "backup", "software", "players", "boss", "mods", "alerts"}
LABELS = {"maintenance": "房间维护", "season": "季节变化", "updates": "更新提醒", "backup": "备份提醒", "software": "面板更新", "players": "死亡复活", "boss": "Boss击杀", "mods": "模组消息", "alerts": "运行告警"}
CATEGORY_KINDS = {"maintenance": {"room.operation", "world.change", "backup.result"}, "season": {"world.season"}, "updates": {"game.available", "game.result", "mod.available", "mod.activation"}, "backup": {"backup.result"}, "players": {"player.death", "player.respawned"}, "boss": {"boss.killed"}, "mods": {"mod.notice"}, "alerts": {"runtime.alert"}}
RESULTS = {"slow": "运行偏慢", "recovered": "已恢复", "running": "进行中", "requested": "已提交", "succeeded": "已完成", "failed": "失败", "partial": "部分完成", "uncertain": "结果待确认", "canceled": "已取消", "available": "发现新版", "observed": "已观测", "prepared": "等待重启", "waiting_for_players": "等待重启", "loaded": "加载已确认", "blocked": "未完成", "recovery_required": "需要处理", "abandoned": "维护已结束", "notifying": "准备更新", "updating": "正在更新", "activating": "正在重启", "stopped": "已停止", "unexpected_exit": "异常停止", "rolled_back": "已退回原版本", "interrupted": "更新中断", "timeout": "等待超时，结果未确认"}


def room_kinds(engines):
    return ",".join(sorted({kind for category, kinds in CATEGORY_KINDS.items() if any(e.options.get(category, False) for e in engines) for kind in kinds}))


async def compatible_kinds(hub, room, engines):
    requested = set(room_kinds(engines).split(",")) - {""}
    added = {"player.death", "player.respawned", "boss.killed", "mod.notice", "runtime.alert"}
    if not requested & added:
        return ",".join(sorted(requested))
    cache = getattr(hub, "notice_capabilities", {})
    if room not in cache or cache[room][0] <= time.monotonic():
        # Probe a fixed legacy kind. An unfiltered probe would accidentally
        # subscribe to game checks and performance alerts on a newer panel.
        page = await hub.read(READ_TOOL, {"path": {"roomId": room}, "query": {"tail": True, "limit": 1, "kinds": "room.operation"}})
        known = page.get("supportedKinds") if isinstance(page, dict) else None
        if known is None:
            available = set().union(*(v for k,v in CATEGORY_KINDS.items() if k not in {"players", "boss", "mods", "alerts"}))
        elif not isinstance(known, list) or len(known)>128 or any(not isinstance(k,str) or len(k)>64 for k in known):
            raise MCPReadError("Invalid event capabilities")
        else:
            available = set(known)
        cache[room] = (time.monotonic()+300, available)
        hub.notice_capabilities = cache
    available = cache[room][1]
    missing = requested - available
    if missing:
        errors = getattr(hub, "session_errors", {})
        for session, engine in getattr(hub, "engines", {}).items():
            if engine in engines and room in engine.rooms:
                errors[session] = "部分新增播报需要更新面板；已支持的播报继续运行。"
        hub.session_errors = errors
    return ",".join(sorted(requested & available))


def supports(hub, name):
    try:
        server = hub.plugin.connections.broadcast(BASE_TOOLS)
        runtime = hub.plugin.manager.mcp_server_runtime_view.get(server)
        return bool(runtime and any(t.name == name and t.annotations is not None and t.annotations.readOnlyHint is True for t in runtime.client.tools))
    except (QueryError, AttributeError):
        return False


async def accept_page(engine, room_id, page, *, platform=False, kinds=None):
    if not isinstance(page, dict) or not isinstance(page.get("items"), list) or len(page["items"]) > 100:
        raise MCPReadError("Invalid event page")
    cursor = page.get("nextCursor")
    if not isinstance(cursor, str) or not cursor or len(cursor) > 1024:
        raise MCPReadError("Invalid event cursor")
    async with engine.lock:
        previous = copy.deepcopy(engine.state)
        try:
            room = engine.state.setdefault("platform", {}) if platform else engine.room(room_id)
            prefix = "software" if platform else "notice"
            if not platform and "collection" in page:
                collection=page["collection"]
                if not isinstance(collection, dict) or set(collection)-{"players","boss","alerts"} or any(v not in {"enabled","disabled","unavailable"} for v in collection.values()):
                    raise MCPReadError("Invalid collection policy")
                room["notice_collection"]=dict(collection)
            seen = room.setdefault(prefix + "_seen", [])
            historical = {}
            for event in page["items"]:
                if not isinstance(event, dict):
                    raise MCPReadError("Invalid event record")
                key = event.get("id")
                if not isinstance(key, str) or not key or len(key) > 512:
                    raise MCPReadError("Invalid event identity")
                if key in seen:
                    continue
                seen.append(key)
                if platform:
                    if event.get("kind") not in {"software.available", "agent.software.available", "software.result"} or event.get("roomId"):
                        continue
                    category = "software"
                else:
                    if event.get("roomId") != room_id:
                        raise MCPReadError("Event room mismatch")
                    category = event.get("category")
                    if event.get("kind") not in CATEGORY_KINDS.get(category, set()):
                        continue
                # New categories require an explicit opt-in; legacy task records
                # without structured category facts are not forwarded twice.
                if category not in CATEGORIES or not engine.options.get(category, False):
                    continue
                status = event.get("status", "uncertain")
                age = engine.clock() - timestamp(event.get("occurredAt"))
                if age < -30 or age > 86400 or status in {"running", "requested", "notifying", "updating", "activating"} and age > 120:
                    continue
                if category == "backup" and status in {"running", "succeeded"}:
                    continue
                title, body = event.get("title"), event.get("message")
                if not isinstance(title, str) or not isinstance(body, str) or not title or not body:
                    continue
                if category in {"players", "boss", "mods", "alerts"} and age > 600:
                    # Old gameplay facts and old alerts remain queryable in the
                    # panel. They are not broadcast as current game events.
                    continue
                if age > 600:
                    historical.setdefault(category, []).append((key, title, body, status))
                    continue
                facts = event.get("facts") or {}
                if not isinstance(facts, dict):
                    raise MCPReadError("Invalid event facts")
                if category in {"players", "boss", "mods"} and facts.get("worldId"):
                    body = engine.world_name(room_id, facts["worldId"]) + " · " + body
                event_time = time_text(event.get("occurredAt"))
                if category == "players" and age >= 15 and event_time:
                    event_time = f"{event_time}（延迟 {int(age)} 秒送达）"
                text = engine.message(room_id, "event_" + category, {
                    "title": title, "body": body, "result": ("已启动" if event.get("kind") == "room.operation" and status == "running" and not (event.get("facts") or {}).get("action") else RESULTS.get(status, "状态见正文")),
                    "time": event_time,
                    **({"room_name": "管理程序", "room_full_name": "管理程序"} if platform else {}),
                })
                related = {"roomId": room_id, "kind": category, "eventKind": event.get("kind", ""), "jobId": event.get("jobId", ""), "status": status}
                if category in {"players", "boss", "mods", "alerts"}:
                    related["expiresAt"] = timestamp(event.get("occurredAt")) + 600
                if category in {"players", "boss"} and isinstance(facts.get("playerId"), str) and facts["playerId"].startswith("KU_"):
                    related.update(playerId=facts["playerId"], name=facts.get("playerName", ""))
                engine.queue("notice/" + key, text, related)
                operation = facts.get("operationId")
                if operation and category in {"maintenance", "updates"}:
                    tracked = engine.room(room_id).setdefault("maintenance_active", {})
                    if status in {"running", "requested", "notifying", "updating", "activating"} and facts.get("action") != "save":
                        tracked[operation] = engine.clock() + 3600
                        engine.quiet_presence(room_id, 3600)
                    elif operation in tracked and (status != "uncertain" or facts.get("day") is not None):
                        tracked.pop(operation, None)
                        if not any(until > engine.clock() for until in tracked.values()):
                            engine.room(room_id)["quiet_until"] = engine.clock() + engine.grace
                    engine.room(room_id)["maintenance_active"] = {k:v for k,v in tracked.items() if v > engine.clock()}
            for category, records in historical.items():
                body = "\n".join(title + "：" + body for _, title, body, _ in records[-5:])
                values = {"title": "离线期间动态", "body": body, "result": "历史记录", "time": ""}
                if platform:
                    values.update(room_name="管理程序", room_full_name="管理程序")
                text = engine.message(room_id, "event_" + category, values)
                engine.queue("notice/history/" + category + "/" + cursor, text, {"roomId": room_id, "kind": category})
            room[prefix + "_cursor"] = cursor
            if kinds is not None:
                room[prefix + "_kinds"] = kinds
            room[prefix + "_seen"] = seen[-1024:]
            if page.get("historyTruncated"):
                room[prefix + "_warning"] = "部分历史已超出保留范围"
            await engine.persist()  # Cursor and outbox advance together.
        except BaseException:
            engine.state = previous
            raise


async def poll_room(hub, room, engines):
    kinds = await compatible_kinds(hub, room, engines)
    if not kinds:
        return False
    for engine in engines:
        for _ in range(4):
            cursor = engine.room(room).get("notice_cursor", "")
            if engine.room(room).get("notice_kinds") != kinds:
                cursor = ""
            page = await hub.read(READ_TOOL, {"path": {"roomId": room}, "query": {"after": cursor, "tail": not bool(cursor), "limit": 50, "kinds": kinds}})
            await accept_page(engine, room, page, kinds=kinds)
            if not page.get("hasMore"):
                break
    hub.wakeup.set()


async def stream_room(hub, room, engines):
    kinds = await compatible_kinds(hub, room, engines)
    if not kinds:
        return False
    cursors = [e.room(room).get("notice_cursor") for e in engines]
    if not cursors or not all(c and c == cursors[0] for c in cursors) or any(e.room(room).get("notice_kinds") != kinds for e in engines):
        await poll_room(hub, room, engines)
        return
    cached = hub.event_resources.get(room)
    if not cached or cached[0] <= time.time() or len(cached) < 3 or cached[2] != kinds:
        data = await hub.read(STREAM_TOOL, {"path": {"roomId": room}, "query": {"kinds": kinds}})
        resource = data.get("resource", {})
        hub.event_resources[room] = (timestamp(resource.get("expiresAt")) - 15, resource, kinds)
    resource = hub.event_resources[room][1]
    server = hub.plugin.connections.broadcast(BASE_TOOLS)
    url, headers = resource_request(hub.plugin.connections.configuration(server), resource)
    headers = {k: v for k, v in headers.items() if k.lower() != "last-event-id"}
    headers["Last-Event-ID"] = cursors[0]
    async with asyncio.timeout(20) as deadline:
        async with hub.http.stream("GET", url, headers=headers) as response:
            if response.status_code in {404, 410}:
                hub.event_resources.pop(room, None)
            if response.status_code != 200 or "text/event-stream" not in response.headers.get("content-type", ""):
                raise MCPReadError("Event stream unavailable")
            deadline.reschedule(None)
            async for kind, _, page in sse_frames(response.aiter_lines()):
                if hub.source != hub.plugin.connection_identity() or not hub.plugin.catalog.valid() or hub.receivers(room) != engines:
                    return
                if kind != "notice.page":
                    continue
                for engine in engines:
                    await accept_page(engine, room, page, kinds=kinds)
                hub.wakeup.set()


async def room_feed(hub, room):
    failures = 0
    while True:
        engines = hub.receivers(room)
        if not any(any(e.options.get(c, False) for c in CATEGORIES - {"software"}) for e in engines):
            return
        if not supports(hub, READ_TOOL):
            hub.session_errors.update({s: "当前面板或密钥未提供扩展事件播报；原有播报继续使用。" for s, e in hub.engines.items() if room in e.rooms})
            await asyncio.sleep(60)
            continue
        try:
            if not await compatible_kinds(hub, room, engines):
                await asyncio.sleep(60)
                continue
            if supports(hub, STREAM_TOOL) and failures < 3:
                await stream_room(hub, room, engines)
            else:
                await poll_room(hub, room, engines)
                await asyncio.sleep(5)
            failures = 0
        except asyncio.CancelledError:
            raise
        except Exception as error:
            if isinstance(error, QueryError) and error.code == "NOTIFICATION_INVALID":
                getattr(hub, "notice_capabilities", {}).pop(room, None)
                hub.event_resources.pop(room, None)
                for engine in engines:
                    async with engine.lock:
                        engine.room(room).pop("notice_cursor", None)
                        engine.room(room)["notice_warning"] = "服务端读取位置已改变，历史覆盖可能不完整"
                        await engine.persist()
            failures = min(5, failures + 1)
            hub.warn(f"房间动态同步暂不可用（{type(error).__name__}），保留读取位置，不重试游戏操作")
            await asyncio.sleep(min(60, 2 ** failures))


async def poll_platform(hub, engine):
    room = next(iter(engine.rooms))
    state = engine.state.get("platform", {})
    cursor = state.get("software_cursor", "")
    seeding = not state.get("software_seeded", False)
    kinds = "software.available,agent.software.available"
    if supports(hub, READ_TOOL):
        kinds += ",software.result"
    if state.get("software_kinds") not in {None,kinds}:
        cursor, seeding = "", True
    for _ in range(4):
        page = await hub.read(SYSTEM_TOOL, {"query": {"kinds": kinds, "after": cursor, "limit": 100}})
        if seeding:
            if not isinstance(page, dict) or not isinstance(page.get("nextCursor"), str) or not page["nextCursor"]:
                raise MCPReadError("Invalid system cursor")
            async with engine.lock:
                previous = copy.deepcopy(engine.state)
                try:
                    saved = engine.state.setdefault("platform", {})
                    saved["software_cursor"] = page["nextCursor"]
                    saved["software_kinds"] = kinds
                    saved["software_seeded"] = not page.get("hasMore", False)
                    await engine.persist()
                except BaseException:
                    engine.state = previous
                    raise
        else:
            await accept_page(engine, room, page, platform=True)
        cursor = page["nextCursor"]
        if not page.get("hasMore"):
            break


async def platform_feed(hub):
    failures = 0
    while True:
        engines = [e for e in hub.engines.values() if e.options.get("software", False) and e.rooms]
        if not engines:
            return
        try:
            if not supports(hub, SYSTEM_TOOL):
                hub.warn("当前面板连接未开放面板版本通知，房间播报继续运行")
                await asyncio.sleep(60)
                continue
            for engine in engines:
                await poll_platform(hub, engine)
            hub.wakeup.set()
            failures = 0
        except asyncio.CancelledError:
            raise
        except Exception as error:
            if isinstance(error, QueryError) and error.code == "NOTIFICATION_INVALID":
                for engine in engines:
                    async with engine.lock:
                        engine.state.pop("platform", None)
                        await engine.persist()
            failures = min(5, failures + 1)
            hub.warn(f"管理程序通知暂不可用（{type(error).__name__}），房间播报继续运行")
        await asyncio.sleep(max(30, 2 ** failures))
