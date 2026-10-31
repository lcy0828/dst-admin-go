"""Actual AstrBot wake -> plugin dispatch -> model routing, with external I/O stubbed.

These tests do not send QQ messages or invoke models. They cover the framework
stages that direct on_message unit tests bypassed.
"""

import copy
import functools
import importlib.util
import sys
import unittest
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock, patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
AVAILABLE = importlib.util.find_spec("astrbot") is not None
if AVAILABLE:
    import test_astrbot_adapter as adapters
    from astrbot.api import message_components as C
    from astrbot.api.event import filter
    from astrbot.api.platform import AstrBotMessage, MessageMember, PlatformMetadata
    from astrbot.core.pipeline.process_stage.method.star_request import StarRequestSubStage
    from astrbot.core.pipeline.process_stage.stage import ProcessStage
    from astrbot.core.pipeline.waking_check.stage import WakingCheckStage
    from astrbot.core.pipeline.whitelist_check.stage import WhitelistCheckStage
    from astrbot.core.platform.message_type import MessageType
    from astrbot.core.platform.sources.aiocqhttp.aiocqhttp_message_event import (
        AiocqhttpMessageEvent,
    )
    from astrbot.core.star.filter.event_message_type import EventMessageTypeFilter
    from astrbot.core.star.star import star_map
    from astrbot.core.star.star_handler import (
        EventType,
        StarHandlerMetadata,
        star_handlers_registry,
    )
    from test_companion import FakeGateway, mcp


