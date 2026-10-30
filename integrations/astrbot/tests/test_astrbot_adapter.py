"""Run in AstrBot's Python image. No models, QQ sends or panel mutations."""

import importlib.util
import sys
import unittest
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
AVAILABLE = importlib.util.find_spec("astrbot") is not None
if AVAILABLE:
    from astrbot.api import message_components as C
    from astrbot.core.agent.tool import FunctionTool, ToolSet
    from astrbot.core.provider.func_tool_manager import _PermissionGuardedTool

    from astrbot_plugin_dst_companion.main import DSTCompanion


@unittest.skipUnless(AVAILABLE, "requires the installed AstrBot framework")
class AdapterTests(unittest.IsolatedAsyncioTestCase):
    def event(self, parts, *, admin=False, user="123", message_id="incoming", private=False):
        return NS(
            unified_msg_origin="adapter:GroupMessage:example",
            message_obj=NS(self_id="456", message_id=message_id),
            get_sender_id=lambda: user,
            get_platform_id=lambda: "adapter",
            is_admin=lambda: admin,
            is_private_chat=lambda: private,
            get_messages=lambda: parts,
            get_platform_name=lambda: "aiocqhttp",
            get_group_id=lambda: "789",
            bot=NS(call_action=AsyncMock(return_value={"message_id": 987})),
            stop_event=lambda: setattr(self, "stopped", True),
        )

    def companion(self):
        config = {
            "enabled": True,
            "mcp_server": "dst-read",
            "management_mcp_server": "dst-manage",
            "allowed_sessions": ["adapter:GroupMessage:example"],
            "room_ids": ["r1"],
            "room_aliases": '{"1服":"r1"}',
            "public_queries": [
                "status",
                "players",
                "profile",
                "inventory",
                "network",
                "help",
                "mods",
            ],
            "panel_admins": ["123"],
        }
        manager = NS(
            mcp_server_runtime_view={},
            _check_tool_permission=AsyncMock(return_value=None),
        )
        return DSTCompanion(NS(get_llm_tool_manager=lambda: manager), config)

    async def test_fast_route_stops_event_before_panel_and_never_requests_model(self):
        bot = self.companion()
        event = self.event([C.At(qq="456"), C.Plain("帮助")])
        self.stopped = False
        await bot.on_message(event)
        self.assertTrue(self.stopped)
        event.bot.call_action.assert_awaited_once()
        payload = event.bot.call_action.call_args.kwargs["message"]
        self.assertEqual(payload[-1]["type"], "text")
        self.assertIn("可用查询", payload[-1]["data"]["text"])
        await bot.terminate()

    async def test_fake_at_plain_text_and_other_at_do_not_wake(self):
        bot = self.companion()
        self.stopped = False
        for parts in (
            [C.At(qq="999"), C.Plain("帮助")],
            [C.Plain("@456 帮助")],
            [C.Plain("帮助")],
        ):
            event = self.event(parts)
            await bot.on_message(event)
            event.bot.call_action.assert_not_awaited()
        self.assertFalse(self.stopped)
        await bot.terminate()

    async def test_true_receipt_quoted_lookup_and_plain_cq_not_parsed(self):
        from astrbot_plugin_dst_companion.state import Target

        bot = self.companion()
        event = self.event([])
        await bot.send_reply(event, "玩家 [CQ:at,qq=all]", Target("r1", "KU_example", "玩家"))
        self.assertEqual(bot.state.quoted(bot.actor(event).scope, "987").player, "KU_example")
        self.assertEqual(
            event.bot.call_action.call_args.kwargs["message"][-1],
            {"type": "text", "data": {"text": "玩家 [CQ:at,qq=all]"}},
        )
        await bot.terminate()

    async def test_model_view_preserves_unrelated_tools_persona_and_reduces_dst(self):
        bot = self.companion()
        event = self.event([])
        unrelated = FunctionTool(
            name="unrelated",
            parameters={"type": "object"},
            description="other",
            handler=None,
        )
        raw = FunctionTool(
            name="api_listRooms", parameters={"type": "object"}, description="fixture", handler=None
        )
        raw.mcp_server_name = "dst-read"
        tools = [unrelated, _PermissionGuardedTool(raw, bot.manager)] + [
            FunctionTool(
                name=name, parameters={"type": "object"}, description="fixture", handler=None
            )
            for name in (
                "dst_rooms",
                "dst_room_query",
                "dst_player_query",
                "dst_game_action",
                "dst_job",
                "dst_find_api",
                "dst_call_api",
            )
        ]
        request = NS(func_tool=ToolSet(tools), system_prompt="Existing persona")
        await bot.on_model_request(event, request)
        self.assertEqual(request.system_prompt, "Existing persona")
        self.assertEqual(
            set(request.func_tool.names()),
            {"unrelated", "dst_rooms", "dst_room_query", "dst_player_query"},
        )
        # Actual AstrBot serializes the final provider tool payload.
        payload = request.func_tool.openai_schema()
        self.assertEqual(len(payload), 4)
        await bot.terminate()

    async def test_native_admin_guard_is_reused(self):
        bot = self.companion()
        event = self.event([])
        manager = bot.manager
        manager._check_tool_permission = AsyncMock(return_value="permission denied")
        self.assertFalse(await bot.native(bot.actor(event), "api_getRoomPlayerInventory"))
        context = manager._check_tool_permission.call_args.args[1]
        self.assertIs(context.context.event, event)
        await bot.terminate()

    async def test_unlisted_session_has_no_dst_model_tools(self):
        bot = self.companion()
        event = self.event([])
        event.unified_msg_origin = "adapter:GroupMessage:unlisted"
        raw = FunctionTool(
            name="api_listRooms", parameters={"type": "object"}, description="fixture", handler=None
        )
        raw.mcp_server_name = "dst-read"
        tools = [_PermissionGuardedTool(raw, bot.manager)] + [
            FunctionTool(
                name=name, parameters={"type": "object"}, description="fixture", handler=None
            )
            for name in ("unrelated", "dst_rooms", "dst_game_action", "dst_call_api")
        ]
        request = NS(func_tool=ToolSet(tools), system_prompt="Existing persona")
        await bot.on_model_request(event, request)
        self.assertEqual(request.func_tool.names(), ["unrelated"])
        self.assertEqual(request.system_prompt, "Existing persona")
        self.assertIsNone(bot.actor(event))
        await bot.terminate()

    async def test_no_management_connection_or_disabled_plugin_has_no_management_role(self):
        bot = self.companion()
        event = self.event([], admin=True)
        self.assertEqual(bot.actor(event).role, "panel-manage")
        bot.config["management_mcp_server"] = ""
        self.assertEqual(bot.actor(event).role, "game-read")
        bot.config["enabled"] = False
        self.assertIsNone(bot.actor(event))
        self.assertFalse(await bot.native(NS(event=event), "api_createRoomBackup"))
        await bot.terminate()

    async def test_player_notice_uses_login_id_true_receipt_and_no_send_replay(self):
        bot = self.companion()
        client = NS(call_action=AsyncMock(side_effect=[{"user_id": 456}, {"message_id": 987}]))
        bot.context.get_platform_inst = lambda _: NS(get_client=lambda: client)
        session = "adapter:GroupMessage:789"
        bot.config["allowed_sessions"] = [session]
        related = {"roomId": "r1", "playerId": "KU_example", "name": "玩家"}
        self.assertTrue(await bot.send_notice(session, "示例通知", related))
        self.assertEqual(client.call_action.call_args_list[0].args, ("get_login_info",))
        self.assertEqual(client.call_action.call_args.kwargs["self_id"], "456")
        self.assertEqual(bot.state.quoted(("adapter", "456", session), "987").player, "KU_example")
        client.call_action.reset_mock(side_effect=True)
        client.call_action.side_effect = TimeoutError()
        with self.assertRaises(TimeoutError):
            await bot.send_notice(session, "示例通知", related)
        client.call_action.assert_awaited_once()
        await bot.terminate()

    async def test_revoked_native_permission_suppresses_delayed_job_notice(self):
        bot = self.companion()
        event = self.event([], admin=True)
        actor = bot.actor(event)
        bot.manager._check_tool_permission.return_value = "permission denied"
        await bot.notify_job(actor, "已完成")
        event.bot.call_action.assert_not_awaited()
        await bot.terminate()

    async def test_write_transport_never_uses_automatic_replay(self):
        bot = self.companion()
        session = NS(call_tool=AsyncMock(side_effect=ConnectionError("closed")))
        client = NS(
            session=session,
            tools=[NS(name="api_createRoomBackup", annotations=NS(readOnlyHint=False))],
            _reconnect=AsyncMock(),
        )
        bot.manager.mcp_server_runtime_view["dst-manage"] = NS(client=client)
        from astrbot_plugin_dst_companion.gateway import QueryError

        with self.assertRaises(QueryError):
            await bot.transport("api_createRoomBackup", {}, 1, True)
        session.call_tool.assert_awaited_once()
        client._reconnect.assert_not_awaited()
        await bot.terminate()


