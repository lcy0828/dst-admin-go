"""Unified onboarding, permission changes and notification ownership, with no external I/O."""

import asyncio
import copy
import importlib.util
import json
import sys
import unittest
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock, patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from astrbot_plugin_dst_companion.broadcast import READ_TOOLS, BroadcastEngine
from astrbot_plugin_dst_companion.configuration import Configuration, migrate_v3
from astrbot_plugin_dst_companion.connections import DISCOVERY_TOOLS, Connections
from astrbot_plugin_dst_companion.gateway import QueryError
from astrbot_plugin_dst_companion.onboarding import Enrollment, RoomCatalog
from astrbot_plugin_dst_companion.sessions import canonical_records, group_origin
from astrbot_plugin_dst_companion.streams import MCPReadError, resource_request, sse_frames

AVAILABLE = importlib.util.find_spec("astrbot") is not None
if AVAILABLE:
    import test_astrbot_adapter as adapters
    from astrbot.api import message_components as C
    from astrbot.core.config.astrbot_config import AstrBotConfig
    from test_companion import mcp

    from astrbot_plugin_dst_companion.main import DSTCompanion
    from astrbot_plugin_dst_companion.migration import migrate_broadcast


class CoreTests(unittest.IsolatedAsyncioTestCase):
    async def test_isolated_group_origins_migrate_without_combining_conflicting_permissions(self):
        original = {
            "adapter:GroupMessage:123_789": {
                "enabled": True,
                "query": True,
                "source": "panel",
                "rooms": "all",
            }
        }
        save = AsyncMock()
        enrollment = Enrollment(save)
        await enrollment.load(AsyncMock(return_value=original))
        self.assertEqual(
            enrollment.records,
            {"adapter:GroupMessage:789": original["adapter:GroupMessage:123_789"]},
        )
        self.assertEqual(save.await_count, 2)
        conflict = {**original, "adapter:GroupMessage:456_789": {"enabled": False}}
        self.assertEqual(
            canonical_records(conflict), {"adapter:GroupMessage:789": {"enabled": False}}
        )
        self.assertEqual(
            group_origin("adapter:FriendMessage:123_789"), "adapter:FriendMessage:123_789"
        )
        self.assertEqual(
            group_origin("adapter:GroupMessage:123_789_bad"), "adapter:GroupMessage:123_789_bad"
        )

    async def test_discovery_coalesces_and_does_not_send_unsupported_pagination(self):
        read = AsyncMock(return_value={"items": [{"id": "r", "name": "营地"}]})
        catalog = RoomCatalog(read, lambda: "a")
        results = await asyncio.gather(*(catalog.refresh() for _ in range(8)))
        read.assert_awaited_once_with("api_listRooms", {})
        self.assertEqual(results[0], [{"id": "r", "name": "营地"}])

    async def test_incomplete_duplicate_or_unbounded_catalog_is_rejected(self):
        for value in (
            {"items": [], "hasMore": True},
            {"items": [], "total": 1},
            {"items": [{"id": "r"}] * 2},
            {"items": [{"id": str(i)} for i in range(257)]},
            {"items": [], "_aiTruncated": True},
        ):
            catalog = RoomCatalog(AsyncMock(return_value=value), lambda: "a")
            with self.assertRaises(QueryError):
                await catalog.refresh()
            self.assertFalse(catalog.valid())

    async def test_scope_expiration_and_source_change_never_extend_old_authority(self):
        clock, identity = [0], ["a"]
        read = AsyncMock(return_value={"items": [{"id": "r"}]})
        catalog = RoomCatalog(read, lambda: identity[0], clock=lambda: clock[0])
        await catalog.refresh()
        clock[0] = 61
        read.side_effect = TimeoutError()
        with self.assertRaises(TimeoutError):
            await catalog.refresh()
        self.assertFalse(catalog.valid())
        clock[0], identity[0] = 0, "b"
        self.assertFalse(catalog.valid())

    async def test_connection_change_during_discovery_does_not_publish_old_rooms(self):
        identity = ["first"]

        async def read(*args):
            identity[0] = "second"
            return {"items": [{"id": "old-room"}]}

        catalog = RoomCatalog(read, lambda: identity[0])
        with self.assertRaises(QueryError):
            await catalog.refresh()
        self.assertEqual(catalog.rows, [])
        self.assertFalse(catalog.valid())

    async def test_enrollment_only_grants_after_successful_persistence(self):
        save = AsyncMock(side_effect=OSError())
        enrollment = Enrollment(save)
        with self.assertRaises(OSError):
            await enrollment.update("adapter:GroupMessage:123", {"enabled": False})
        self.assertEqual(enrollment.records, {})
        with self.assertRaises(ValueError):
            await enrollment.update("adapter:GroupMessage:123", {"enabled": "true"})

    def runtime(self, tools, url="https://panel.example/mcp?view=bot"):
        return NS(
            client=NS(
                session=NS(), tools=[NS(name=n) for n in tools], _mcp_server_config={"url": url}
            )
        )

    async def test_connections_require_selection_and_never_use_other_panel_for_broadcast(self):
        manager = NS(
            mcp_server_runtime_view={
                "read": self.runtime(DISCOVERY_TOOLS),
                "other": self.runtime(DISCOVERY_TOOLS | READ_TOOLS, "https://other.example/mcp"),
            }
        )
        connections = Connections(manager, {})
        with self.assertRaises(QueryError) as e:
            connections.read()
        self.assertEqual(e.exception.code, "MCP_SELECTION_REQUIRED")
        connections.config["mcp_server"] = "read"
        self.assertEqual(connections.broadcast(READ_TOOLS), "")
        manager.mcp_server_runtime_view["notices"] = self.runtime(READ_TOOLS)
        self.assertEqual(connections.broadcast(READ_TOOLS), "notices")
        manager.mcp_server_runtime_view["duplicate"] = self.runtime(READ_TOOLS)
        self.assertEqual(connections.broadcast(READ_TOOLS), "")
        connections.config["management_mcp_server"] = "notices"
        self.assertEqual(connections.broadcast(READ_TOOLS), "notices")

    async def test_auto_connection_is_pinned_across_disconnect_not_switched(self):
        manager = NS(mcp_server_runtime_view={"one": self.runtime(DISCOVERY_TOOLS)})
        connections = Connections(manager, {})
        self.assertEqual(connections.read(), "one")
        manager.mcp_server_runtime_view["two"] = self.runtime(DISCOVERY_TOOLS)
        manager.mcp_server_runtime_view["one"].client.session = None
        self.assertEqual(connections.read(), "one")

    async def test_resources_support_prefix_and_refuse_foreign_credentials_redirects(self):
        config = {
            "url": "https://panel.example/dst/mcp?view=bot",
            "headers": {"Authorization": "Bearer EXAMPLE"},
        }
        resource = {"kind": "stream", "url": "/dst/api/v2/system/ai/resources/example"}
        url, headers = resource_request(config, resource)
        self.assertTrue(url.startswith("https://panel.example/dst/"))
        self.assertEqual(headers["Accept"], "text/event-stream")
        for bad in (
            "https://other.example/dst/api/v2/system/ai/resources/example",
            "/api/v2/system/ai/resources/example",
            "/dst/api/v2/system/ai/resources/../other",
            "https://:EXAMPLE@panel.example/dst/api/v2/system/ai/resources/example",
        ):
            with self.assertRaises(MCPReadError):
                resource_request(config, {**resource, "url": bad})

    async def test_incomplete_sse_never_advances_cursor(self):
        async def lines():
            for line in ["id: old", 'data: {"n":1}', "", "id: incomplete", 'data: {"n":2}']:
                yield line

        frames = [f async for f in sse_frames(lines())]
        self.assertEqual(frames, [("message", "old", {"n": 1})])

    async def test_legacy_configuration_migration_preserves_restrictions(self):
        old = {
            "configuration_version": 2,
            "defaults": {
                "mode": "shortcut",
                "rooms": ["r1"],
                "categories": ["status"],
                "writeAllowlist": [],
            },
            "allowed_sessions": ["group"],
        }
        new = migrate_v3(old)
        effective = Configuration(new).effective("group")
        self.assertEqual(effective["mode"], "shortcut")
        self.assertEqual(effective["rooms"], ("r1",))
        self.assertEqual(effective["categories"], ("status",))
        self.assertEqual(old["configuration_version"], 2)
        with self.assertRaises(ValueError):
            Configuration({"configuration_version": 3, "advanced": {"pollInterval": 0}}).effective(
                "s"
            )


