"""One event feed per room, per-session durable delivery, shared MCP budget."""

import asyncio
import copy
import json
import time

import httpx

from .broadcast import BroadcastEngine
from .display import REFERENCE_LIMIT, REFERENCE_TTL
from .chat_stream import CHAT_STREAM_TOOL, NOTICE_READ_TOOLS, chat_stream_session, supports_chat_stream
from .gateway import QueryError, unpack
from .notice_settings import notices_enabled
from .event_notices import BASE_TOOLS, EVENT_TOOLS, SYSTEM_TOOL, room_feed as facts_feed, platform_feed, supports as supports_events

NOTICE_READ_TOOLS = NOTICE_READ_TOOLS | EVENT_TOOLS
from .sessions import group_origin
from .streams import MCPReadError, closed_transport, resource_request, sse_frames, stateless_read


class Notifications:
    MAX_ROOMS = 8
    MAX_WORLDS = 4
    MAX_SESSIONS = 16

    def __init__(self, plugin):
        self.plugin = plugin
        self.engines, self.tasks = {}, {}
        self.http = None
        self.loop_task = None
        self.lock = asyncio.Lock()
        self.storage_lock = asyncio.Lock()
        self.saved = {}
        self.cache, self.read_tasks = {}, {}
        # Notifications retain their own small budget, so two foreground
        # queries cannot block joins/chat enrichment behind model operations.
        self.read_limit = asyncio.Semaphore(2)
        self.source = ""
        self.status = "关闭"
        self.session_errors = {}
        self.delivery_errors = {}
        self.warned = {}
        self.ready = set()
        self.presence_tasks = {}
        self.wakeup = asyncio.Event()
        self.closed_sessions = set()
        self.chat_resources = {}
        self.event_resources = {}
        self.notice_capabilities = {}
        self.platform_task = None

    def warn(self, text):
        if time.monotonic() - self.warned.get(text, -1000) > 60:
            self.warned[text] = time.monotonic()
            self.plugin.logger.warning(text)

    def subscriptions(self):
        p = self.plugin
        if not p.alive or not p.config.get("enabled", False):
            return {}
        try:
            source = p.connections.source()
        except QueryError:
            return {}
        from .migration import legacy_owns

        return {
            s: r
            for s, r in p.enrollment.records.items()
            if r.get("enabled") is True
            and r.get("source") == source
            and r.get("noticeOwner", "unified") == "unified"
            and not legacy_owns(p, s)
            and notices_enabled(r)
            and p.session_native_allowed(s)
        }

    async def load(self):
        value = await self.plugin.get_kv_data("broadcast_state_v2", {})
        if not isinstance(value, dict) or len(value) > 64:
            raise ValueError("invalid notification state")
        normalized = {}
        for session, record in value.items():
            target = group_origin(session)
            if target in normalized and normalized[target] != record:
                # Conflicting enrollments are disabled; retain both journals
                # instead of silently dropping unknown delivery receipts.
                self.saved = value
                return
            normalized[target] = record
        if normalized != value:
            await self.plugin.put_kv_data("broadcast_v2_before_group_normalization", value)
            await self.plugin.put_kv_data("broadcast_state_v2", normalized)
        self.saved = normalized

    async def save(self, session, source, state):
        async with self.storage_lock:
            candidate = copy.deepcopy(self.saved)
            candidate[session] = {"source": source, "state": state}
            if len(json.dumps(candidate, ensure_ascii=False).encode()) > 8 << 20:
                raise QueryError("NOTICE_STATE_LIMIT")
            if await self.plugin.put_kv_data("broadcast_state_v2", candidate) is False:
                raise QueryError("NOTICE_STATE_SAVE_FAILED")
            self.saved = candidate

    async def remember_reference(self, session, source, row):
        """Share the notification journal so its next save keeps query replies."""
        async with self.lock:
            await self._remember_reference(session, source, row)

    async def _remember_reference(self, session, source, row):
        engine = self.engines.get(session)
        if engine:
            async with engine.lock:
                if (self.engines.get(session) is not engine
                        or self.source != self.plugin.connection_identity()
                        or self.plugin.connections.source() != source):
                    return
                engine.state["references"].append(row)
                await engine.persist()
        else:
            saved = self.saved.get(session, {})
            if saved.get("source") != source:
                return
            state = copy.deepcopy(saved.get("state", {}))
            state["references"] = [
                ref for ref in [*state.get("references", []), row]
                if 0 <= time.time() - ref.get("at", 0) <= REFERENCE_TTL
            ][-REFERENCE_LIMIT:]
            await self.save(session, source, state)

    async def read(self, name, args):
        if name not in NOTICE_READ_TOOLS:
            raise QueryError("PERMISSION_DENIED")
        key = (self.source, name, json.dumps(args, sort_keys=True))
        previous = self.cache.get(key)
        if (
            name not in {"api_streamWorldRuntimeEvents", CHAT_STREAM_TOOL}
            and previous
            and time.monotonic() - previous[0] < 2
        ):
            return copy.deepcopy(previous[1])
        task = self.read_tasks.get(key)
        if task is None:
            task = asyncio.create_task(self._read(name, args))
            self.read_tasks[key] = task

            def finish(done):
                if self.read_tasks.get(key) is done:
                    self.read_tasks.pop(key, None)
                if not done.cancelled():
                    done.exception()

            task.add_done_callback(finish)
        try:
            result = await asyncio.shield(task)
            if len(self.cache) >= 128:
                self.cache.pop(next(iter(self.cache)))
            if name not in {"api_streamWorldRuntimeEvents", CHAT_STREAM_TOOL}:
                self.cache[key] = (time.monotonic(), result)
            return copy.deepcopy(result)
        finally:
            if task.done() and self.read_tasks.get(key) is task:
                self.read_tasks.pop(key, None)

    async def _read(self, name, args):
        if name not in NOTICE_READ_TOOLS:
            raise QueryError("PERMISSION_DENIED")
        source = self.source
        if name == SYSTEM_TOOL and not any(e.options.get("software", False) for e in self.engines.values()):
            raise QueryError("PERMISSION_DENIED")
        p = self.plugin
        server = p.connections.broadcast(BASE_TOOLS)
        runtime = p.manager.mcp_server_runtime_view.get(server)
        tool = next((t for t in runtime.client.tools if t.name == name), None) if runtime else None
        if (
            not server
            or tool is None
            or tool.annotations is None
            or tool.annotations.readOnlyHint is not True
        ):
            raise QueryError("NOTICE_CAPABILITY_MISSING")
        async with asyncio.timeout(20), self.read_limit:
            if self.source != p.connection_identity() or name != SYSTEM_TOOL and args.get("path", {}).get("roomId") not in {
                r for e in self.engines.values() for r in e.rooms
            }:
                raise QueryError("ROOM_DENIED")
            from datetime import timedelta

            identity = (server, id(runtime.client.session))
            if identity in self.closed_sessions:
                result = await stateless_read(
                    self.http, p.connections.configuration(server), name, args, NOTICE_READ_TOOLS
                )
            else:
                try:
                    result = await runtime.client.session.call_tool(
                        name, args, read_timeout_seconds=timedelta(seconds=18)
                    )
                except Exception as error:
                    if not closed_transport(error):
                        raise
                    if len(self.closed_sessions) >= 8:
                        self.closed_sessions.clear()
                    self.closed_sessions.add(identity)
                    result = await stateless_read(
                        self.http, p.connections.configuration(server), name, args, NOTICE_READ_TOOLS
                    )
            if source != self.source or source != p.connection_identity():
                raise QueryError("ROOM_DENIED")
            return unpack(result)

    async def reconcile(self):
        async with self.lock:
            await self._reconcile()
        if self.subscriptions() and self.loop_task is None:
            self.loop_task = asyncio.create_task(self.run(), name="dst-notifications")

    async def _reconcile(self):
        p = self.plugin
        self.session_errors = {}
        subscriptions = self.subscriptions()
        if not subscriptions:
            await self.stop_feeds()
            self.engines.clear()
            self.ready.clear()
            self.status = "关闭"
            return
        if len(subscriptions) > self.MAX_SESSIONS:
            await self.stop_feeds()
            self.engines.clear()
            self.status = "订阅群超过 16 个，请先关闭不需要的订阅"
            self.session_errors = dict.fromkeys(subscriptions, self.status)
            return
        try:
            # The reconcile loop runs every 30s. Renew before expiry so a stream
            # heartbeat cannot race the next tick and spuriously lose authority.
            await p.catalog.refresh(min_valid_for=35)
            server = p.connections.broadcast(BASE_TOOLS)
            if not server:
                raise QueryError("NOTICE_CAPABILITY_MISSING")
        except Exception:
            await self.stop_feeds()
            self.engines.clear()
            self.status = "等待同源 MCP 播报权限或房间目录恢复"
            return
        identity = p.connection_identity()
        if identity != self.source:
            await self.stop_feeds()
            self.engines.clear()
            self.ready.clear()
            self.cache.clear()
            self.source = identity
        desired = {}
        for session, record in subscriptions.items():
            config = p.effective(session)
            rooms = {r["id"]: r["name"] for r in p.catalog.rows if r["id"] in config["rooms"]}
            # This implementation bounds total feeds rather than silently
            # selecting a different room by directory position.
            if len(rooms) > self.MAX_ROOMS:
                self.status = "房间超过播报上限，请按名称选择房间"
                self.session_errors[session] = self.status
                continue
            options = {
                **record.get("notices", {}),
                "leave_grace_seconds": p.config.get("advanced", {}).get("leaveGrace", 10),
            }
            sources = {
                "presence": supports_events(self, "api_streamWorldRuntimeEvents"),
                "server": supports_events(self, "api_listRoomRuntimeEvents"),
                "chat": supports_chat_stream(self) or supports_events(self, "api_listRoomChatLogs"),
                **{category: supports_events(self, "api_readRoomNotificationEvents") for category in ("maintenance", "season", "updates", "backup", "players", "boss", "mods", "alerts")},
                "software": supports_events(self, SYSTEM_TOOL),
            }
            missing = [name for name, available in sources.items() if options.get(name, False) and not available]
            known = hasattr(p, "manager")
            if missing and known:
                self.session_errors[session] = "部分播报暂不可用：当前连接未提供所需读取能力；查询继续使用。"
            if known and not any(available and options.get(name, False) for name,available in sources.items()):
                continue
            desired[session] = (rooms, options)
        if len({r for rooms, _ in desired.values() for r in rooms}) > self.MAX_ROOMS:
            await self.stop_feeds()
            self.engines.clear()
            self.status = "订阅房间总数超过 8 个，请缩小范围"
            self.session_errors = dict.fromkeys(subscriptions, self.status)
            return
        changes = {
            s
            for s in self.engines
            if s not in desired
            or self.engines[s].rooms != desired[s][0]
            or self.engines[s].options != desired[s][1]
        }
        if changes:
            await self.stop_feeds()  # No old delivery may race a replacement engine.
            for session in changes:
                self.engines.pop(session, None)
            self.ready = {(s, r) for s, r in self.ready if s not in changes}
        for session, (rooms, options) in desired.items():
            if session in self.engines or not rooms:
                continue
            saved = self.saved.get(session, {})
            source = p.connections.source()
            state = saved.get("state", {}) if saved.get("source") == source else {}

            async def save(value, session=session, source=source):
                await self.save(session, source, value)

            async def emit(text, related, session=session):
                # Every queue item carries a room; check current authority at
                # the last possible moment, even after a key/config change.
                return await p.send_notice(session, text, related)

            self.engines[session] = BroadcastEngine(
                self.read,
                None,
                save,
                rooms,
                options=options,
                state=state,
                log=self.warn,
                emit_notice=emit,
                message_renderer=lambda kind, values, session=session: p.render_message(
                    session, kind, values
                ),
            )
        for session, engine in self.engines.items():
            collection_missing = {category for room in engine.rooms
                for category, state in engine.room(room).get("notice_collection", {}).items()
                if engine.options.get(category, False) and state in {"disabled", "unavailable"}}
            if collection_missing:
                from .event_notices import LABELS
                self.session_errors[session] = "部分播报没有可用采集：" + "、".join(LABELS[c] for c in sorted(collection_missing)) + "。请在面板查看采集设置；插件不会自动开启采集。"
        rooms = {r for e in self.engines.values() for r in e.rooms}
        for room in set(self.tasks) - rooms:
            task = self.tasks.pop(room)
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)
        if rooms and self.http is None:
            self.http = httpx.AsyncClient(
                timeout=httpx.Timeout(connect=10, read=45, write=10, pool=10),
                limits=httpx.Limits(max_connections=34, max_keepalive_connections=2),
                follow_redirects=False,
            )
        for room in rooms - set(self.tasks):
            self.tasks[room] = asyncio.create_task(self.room_feed(room), name="dst-room-events")
        if self.engines and any(e.options.get("software", False) for e in self.engines.values()) and (self.platform_task is None or self.platform_task.done()):
            self.platform_task = asyncio.create_task(platform_feed(self), name="dst-platform-notifications")
        if self.engines:
            self.status = f"已就绪 · {len(rooms)} 个房间 · {len(self.engines)} 个群"

    def receivers(self, room, presence=False, seeded=False):
        return [
            e
            for session, e in self.engines.items()
            if room in e.rooms
            and (not seeded or (session, room) in self.ready)
            and (not presence or e.options.get("presence", True) or e.options.get("season", False) or e.options.get("mods", False))
        ]

    async def room_feed(self, room):
        children = {}
        chat_task = asyncio.create_task(self.chat_feed(room), name="dst-room-chat")
        facts_task = asyncio.create_task(facts_feed(self, room), name="dst-room-notifications")
        advanced = self.plugin.config.get("advanced", {})
        interval = max(15, min(60, int(advanced.get("pollInterval", 30))))
        try:
            last_seed = 0
            while True:
                try:
                    engines = self.receivers(room)
                    unseeded = any(
                        (s, room) not in self.ready
                        for s, e in self.engines.items()
                        if room in e.rooms
                    )
                    if time.monotonic() - last_seed >= 120 or not last_seed or unseeded:
                        worlds = set()
                        for engine in engines:
                            worlds.update(await engine.seed(room))
                            self.ready.update(
                                (s, room) for s, e in self.engines.items() if e is engine
                            )
                        if len(worlds) > self.MAX_WORLDS:
                            raise QueryError("NOTICE_WORLD_LIMIT")
                        desired = worlds if self.receivers(room, True) and supports_events(self, "api_streamWorldRuntimeEvents") else set()
                        for world in set(children) - desired:
                            task = children.pop(world)
                            task.cancel()
                            await asyncio.gather(task, return_exceptions=True)
                        for world in desired - set(children):
                            children[world] = asyncio.create_task(self.stream(room, world))
                        last_seed = time.monotonic()
                    if facts_task.done():
                        await asyncio.gather(facts_task, return_exceptions=True)
                        facts_task = asyncio.create_task(facts_feed(self, room))
                    if any(e.options.get("season", False) or e.options.get("maintenance", False) for e in engines):
                        await self.read("api_listWorldStates", {"path": {"roomId": room}})
                    for engine in engines:
                        if engine.options.get("server", True) and supports_events(self, "api_listRoomRuntimeEvents") and not (engine.options.get("maintenance", False) and supports_events(self, "api_readRoomNotificationEvents")):
                            await engine.poll_audit(room)
                            self.wakeup.set()
                except asyncio.CancelledError:
                    raise
                except Exception as error:
                    self.warn(f"播报来源暂不可用（{type(error).__name__}），不据此判定崩服")
                await asyncio.sleep(interval)
        finally:
            chat_task.cancel()
            facts_task.cancel()
            for task in children.values():
                task.cancel()
            await asyncio.gather(chat_task, facts_task, *children.values(), return_exceptions=True)

    async def chat_feed(self, room):
        """Prefer one durable room stream; retain bounded polling for older panels."""
        advanced = self.plugin.config.get("advanced", {})
        interval = max(2, min(30, int(advanced.get("chatPollInterval", 3))))
        failures = 0
        while True:
            engines = [e for e in self.receivers(room) if e.options.get("chat", False)]
            if not engines:
                return
            if supports_chat_stream(self):
                try:
                    await chat_stream_session(self, room, engines)
                    failures = 0
                    await asyncio.sleep(0.5)
                    continue
                except asyncio.CancelledError:
                    raise
                except Exception as error:
                    failures = min(4, failures + 1)
                    self.warn(f"聊天推送暂不可用（{type(error).__name__}），保留续读位置重连")
                    if failures < 3:
                        await asyncio.sleep(min(30, 2**failures))
                        continue
                    # Recover recent public messages during a sustained transport
                    # outage. Keep the stream cursor and dedup IDs for reconnection.
            started = time.monotonic()
            failed = False
            for engine in engines:
                try:
                    await engine.poll_chat(room)
                    self.wakeup.set()  # Wake only after the outbox has persisted.
                except asyncio.CancelledError:
                    raise
                except Exception as error:
                    failed = True
                    self.warn(f"公屏读取暂不可用（{type(error).__name__}），保留去重记录并退避")
            failures = min(4, failures + 1) if failed else 0
            pause = min(60, interval * 2**failures)
            await asyncio.sleep(max(0.05, pause - (time.monotonic() - started)))

    def start_presence(self, session, engine):
        if session in self.presence_tasks or not any(
            event["due"] <= engine.clock()
            for room in engine.rooms
            for event in engine.room(room)["pending"].values()
        ):
            return
        task = asyncio.create_task(engine.resolve_presence(), name="dst-presence-details")
        self.presence_tasks[session] = task

        def finish(done):
            if self.presence_tasks.get(session) is done:
                self.presence_tasks.pop(session, None)
            if not done.cancelled() and done.exception():
                self.warn("进退服详情暂未读取成功；保留待确认事件")
            self.wakeup.set()

        task.add_done_callback(finish)

    async def stream(self, room, world):
        delay = 2
        while True:
            try:
                engines = self.receivers(room, True, seeded=True)
                args = {"path": {"roomId": room, "worldId": world}}
                # Reuse a common cursor only when every receiver agrees.
                # Different histories must resynchronize instead of guessing
                # an ordering for opaque cursors.
                cursors = [e.state["cursors"].get(room + "/" + world) for e in engines]
                if cursors and all(c == cursors[0] and c for c in cursors):
                    args["query"] = {"cursor": cursors[0]}
                data = await self.read("api_streamWorldRuntimeEvents", args)
                server = self.plugin.connections.broadcast(BASE_TOOLS)
                url, headers = resource_request(
                    self.plugin.connections.configuration(server), data.get("resource", {})
                )
                async with self.http.stream("GET", url, headers=headers) as response:
                    if (
                        response.status_code != 200
                        or "text/event-stream" not in response.headers.get("content-type", "")
                    ):
                        raise MCPReadError("event stream unavailable")
                    async for kind, event_id, payload in sse_frames(response.aiter_lines()):
                        if not isinstance(payload, dict) or kind == "runtime.error":
                            raise MCPReadError("event stream error")
                        if (
                            self.source != self.plugin.connection_identity()
                            or not self.plugin.catalog.valid()
                        ):
                            raise QueryError("CONFIG_CHANGED")
                        for engine in self.receivers(room, True, seeded=True):
                            await engine.accept(room, world, kind, payload, event_id)
                            if kind in {"runtime.gap", "runtime.reset"}:
                                await engine.seed(room)
                        delay = 2
            except asyncio.CancelledError:
                raise
            except Exception as error:
                self.warn(f"事件流暂不可用（{type(error).__name__}）；保留游标并退避重连")
            await asyncio.sleep(delay)
            delay = min(30, delay * 2)

    async def run(self):
        try:
            next_refresh = time.monotonic() + 30
            while self.plugin.alive and self.subscriptions():
                self.wakeup.clear()
                if time.monotonic() >= next_refresh:
                    await self.reconcile()
                    next_refresh = time.monotonic() + 30
                for session, engine in list(self.engines.items()):
                    async with self.lock:
                        if self.engines.get(session) is not engine:
                            continue
                        try:
                            await engine.deliver_one()
                            self.start_presence(session, engine)
                        except Exception as error:
                            self.warn(f"通知处理暂不可用（{type(error).__name__}）")
                # One delivery per group per turn, with a short burst bound.
                # Presence enrichment runs separately so slow detail reads
                # cannot hold up already persisted public chat notifications.
                await asyncio.sleep(0.5)
                try:
                    await asyncio.wait_for(self.wakeup.wait(), timeout=0.5)
                except TimeoutError:
                    pass
        finally:
            self.loop_task = None
            if not self.subscriptions():
                async with self.lock:
                    await self.stop_feeds()
                    self.engines.clear()

    async def stop_feeds(self):
        tasks = (
            list(self.tasks.values())
            + list(self.read_tasks.values())
            + list(self.presence_tasks.values())
            + ([self.platform_task] if self.platform_task else [])
        )
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        self.tasks.clear()
        self.read_tasks.clear()
        self.presence_tasks.clear()
        self.chat_resources.clear()
        self.event_resources.clear()
        self.notice_capabilities.clear()
        self.platform_task = None
        if self.http is not None:
            await self.http.aclose()
            self.http = None

    async def close(self):
        if self.loop_task:
            self.loop_task.cancel()
            await asyncio.gather(self.loop_task, return_exceptions=True)
        await self.stop_feeds()
        self.engines.clear()