if __name__ == "__main__":
    unittest.main()


@unittest.skipUnless(AVAILABLE, "requires the installed AstrBot framework")
class V41FrameworkTests(unittest.IsolatedAsyncioTestCase):
    event = AdapterTests.event
    companion = AdapterTests.companion

    async def test_shortcut_unknown_stops_before_model_but_explicit_ai_passes(self):
        from astrbot_plugin_dst_companion.configuration import DEFAULTS

        bot = self.companion()
        bot.config.update(configuration_version=2, defaults={**DEFAULTS, "rooms": ["r1"]})
        self.stopped = False
        event = self.event([C.At(qq="456"), C.Plain("这个问题需要复杂分析")])
        await bot.on_message(event)
        self.assertTrue(self.stopped)
        self.stopped = False
        event = self.event([C.At(qq="456"), C.Plain("AI: 分析这个问题")], message_id="ai")
        await bot.on_message(event)
        self.assertFalse(self.stopped)
        event.bot.call_action.assert_not_awaited()
        await bot.terminate()

    async def test_actual_native_permission_storage_failure_denies(self):
        from unittest.mock import patch

        from astrbot.core.provider import func_tool_manager

        bot = self.companion()
        a = bot.actor(self.event([]))
        with patch.object(
            func_tool_manager.sp,
            "global_get",
            AsyncMock(side_effect=OSError("storage unavailable")),
        ):
            self.assertFalse(await bot.native(a, "api_listRoomPlayers"))
        with patch.object(
            func_tool_manager.sp,
            "global_get",
            AsyncMock(return_value={"_default": {"api_listRoomPlayers": "admin"}}),
        ):
            self.assertFalse(await bot.native(a, "api_listRoomPlayers"))
        await bot.terminate()

    async def test_actual_schema_default_filling_preserves_explicit_overrides(self):
        import copy
        import json

        from astrbot.core.config.astrbot_config import AstrBotConfig

        from astrbot_plugin_dst_companion.configuration import Configuration

        schema = json.loads(
            (
                Path(__file__).resolve().parents[1]
                / "astrbot_plugin_dst_companion/_conf_schema.json"
            ).read_text()
        )
        defaults = AstrBotConfig._config_schema_to_default_config(None, schema)
        defaults["configuration_version"] = 2
        defaults["defaults"]["rooms"] = ["r1"]
        defaults["defaults"]["pageSize"] = 9
        filled = copy.deepcopy(defaults["defaults"])
        filled["pageSize"] = 5
        filled["taskNotifications"] = False
        defaults["session_overrides"] = [
            {
                "__template_key": "session",
                "session": "group",
                "overrideFields": ["taskNotifications"],
                "values": filled,
            }
        ]
        result = Configuration(defaults).effective("group")
        self.assertEqual(result["pageSize"], 9)
        self.assertFalse(result["taskNotifications"])

    async def test_connection_change_invalidates_old_actor_and_generation_sends_nothing(self):
        bot = self.companion()
        event = self.event([])
        actor = bot.actor(event)
        bot.config["mcp_server"] = "different-panel"
        self.assertFalse(await bot.native(actor, "api_listRooms"))
        await bot.terminate()
        await bot.send_reply(event, "stale result")
        event.bot.call_action.assert_not_awaited()