@unittest.skipUnless(AVAILABLE, "requires the installed AstrBot framework")
class QQPipelineTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.bot = adapters.AdapterTests.companion(self)
        self.bot.config["allowed_sessions"] = [
            "adapter:GroupMessage:789",
            "adapter:FriendMessage:123",
        ]
        self.bot.config["allow_private"] = True
        self.panel = FakeGateway()
        self.bot.policy.native = AsyncMock(return_value=True)

        async def transport(name, args, timeout, write):
            return mcp(await self.panel.call(None, name, args, 0))

        self.bot.gateway.transport = transport
        self.models = []
        self.other_handlers = []

        async def model(event):
            self.models.append(event)
            if False:
                yield

        async def persona_plugin(event):
            self.other_handlers.append(event)
            event.message_str = "identity prefix " + event.message_str

        self.ctx = NS(
            astrbot_config={
                "platform_settings": {
                    "enable_id_white_list": False,
                    "id_whitelist": [],
                    "wl_ignore_admin_on_group": False,
                    "wl_ignore_admin_on_friend": False,
                    "id_whitelist_log": False,
                },
                "admins_id": [],
                "wake_prefix": ["/"],
                "provider_settings": {"enable": True, "prompt_prefix": "", "identifier": False},
            },
            astrbot_config_id="fixture",
            db_helper=NS(),
        )
        self.wake = WakingCheckStage()
        await self.wake.initialize(self.ctx)
        self.wake._umo_auto_name_recorder = NS(schedule=lambda _: None)
        self.bot.context.get_config = lambda **kw: self.ctx.astrbot_config
        self.process = ProcessStage()
        self.process.ctx = self.ctx
        self.process.agent_sub_stage = NS(process=model)
        self.process.star_request_sub_stage = StarRequestSubStage()
        await self.process.star_request_sub_stage.initialize(self.ctx)
        # Use the plugin's actual decorator registrations and the framework's
        # real session/configuration filtering. Only persistence and I/O are faked.
        self.companion_module = type(self.bot).__module__
        handlers = [
            copy.copy(m)
            for m in star_handlers_registry
            if m.handler_module_path == self.companion_module
            and m.event_type == EventType.AdapterMessageEvent
        ]
        for metadata in handlers:
            metadata.handler = functools.partial(
                getattr(type(self.bot), metadata.handler_name), self.bot
            )
        self.assertEqual({h.handler_name for h in handlers}, {"dst_command", "on_message"})
        handlers.extend(
            [
                StarHandlerMetadata(
                    EventType.AdapterMessageEvent,
                    "fixture.persona",
                    "persona",
                    "fixture.persona",
                    persona_plugin,
                    [EventMessageTypeFilter(filter.EventMessageType.ALL)],
                ),
            ]
        )
        handlers.sort(key=lambda h: -h.extras_configs.get("priority", 0))
        self.handlers = handlers

        def get_handlers(event_type, **kwargs):
            if event_type != EventType.AdapterMessageEvent:
                return []
            selected = kwargs.get("plugins_name")
            return [
                h
                for h in handlers
                if selected is None or star_map[h.handler_module_path].name in selected
            ]

        self.patches = [
            patch.object(
                star_handlers_registry,
                "get_handlers_by_event_type",
                side_effect=get_handlers,
            ),
            patch.dict(
                star_map,
                {
                    m.handler_module_path: NS(
                        name=m.handler_module_path,
                        display_name="fixture",
                        reserved=False,
                        activated=True,
                    )
                    for m in handlers
                },
            ),
            patch(
                "astrbot.core.star.session_plugin_manager.sp.get_async",
                AsyncMock(return_value={}),
            ),
        ]
        for p in self.patches:
            p.start()
        self.sequence = 0

    async def asyncTearDown(self):
        for p in reversed(self.patches):
            p.stop()
        await self.bot.terminate()

    def event(self, parts, user="123", session="789", private=False):
        self.sequence += 1
        message = AstrBotMessage()
        message.type = MessageType.FRIEND_MESSAGE if private else MessageType.GROUP_MESSAGE
        message.self_id = "456"
        message.message_id = str(self.sequence)
        message.group_id = "" if private else session
        message.sender = MessageMember(user, "test-user")
        message.message = parts
        message.message_str = "".join(p.text for p in parts if isinstance(p, C.Plain))
        message.raw_message = {}
        client = NS(call_action=AsyncMock(return_value={"message_id": str(1000 + self.sequence)}))
        client.send_group_msg = client.call_action
        client.send_private_msg = client.call_action
        return AiocqhttpMessageEvent(
            message.message_str,
            message,
            PlatformMetadata("aiocqhttp", "fixture", "adapter"),
            user if private else session,
            client,
        )

    async def dispatch(self, event):
        await self.wake.process(event)
        whitelist = WhitelistCheckStage()
        await whitelist.initialize(self.ctx)
        if not event.is_stopped():
            await whitelist.process(event)
        if not event.is_stopped():
            async for _ in self.process.process(event):
                pass
        return [
            call.kwargs["message"][-1]["data"]["text"]
            for call in event.bot.call_action.call_args_list
            if "message" in call.kwargs
        ]

    def automatic_setup(self):
        import json

        from astrbot.core.config.astrbot_config import AstrBotConfig

        from astrbot_plugin_dst_companion.connections import DISCOVERY_TOOLS

        schema = json.loads(
            (
                Path(__file__).resolve().parents[1]
                / "astrbot_plugin_dst_companion/_conf_schema.json"
            ).read_text()
        )
        self.bot.config.clear()
        self.bot.config.update(AstrBotConfig._config_schema_to_default_config(None, schema))
        self.bot.enrollment.loaded = True
        self.bot.enrollment.save = AsyncMock()
        self.bot.notifications.reconcile = AsyncMock()
        self.bot.manager.mcp_server_runtime_view["panel"] = NS(
            client=NS(
                session=NS(),
                tools=[NS(name=n) for n in DISCOVERY_TOOLS],
                _mcp_server_config={"url": "https://panel.example/mcp"},
            )
        )
        self.bot.catalog.read = AsyncMock(return_value={"items": [{"id": "r1", "name": "1服"}]})
        self.ctx.astrbot_config["admins_id"] = ["123"]

    async def test_real_pipeline_group_enable_then_menu_number_never_uses_model(self):
        self.automatic_setup()
        enabled = await self.dispatch(self.event([C.Plain("/dst 启用本群")]))
        self.assertIn("已启用本群", enabled[-1])
        output = await self.dispatch(self.event([C.Plain("菜单 "), C.At(qq="456")]))
        self.assertIn("可用查询", output[-1])
        output = await self.dispatch(self.event([C.Plain("1")]))
        self.assertIn("可访问房间", output[-1])
        self.assertEqual(self.models, [])
        self.assertEqual(self.other_handlers, [])

    async def test_reported_dst_commands_are_claimed_and_combined_setting_is_saved(self):
        self.automatic_setup()
        self.ctx.astrbot_config["plugin_set"] = [self.companion_module]
        output = await self.dispatch(self.event([C.Plain("/dst")]))
        self.assertIn("DST 助手设置", output[-1])
        self.bot.catalog.read.assert_not_awaited()
        output = await self.dispatch(self.event([C.Plain("/dst 启动本群")]))
        self.assertIn("已启用本群", output[-1])
        output = await self.dispatch(self.event([C.Plain("/dst 启动本群 聊天播报 开")]))
        self.assertIn("聊天播报开关：开", output[-1])
        self.assertTrue(self.bot.enrollment.records["adapter:GroupMessage:789"]["notices"]["chat"])
        self.assertNotIn("adapter:GroupMessage:999", self.bot.enrollment.records)
        self.assertEqual(self.models, [])
        self.assertEqual(self.other_handlers, [])
        self.bot.enrollment.save.assert_awaited()

    async def test_isolated_group_member_can_query_named_inventory_without_model(self):
        self.automatic_setup()
        self.ctx.astrbot_config["platform_settings"].update(
            enable_id_white_list=True, id_whitelist=["789"]
        )
        enable = self.event([C.Plain("/dst 启用本群 聊天播报 开")], session="123_789")
        enable.message_obj.group_id = "789"
        output = await self.dispatch(enable)
        self.assertIn("已启用本群", output[-1])
        member = self.event(
            [C.At(qq="456"), C.Plain("查询 example-player 背包")], user="999", session="999_789"
        )
        member.message_obj.group_id = "789"
        output = await self.dispatch(member)
        self.assertTrue(output)
        resolution = [args for name, args in self.panel.calls if name == "api_resolveRoomPlayer"]
        self.assertEqual(resolution[-1]["query"]["query"], "example-player")
        self.assertTrue(any(name == "api_getRoomPlayerInventory" for name, _ in self.panel.calls))
        unnamed = self.event(
            [C.At(qq="456"), C.Plain("查询 玩家背包")], user="888", session="888_789"
        )
        unnamed.message_obj.group_id = "789"
        self.panel.calls.clear()
        output = await self.dispatch(unnamed)
        self.assertIn("玩家昵称", output[-1])
        self.assertFalse(any(name == "api_resolveRoomPlayer" for name, _ in self.panel.calls))
        self.assertEqual(self.models, [])
        self.assertEqual(self.other_handlers, [])
        self.bot.enrollment.save.assert_awaited()

    async def test_unknown_explicit_commands_never_escape_to_model(self):
        self.automatic_setup()
        for text in ("/dst 启用本群 聊天播报 随便", "/dst 未知设置", "/dst\t设置"):
            output = await self.dispatch(self.event([C.Plain(text)]))
            self.assertTrue(output)
            self.assertNotIn("已启用本群", output[-1])
        self.assertEqual(self.models, [])
        self.assertEqual(self.bot.enrollment.records, {})

    async def test_enrolled_group_unknown_command_and_uncertain_reply_do_not_use_model(self):
        self.automatic_setup()
        await self.dispatch(self.event([C.Plain("/dst 启用本群")]))
        output = await self.dispatch(self.event([C.Plain("/dst 修改神秘开关")]))
        self.assertIn("未识别此 DST 指令", output[-1])
        event = self.event([C.Plain("/dst")])
        event.bot.call_action.side_effect = TimeoutError()
        await self.dispatch(event)
        self.assertTrue(event.is_stopped())
        self.assertEqual(event.bot.call_action.await_count, 1)
        self.assertEqual(self.models, [])

    async def test_real_session_filter_and_plugin_selection_are_respected(self):
        self.automatic_setup()
        # A native exclusion must not be bypassed by either command or listener.
        self.ctx.astrbot_config["plugin_set"] = ["fixture.persona"]
        await self.dispatch(self.event([C.Plain("/dst 启用本群")]))
        self.assertEqual(self.bot.enrollment.records, {})
        self.ctx.astrbot_config["plugin_set"] = [self.companion_module]
        with patch(
            "astrbot.core.star.session_plugin_manager.sp.get_async",
            AsyncMock(
                return_value={
                    "adapter:GroupMessage:789": {"disabled_plugins": [self.companion_module]}
                }
            ),
        ):
            await self.dispatch(self.event([C.Plain("/dst 启用本群")]))
        self.assertEqual(self.bot.enrollment.records, {})

    async def test_admin_exemption_does_not_silently_grant_whole_group_access(self):
        self.automatic_setup()
        settings = self.ctx.astrbot_config["platform_settings"]
        settings.update(
            enable_id_white_list=True, id_whitelist=["another-group"], wl_ignore_admin_on_group=True
        )
        output = await self.dispatch(self.event([C.Plain("/dst 启用本群")]))
        self.assertIn("管理员豁免", output[-1])
        self.assertEqual(self.bot.enrollment.records, {})
        output = await self.dispatch(self.event([C.Plain("/dst")], user="999"))
        self.assertEqual(output, [])
        # Grant only the selected group; native access is checked before setup.
        settings["id_whitelist"].append("adapter:GroupMessage:789")
        output = await self.dispatch(self.event([C.Plain("/dst 启动本群 聊天播报 开")]))
        self.assertIn("已启用本群", output[-1])
        self.assertTrue(self.bot.allowed("adapter:GroupMessage:789"))
        self.assertFalse(self.bot.allowed("adapter:GroupMessage:999"))
        self.assertEqual(self.models, [])

    async def test_real_pipeline_nonadmin_cannot_enable_another_group(self):
        self.automatic_setup()
        output = await self.dispatch(self.event([C.Plain("/dst 启用本群")], user="999"))
        self.assertIn("仅允许 AstrBot 管理员", output[-1])
        self.bot.catalog.read.assert_not_awaited()
        self.assertEqual(self.bot.enrollment.records, {})
        self.assertEqual(self.models, [])

    async def test_real_group_leading_and_trailing_at_menus_never_use_model(self):
        for parts in ([C.At(qq="456"), C.Plain(" 菜单")], [C.Plain("菜单 "), C.At(qq="456")]):
            event = self.event(parts)
            output = await self.dispatch(event)
            self.assertTrue(event.is_stopped())
            self.assertIn("可用查询", output[-1])
        self.assertEqual(self.models, [])
        self.assertEqual(self.other_handlers, [])

    async def test_bad_number_and_replaced_menu_reply_do_not_escape_to_model(self):
        await self.dispatch(self.event([C.At(qq="456"), C.Plain("菜单")]))
        first = self.bot.state.choices[self.bot.actor(self.event([])).key]["messageId"]
        await self.dispatch(self.event([C.At(qq="456"), C.Plain("菜单")]))
        output = await self.dispatch(self.event([C.Plain("99")]))
        self.assertIn("序号不在当前页", output[-1])
        output = await self.dispatch(self.event([C.Reply(id=first, sender_id="456"), C.Plain("1")]))
        self.assertIn("已被替换", output[-1])
        self.assertEqual(self.models, [])

    async def test_mixed_mode_does_not_consume_ordinary_unaddressed_group_chat(self):
        output = await self.dispatch(self.event([C.Plain("这个模组具体什么效果")]))
        self.assertEqual(output, [])
        self.assertEqual(self.models, [])
        self.assertEqual(self.panel.calls, [])

    async def test_menu_reopen_then_plain_number_and_page_are_owned_without_at(self):
        for _ in range(2):
            await self.dispatch(self.event([C.At(qq="456"), C.Plain("菜单")]))
        other = self.event([C.Plain("1")], user="999")
        self.assertEqual(await self.dispatch(other), [])
        selection = self.event([C.Plain("1")])
        output = await self.dispatch(selection)
        self.assertIn("可访问房间", output[-1])
        self.assertEqual(self.models, [])
        # A numeric choice on the new room menu continues to actual panel data.
        output = await self.dispatch(self.event([C.Plain("1")]))
        self.assertIn("运行中", output[-1])
        self.assertTrue(any(name == "api_listWorldStates" for name, _ in self.panel.calls))

    async def test_private_menu_and_reply_to_bot_wake_through_framework(self):
        output = await self.dispatch(self.event([C.Plain("菜单")], private=True))
        self.assertIn("可用查询", output[-1])
        output = await self.dispatch(
            self.event([C.Reply(id="555", sender_id="456"), C.Plain("菜单")])
        )
        self.assertIn("可用查询", output[-1])
        self.assertEqual(self.models, [])

    async def test_quoted_leave_notice_in_isolated_session_queries_inventory_without_model(self):
        from astrbot_plugin_dst_companion.state import Target

        session = "123_789"
        self.bot.config["allowed_sessions"].append("adapter:GroupMessage:" + session)
        self.bot.state.associate(
            ("adapter", "456", "adapter:GroupMessage:789"),
            "555",
            Target("r1", "KU_example", "示例玩家"),
            source=self.bot.connections.source(),
        )
        event = self.event(
            [C.Reply(id="555", sender_id="456"), C.At(qq="456"), C.Plain("查背包")], session=session
        )
        output = await self.dispatch(event)
        self.assertTrue(any("背包" in text for text in output))
        inventory = [
            args for name, args in self.panel.calls if name == "api_getRoomPlayerInventory"
        ]
        self.assertEqual(inventory[0]["path"]["playerId"], "KU_example")
        self.assertTrue(event.is_stopped())
        self.assertEqual(self.models, [])
        self.assertEqual(self.other_handlers, [])

    async def test_untrusted_quote_does_not_reuse_a_previous_players_inventory(self):
        from astrbot_plugin_dst_companion.state import Target

        event = self.event([C.Reply(id="555", sender_id="999"), C.At(qq="456"), C.Plain("查背包")])
        actor = self.bot.actor(event)
        sequence = self.bot.state.begin(actor.key)
        self.bot.state.remember(actor.key, sequence, Target("r1", "KU_previous"), "此前查询")
        output = await self.dispatch(event)
        self.assertIn("未能确认引用中的玩家", output[-1])
        self.assertFalse(any(name == "api_getRoomPlayerInventory" for name, _ in self.panel.calls))
        self.assertEqual(self.models, [])

    async def test_legacy_quoted_notice_rechecks_original_and_reads_offline_inventory(self):
        import time

        from astrbot_plugin_dst_companion.broadcast import presence_text

        self.panel.candidates = [
            {**self.panel.candidates[0], "online": False, "presenceStatus": "offline"}
        ]
        text = presence_text("测试房间", "player.left", {"name": "示例玩家"}, "地面", {})
        event = self.event(
            [
                C.Reply(id="555", sender_id="456", message_str=text),
                C.At(qq="456"),
                C.Plain("查背包"),
            ]
        )
        self.bot.notifications.saved = {
            "adapter:GroupMessage:789": {
                "source": self.bot.connections.source(),
                "state": {
                    "deliveries": [
                        {"key": "r1/world/cursor", "at": time.time() - 1, "status": "sent"}
                    ]
                },
            }
        }
        original = {
            "message_id": 555,
            "message_type": "group",
            "group_id": 789,
            "sender": {"user_id": 456},
            "time": time.time(),
            "message": [{"type": "text", "data": {"text": text}}],
        }

        async def call_action(action, **kwargs):
            return original if action == "get_msg" else {"message_id": "receipt"}

        event.bot.call_action.side_effect = call_action
        output = await self.dispatch(event)
        self.assertTrue(any("存档快照" in text for text in output))
        resolution = [args for name, args in self.panel.calls if name == "api_resolveRoomPlayer"]
        self.assertEqual(resolution[0]["query"]["query"], "示例玩家")
        self.assertEqual(self.models, [])
        self.assertEqual(self.other_handlers, [])

    async def test_chinese_quantity_grant_is_claimed_before_default_model_pipeline(self):
        from astrbot_plugin_dst_companion.engine import Reply

        self.ctx.astrbot_config["admins_id"] = ["123"]
        original = self.bot.engine.handle

        async def handled(actor, text, **kwargs):
            route = self.bot.engine.parse(actor, text)
            self.assertEqual(route.capability, "player.give_item")
            self.assertEqual(route.parameters, {"playerId": "示例玩家", "item": "肉丸", "count": 3})
            self.assertEqual(actor.role, "panel-manage")
            return Reply("matched", "固定发放入口已匹配（测试替身未执行写入）。")

        self.bot.engine.handle = handled
        try:
            event = self.event([C.At(qq="456"), C.Plain("给 示例玩家 三个肉丸")])
            output = await self.dispatch(event)
            self.assertIn("固定发放入口已匹配", output[-1])
            self.assertTrue(event.is_stopped())
            self.assertEqual(self.models, [])
            self.assertEqual(self.other_handlers, [])
            self.assertEqual(self.panel.calls, [])
        finally:
            self.bot.engine.handle = original

    async def test_unconfigured_group_prefix_reports_setup_without_exposing_data(self):
        event = self.event([C.Plain("/dst 菜单")], session="99999")
        output = await self.dispatch(event)
        self.assertIn("尚未启用", output[-1])
        self.assertEqual(self.panel.calls, [])
        self.assertEqual(self.models, [])

    async def test_chinese_quantity_spawn_uses_shortcut_before_model_pipeline(self):
        from astrbot_plugin_dst_companion.engine import Reply

        self.ctx.astrbot_config["admins_id"] = ["123"]

        async def handled(actor, text, **kwargs):
            intent = self.bot.engine.parse(actor, text)
            self.assertEqual(intent.capability, "player.spawn_entity")
            self.assertEqual(
                intent.parameters, {"playerId": "示例玩家", "entity": "座狼", "count": 3}
            )
            self.assertEqual(actor.role, "panel-manage")
            return Reply("matched", "已匹配受控生成入口（测试替身未执行）。")

        self.bot.engine.handle = handled
        event = self.event([C.At(qq="456"), C.Plain("在 示例玩家 附近生成三只座狼")])
        output = await self.dispatch(event)
        self.assertIn("已匹配受控生成入口", output[-1])
        self.assertTrue(event.is_stopped())
        self.assertEqual(self.models, [])
        self.assertEqual(self.other_handlers, [])
        self.assertEqual(self.panel.calls, [])

    async def test_mixed_routes_complex_questions_to_model_but_quick_queries_remain_direct(self):
        event = self.event([C.At(qq="456"), C.Plain("这个模组具体什么效果")])
        await self.dispatch(event)
        self.assertEqual(self.models, [event])
        self.assertEqual(self.panel.calls, [])
        self.models.clear()
        output = await self.dispatch(self.event([C.At(qq="456"), C.Plain("1服信息")]))
        self.assertIn("运行中", output[-1])
        self.assertIn("在线 1 人", output[-1])
        self.assertEqual(self.models, [])

    async def test_template_commands_are_claimed_before_persona_and_models(self):
        self.automatic_setup()
        output = await self.dispatch(self.event([C.Plain("/dst 启用本群")]))
        self.assertIn("已启用本群", output[-1])
        self.bot.catalog.read.reset_mock()
        for command, expected in (
            ("/dst 模板 编辑 聊天播报 💬 {room_name}\n{lines}", "草稿"),
            ("/dst 模板 预览 聊天播报", "模拟预览"),
            ("/dst 模板 保存", "已保存"),
        ):
            event = self.event([C.Plain(command)])
            output = await self.dispatch(event)
            self.assertTrue(event.is_stopped())
            self.assertIn(expected, output[-1])
        self.assertEqual(self.models, [])
        self.assertEqual(self.other_handlers, [])
        self.bot.catalog.read.assert_not_awaited()
        self.assertEqual(self.panel.calls, [])

    async def test_revive_is_claimed_by_plugin_before_model_and_persona(self):
        from test_v41 import Panel
        from test_workbench import recipe

        panel = Panel()
        panel.items = [recipe()]
        self.bot.gateway.transport = panel.transport
        # Explicit manager grants remain subject to the backend and native tools.
        self.ctx.astrbot_config["admins_id"] = ["123"]
        self.bot.policy.native = AsyncMock(return_value=True)
        event = self.event([C.At(qq="456"), C.Plain("复活玩家")])
        output = await self.dispatch(event)
        self.assertTrue(event.is_stopped())
        self.assertTrue(any("玩家昵称或 KU ID" in text for text in output))
        self.assertFalse(any("player_id" in text for text in output))
        self.assertEqual(panel.executions, 0)
        self.assertEqual(self.models, [])
        self.assertEqual(self.other_handlers, [])


if __name__ == "__main__":
    unittest.main()