class SavedConfig(dict):
    async def save_config_async(self):
        return True


@unittest.skipUnless(AVAILABLE, "requires AstrBot 4.29.3")
class UnifiedAdapterTests(unittest.IsolatedAsyncioTestCase):
    def event(self, text, admin=True, session="adapter:GroupMessage:789"):
        event = adapters.AdapterTests.event(self, [C.Plain(text)], admin=admin)
        event.unified_msg_origin = session
        event.send = AsyncMock(return_value=True)
        return event

    async def asyncSetUp(self):
        schema = json.loads(
            (
                Path(__file__).resolve().parents[1]
                / "astrbot_plugin_dst_companion/_conf_schema.json"
            ).read_text()
        )
        config = SavedConfig(AstrBotConfig._config_schema_to_default_config(None, schema))
        self.rooms = [{"id": "r1", "name": "1服"}, {"id": "r2", "name": "2服"}]
        self.calls = []

        async def call(name, arguments, **kw):
            self.calls.append((name, arguments))
            if name == "api_listRooms":
                return mcp({"items": self.rooms})
            return mcp({"items": []})

        runtime = NS(
            client=NS(
                session=NS(call_tool=AsyncMock(side_effect=call)),
                tools=[NS(name=n, annotations=NS(readOnlyHint=True)) for n in DISCOVERY_TOOLS],
                _mcp_server_config={"url": "https://panel.example/mcp?view=bot", "headers": {}},
            )
        )
        self.manager = NS(
            mcp_server_runtime_view={"panel": runtime},
            _check_tool_permission=AsyncMock(return_value=None),
        )
        self.context = NS(
            get_llm_tool_manager=lambda: self.manager,
            get_config=lambda **kw: {},
            get_registered_star=lambda name: None,
        )
        self.bot = DSTCompanion(self.context, config)
        self.storage = {}

        async def put(key, value):
            self.storage[key] = copy.deepcopy(value)

        self.bot.get_kv_data = AsyncMock(
            side_effect=lambda key, default: copy.deepcopy(self.storage.get(key, default))
        )
        self.bot.put_kv_data = AsyncMock(side_effect=put)
        self.bot.enrollment.save = self.bot.put_kv_data
        await self.bot.initialize()

    async def asyncTearDown(self):
        await self.bot.terminate()

    def output(self, event):
        return "\n".join(str(call.args[0]) for call in event.send.call_args_list)

    async def enable(self):
        event = self.event("/dst 启用本群")
        await self.bot.on_message(event)
        self.assertIn("已启用本群", self.output(event))
        return event

    async def test_new_install_has_zero_network_or_background_tasks(self):
        self.assertEqual(self.calls, [])
        self.assertIsNone(self.bot.notifications.loop_task)
        self.assertEqual(self.bot.notifications.tasks, {})
        self.assertFalse(self.bot.allowed("adapter:GroupMessage:789"))

    async def test_one_click_discovers_rooms_and_group_without_ids_and_is_idempotent(self):
        event = await self.enable()
        self.assertEqual(self.bot.actor(event).rooms, ("r1", "r2"))
        self.assertEqual(self.bot.actor(event).role, "game-read")
        self.assertTrue(self.bot.enrollment.records[event.unified_msg_origin]["rooms"] == "all")
        self.assertIn("自动同步", self.output(event))
        self.assertIn("暂不可用", self.output(event))
        await self.enable()
        self.assertEqual(len(self.bot.enrollment.records), 1)
        self.assertEqual(len(self.calls), 1)
        self.assertFalse(self.bot.allowed("adapter:GroupMessage:other"))

    async def test_isolated_member_session_enrolls_the_group_and_routes_notices_to_group(self):
        event = self.event("/dst 启用本群 聊天播报 开", session="adapter:GroupMessage:123_789")
        await self.bot.on_message(event)
        self.assertIn("已启用本群", self.output(event))
        self.assertIn("adapter:GroupMessage:789", self.bot.enrollment.records)
        self.assertNotIn("adapter:GroupMessage:123_789", self.bot.enrollment.records)
        member = self.event("菜单", admin=False, session="adapter:GroupMessage:456_789")
        self.assertIsNotNone(self.bot.actor(member))
        self.assertFalse(self.bot.allowed("adapter:GroupMessage:456_999"))
        sender = AsyncMock(side_effect=[{"user_id": "456"}, {"message_id": 987}])
        self.context.get_platform_inst = lambda _: NS(get_client=lambda: NS(call_action=sender))
        result = await self.bot.send_notice(
            str(event.unified_msg_origin), "合成消息", {"roomId": "r1", "kind": "chat"}
        )
        self.assertTrue(result)
        self.assertEqual(sender.call_args.args[0], "send_group_msg")
        self.assertEqual(sender.call_args.kwargs["group_id"], 789)
        self.assertEqual(self.bot.notifications.delivery_errors, {})

    async def test_group_whitelist_accepts_isolated_members_but_user_exemption_does_not_grant_group(
        self,
    ):
        self.context.get_config = lambda **kw: {
            "platform_settings": {
                "enable_id_white_list": True,
                "id_whitelist": ["adapter:GroupMessage:123_789"],
            }
        }
        await self.bot.on_message(
            event := self.event("/dst 启用本群", session="adapter:GroupMessage:123_789")
        )
        self.assertNotIn("已启用本群", self.output(event))
        self.context.get_config = lambda **kw: {
            "platform_settings": {"enable_id_white_list": True, "id_whitelist": ["789"]}
        }
        await self.bot.on_message(
            event := self.event("/dst 启用本群", session="adapter:GroupMessage:123_789")
        )
        self.assertIn("已启用本群", self.output(event))
        self.assertTrue(self.bot.allowed("adapter:GroupMessage:456_789"))
        self.assertFalse(self.bot.allowed("adapter:GroupMessage:456_999"))

    async def test_isolated_notification_journal_keeps_seen_ids_and_delivery_outcomes(self):
        original = {
            "adapter:GroupMessage:123_789": {
                "source": "panel",
                "state": {
                    "rooms": {"r1": {"chat_seen": ["event-1"], "chat_seeded": True}},
                    "deliveries": [{"key": "event-1", "status": "unknown"}],
                },
            }
        }
        self.storage["broadcast_state_v2"] = copy.deepcopy(original)
        await self.bot.notifications.load()
        self.assertEqual(
            self.bot.notifications.saved["adapter:GroupMessage:789"],
            original["adapter:GroupMessage:123_789"],
        )
        self.assertEqual(self.storage["broadcast_v2_before_group_normalization"], original)

    async def test_failed_delivery_is_explained_in_group_settings_without_sending_or_retrying(self):
        event = await self.enable()
        self.manager.mcp_server_runtime_view["panel"].client.tools = [
            NS(name=n, annotations=NS(readOnlyHint=True)) for n in DISCOVERY_TOOLS | READ_TOOLS
        ]
        await self.bot.catalog.refresh(force=True)
        self.context.get_platform_inst = lambda _: None
        result = await self.bot.send_notice(
            event.unified_msg_origin, "合成消息", {"roomId": "r1", "kind": "player"}
        )
        self.assertIsNone(result)
        settings = self.event("/dst 设置")
        await self.bot.on_message(settings)
        self.assertIn("当前 QQ 平台尚未连接", self.output(settings))

    async def test_native_admin_required_and_failed_save_never_enables_group(self):
        event = self.event("/dst 启用本群", admin=False)
        await self.bot.on_message(event)
        self.assertEqual(self.calls, [])
        self.assertIn("仅允许", self.output(event))
        self.bot.enrollment.save = AsyncMock(side_effect=OSError())
        event = self.event("/dst 启用本群")
        await self.bot.on_message(event)
        self.assertFalse(self.bot.allowed(event.unified_msg_origin))
        self.assertNotIn("已启用本群", self.output(event))

    async def test_native_group_whitelist_blocks_enrollment(self):
        self.context.get_config = lambda **kw: {
            "platform_settings": {"enable_id_white_list": True, "id_whitelist": ["another"]}
        }
        event = self.event("/dst 启用本群")
        await self.bot.on_message(event)
        self.assertEqual(self.calls, [])
        self.assertFalse(self.bot.allowed(event.unified_msg_origin))

    async def test_room_names_selection_and_revocation_recheck_old_actor(self):
        event = await self.enable()
        original = self.bot.actor(event)
        self.rooms = [{"id": "r2", "name": "新名字"}, {"id": "r3", "name": "3服"}]
        await self.bot.catalog.refresh(force=True)
        self.assertEqual(self.bot.actor(event).rooms, ("r2", "r3"))
        self.assertFalse(await self.bot.native(original, "api_listRooms"))
        choose = self.event("/dst 订阅房间 新名字")
        await self.bot.on_message(choose)
        self.assertEqual(self.bot.actor(event).rooms, ("r2",))
        # New rooms never change an explicitly selected range.
        self.rooms.append({"id": "r4", "name": "4服"})
        await self.bot.catalog.refresh(force=True)
        self.assertEqual(self.bot.actor(event).rooms, ("r2",))

    async def test_disabled_group_never_sends_queued_notices(self):
        event = await self.enable()
        await self.bot.on_message(self.event("/dst 禁用本群"))
        self.assertIsNone(self.bot.actor(event))
        self.assertIsNone(
            await self.bot.send_notice(
                event.unified_msg_origin, "旧消息", {"roomId": "r1", "kind": "player"}
            )
        )
        self.assertEqual(self.bot.notifications.tasks, {})

    async def test_legacy_room_ceiling_is_preserved_when_enabling_existing_group(self):
        self.bot.config.update(
            configuration_version=1, room_ids=["r1"], allowed_sessions=["adapter:GroupMessage:789"]
        )
        event = await self.enable()
        self.assertEqual(self.bot.actor(event).rooms, ("r1",))

    async def test_rebinding_connection_never_moves_existing_groups_silently(self):
        event = await self.enable()
        runtime = copy.copy(self.manager.mcp_server_runtime_view["panel"])
        runtime.client = copy.copy(runtime.client)
        runtime.client._mcp_server_config = {"url": "https://other.example/mcp"}
        self.manager.mcp_server_runtime_view["other"] = runtime
        await self.bot.on_message(self.event("/dst 使用连接 other"))
        self.assertFalse(self.bot.allowed(event.unified_msg_origin))
        self.assertFalse(self.bot.notice_allowed(event.unified_msg_origin, {"roomId": "r1"}))

    async def test_scope_expiry_does_not_report_empty_rooms_as_a_valid_snapshot(self):
        event = await self.enable()
        self.bot.catalog.until = 0
        self.assertIsNone(self.bot.actor(event))
        self.assertFalse(self.bot.notice_allowed(event.unified_msg_origin, {"roomId": "r1"}))
        self.assertTrue(await self.bot.prepare(event))
        self.assertEqual(self.bot.actor(event).rooms, ("r1", "r2"))

    async def test_two_groups_share_one_room_feed_and_source_read(self):
        self.manager.mcp_server_runtime_view["panel"].client.tools = [
            NS(name=n, annotations=NS(readOnlyHint=True)) for n in (DISCOVERY_TOOLS | READ_TOOLS)
        ]
        # No real socket, event stream, model or QQ send is used.
        gate = asyncio.Event()

        async def feed(room):
            await gate.wait()

        with patch.object(self.bot.notifications, "room_feed", side_effect=feed):
            await self.enable()
            await self.bot.on_message(
                self.event("/dst 启用本群", session="adapter:GroupMessage:987")
            )
            self.assertEqual(len(self.bot.notifications.engines), 2)
            self.assertEqual(set(self.bot.notifications.tasks), {"r1", "r2"})
            args = {"path": {"roomId": "r1"}}
            before = len(self.calls)
            await asyncio.gather(
                *(self.bot.notifications.read("api_listRoomWorlds", args) for _ in range(5))
            )
            self.assertEqual(len(self.calls) - before, 1)
            with self.assertRaises(QueryError):
                await self.bot.notifications.read("api_executeRoomCommand", {})

    async def test_subscription_limit_reports_error_instead_of_selecting_first_groups(self):
        event = await self.enable()
        record = self.bot.enrollment.records[event.unified_msg_origin]
        for i in range(17):
            self.bot.enrollment.records[f"adapter:GroupMessage:{1000 + i}"] = copy.deepcopy(record)
        await self.bot.notifications.reconcile()
        self.assertEqual(self.bot.notifications.engines, {})
        self.assertEqual(self.bot.notifications.tasks, {})
        self.assertIn("超过 16", self.bot.notifications.status)

    async def test_notices_have_one_sender_and_do_not_retry_uncertain_delivery(self):
        event = await self.enable()
        sender = AsyncMock(side_effect=[{"user_id": "456"}, TimeoutError()])
        self.context.get_platform_inst = lambda _: NS(get_client=lambda: NS(call_action=sender))
        save = AsyncMock()
        engine = BroadcastEngine(
            AsyncMock(),
            None,
            save,
            {"r1": "1服"},
            emit_notice=lambda text, related: self.bot.send_notice(
                event.unified_msg_origin, text, related
            ),
        )
        engine.queue("r1/event", "例子", {"roomId": "r1", "kind": "player"})
        await engine.deliver_one()
        await engine.deliver_one()
        self.assertEqual(sender.await_count, 2)  # Login metadata + exactly one send.
        self.assertEqual(engine.state["deliveries"][-1]["status"], "unknown")

    async def test_missing_send_receipt_is_unknown_and_never_retried(self):
        event = await self.enable()
        sender = AsyncMock(side_effect=[{"user_id": "456"}, {}])
        self.context.get_platform_inst = lambda _: NS(get_client=lambda: NS(call_action=sender))
        engine = BroadcastEngine(
            AsyncMock(),
            None,
            AsyncMock(),
            {"r1": "1服"},
            emit_notice=lambda text, related: self.bot.send_notice(
                event.unified_msg_origin, text, related
            ),
        )
        engine.queue("r1/event", "例子", {"roomId": "r1", "kind": "player"})
        await engine.deliver_one()
        await engine.deliver_one()
        self.assertEqual(sender.await_count, 2)
        self.assertEqual(engine.state["deliveries"][-1]["status"], "unknown")

    async def test_framework_session_disable_blocks_background_send(self):
        event = await self.enable()
        with patch(
            "astrbot.core.star.session_plugin_manager.SessionPluginManager.is_plugin_enabled_for_session",
            AsyncMock(return_value=False),
        ):
            result = await self.bot.send_notice(
                event.unified_msg_origin, "例子", {"roomId": "r1", "kind": "player"}
            )
        self.assertIsNone(result)
        self.assertEqual(self.bot.bot_ids, {})

    async def test_reconcile_waits_for_old_delivery_before_replacing_its_queue(self):
        event = await self.enable()
        hub = self.bot.notifications
        if hub.loop_task:
            hub.loop_task.cancel()
            await asyncio.gather(hub.loop_task, return_exceptions=True)
        self.manager.mcp_server_runtime_view["panel"].client.tools = [
            NS(name=n, annotations=NS(readOnlyHint=True)) for n in DISCOVERY_TOOLS | READ_TOOLS
        ]
        await self.bot.catalog.refresh(force=True)
        entered, release = asyncio.Event(), asyncio.Event()

        async def delivery():
            entered.set()
            await release.wait()

        original = BroadcastEngine(AsyncMock(), None, AsyncMock(), {"r1": "1服", "r2": "2服"})
        original.resolve_presence = AsyncMock()
        original.deliver_one = delivery
        hub.source = self.bot.connection_identity()
        hub.engines[event.unified_msg_origin] = original
        runner = asyncio.create_task(hub.run())
        await asyncio.wait_for(entered.wait(), 1)
        gate = asyncio.Event()

        async def feed(room):
            await gate.wait()

        try:
            with patch.object(hub, "room_feed", side_effect=feed):
                change = asyncio.create_task(self.bot.on_message(self.event("/dst 订阅房间 1服")))
                await asyncio.sleep(0)
                self.assertFalse(change.done())
                self.assertIs(hub.engines[event.unified_msg_origin], original)
                release.set()
                await asyncio.wait_for(change, 2)
                self.assertIsNot(hub.engines[event.unified_msg_origin], original)
        finally:
            release.set()
            runner.cancel()
            await asyncio.gather(runner, return_exceptions=True)

    async def test_live_stream_waits_for_new_groups_presence_baseline(self):
        event = await self.enable()
        hub = self.bot.notifications
        self.manager.mcp_server_runtime_view["panel"].client.tools = [
            NS(name=n, annotations=NS(readOnlyHint=True)) for n in DISCOVERY_TOOLS | READ_TOOLS
        ]
        await self.bot.catalog.refresh(force=True)
        hub.source = self.bot.connection_identity()
        engine = BroadcastEngine(AsyncMock(), None, AsyncMock(), {"r1": "1服"})
        engine.accept = AsyncMock()
        hub.engines[event.unified_msg_origin] = engine
        hub.read = AsyncMock(
            return_value={
                "resource": {"kind": "stream", "url": "/api/v2/system/ai/resources/example"}
            }
        )

        class Response:
            status_code = 200
            headers = {"content-type": "text/event-stream"}

            async def __aenter__(self):
                return self

            async def __aexit__(self, *args):
                return False

            async def aiter_lines(self):
                for line in [
                    "event: runtime.event",
                    "id: cursor",
                    'data: {"event":{"kind":"player.joined"}}',
                    "",
                ]:
                    yield line
                raise asyncio.CancelledError()

        class HTTP:
            def stream(self, *args, **kw):
                return Response()

            async def aclose(self):
                pass

        hub.http = HTTP()
        with self.assertRaises(asyncio.CancelledError):
            await hub.stream("r1", "world")
        engine.accept.assert_not_awaited()
        hub.ready.add((event.unified_msg_origin, "r1"))
        with self.assertRaises(asyncio.CancelledError):
            await hub.stream("r1", "world")
        engine.accept.assert_awaited_once()

    async def legacy(self):
        config = SavedConfig(
            enabled=True,
            mcp_server="panel",
            platform_id="adapter",
            target_group_id="789",
            room_id="r1",
            presence=True,
            server=True,
            chat=False,
        )
        state = {
            "version": 1,
            "cursors": {"r1/world": "cursor-1"},
            "deliveries": [{"key": "old", "status": "sending"}],
            "outbox": [],
        }
        legacy = NS(
            config=config,
            get_kv_data=AsyncMock(return_value=state),
            terminate=AsyncMock(),
            initialize=AsyncMock(),
        )
        metadata = NS(name="astrbot_plugin_dst_broadcast", star_cls=legacy)
        self.context.get_registered_star = lambda _: metadata
        self.manager.mcp_server_runtime_view["panel"].client.tools = [
            NS(name=n, annotations=NS(readOnlyHint=True)) for n in DISCOVERY_TOOLS | READ_TOOLS
        ]
        return legacy, metadata

    async def test_migration_preserves_cursors_unknown_delivery_and_notification_only_scope(self):
        legacy, metadata = await self.legacy()
        await migrate_broadcast(self.bot, metadata)
        legacy.terminate.assert_awaited_once()
        self.assertFalse(legacy.config["enabled"])
        record = self.bot.enrollment.records["adapter:GroupMessage:789"]
        self.assertEqual(record["noticeOwner"], "unified")
        self.assertFalse(record["query"])
        self.assertFalse(self.bot.allowed("adapter:GroupMessage:789"))
        saved = self.bot.notifications.saved["adapter:GroupMessage:789"]["state"]
        engine = BroadcastEngine(AsyncMock(), AsyncMock(), AsyncMock(), {"r1": "1服"}, state=saved)
        self.assertEqual(engine.state["cursors"]["r1/world"], "cursor-1")
        self.assertEqual(engine.state["deliveries"][0]["status"], "unknown")
        # Repeated hook does not take ownership twice.
        await migrate_broadcast(self.bot, metadata)
        self.assertEqual(legacy.terminate.await_count, 1)

    async def test_other_panel_legacy_notice_cannot_create_a_false_player_reference(self):
        event = await self.enable()
        legacy, _ = await self.legacy()
        legacy.config["mcp_server"] = "foreign"
        foreign = copy.copy(self.manager.mcp_server_runtime_view["panel"])
        foreign.client = copy.copy(foreign.client)
        foreign.client._mcp_server_config = {"url": "https://other.example/mcp"}
        self.manager.mcp_server_runtime_view["foreign"] = foreign
        await self.bot.catalog.refresh(force=True)
        result = await self.bot.send_notice(
            event.unified_msg_origin,
            "foreign example",
            {"roomId": "r1", "playerId": "KU_example", "kind": "player"},
        )
        self.assertIsNone(result)
        self.assertEqual(self.bot.bot_ids, {})

    async def test_failed_legacy_disable_cannot_create_second_delivery_owner(self):
        legacy, metadata = await self.legacy()
        legacy.config.save_config_async = AsyncMock(return_value=False)
        await migrate_broadcast(self.bot, metadata)
        record = self.bot.enrollment.records["adapter:GroupMessage:789"]
        self.assertEqual(record["noticeOwner"], "pending")
        self.assertEqual(self.bot.notifications.subscriptions(), {})
        self.assertEqual(self.bot.notifications.tasks, {})

    async def test_late_loaded_legacy_plugin_is_migrated_by_framework_hook(self):
        legacy, metadata = await self.legacy()
        with patch.object(self.bot.notifications, "reconcile", AsyncMock()):
            await self.bot.on_plugin_loaded(metadata)
        self.assertFalse(legacy.config["enabled"])
        self.assertIn("broadcast_state_v2", self.storage)


if __name__ == "__main__":
    unittest.main()
