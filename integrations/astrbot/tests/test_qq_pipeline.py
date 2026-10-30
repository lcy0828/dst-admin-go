"""Actual AstrBot wake -> plugin dispatch -> model routing, with external I/O stubbed.

These tests do not send QQ messages or invoke models. They cover the framework
stages that direct on_message unit tests bypassed.
"""

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
                "platform_settings": {},
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
        self.process = ProcessStage()
        self.process.ctx = self.ctx
        self.process.agent_sub_stage = NS(process=model)
        self.process.star_request_sub_stage = StarRequestSubStage()
        await self.process.star_request_sub_stage.initialize(self.ctx)
        handlers = [
            StarHandlerMetadata(
                EventType.AdapterMessageEvent,
                "fixture.companion",
                "on_message",
                "fixture.companion",
                self.bot.on_message,
                [EventMessageTypeFilter(filter.EventMessageType.ALL)],
                extras_configs={"priority": 100},
            ),
            StarHandlerMetadata(
                EventType.AdapterMessageEvent,
                "fixture.persona",
                "persona",
                "fixture.persona",
                persona_plugin,
                [EventMessageTypeFilter(filter.EventMessageType.ALL)],
            ),
        ]
        self.patches = [
            patch.object(
                star_handlers_registry,
                "get_handlers_by_event_type",
                side_effect=lambda event_type, **kw: (
                    handlers if event_type == EventType.AdapterMessageEvent else []
                ),
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
                "astrbot.core.pipeline.waking_check.stage.SessionPluginManager.filter_handlers_by_session",
                AsyncMock(side_effect=lambda event, handlers: handlers),
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
        if not event.is_stopped():
            async for _ in self.process.process(event):
                pass
        return [
            call.kwargs["message"][-1]["data"]["text"]
            for call in event.bot.call_action.call_args_list
        ]

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

    async def test_unconfigured_group_prefix_reports_setup_without_exposing_data(self):
        event = self.event([C.Plain("/dst 菜单")], session="99999")
        output = await self.dispatch(event)
        self.assertIn("尚未启用", output[-1])
        self.assertEqual(self.panel.calls, [])
        self.assertEqual(self.models, [])

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


if __name__ == "__main__":
    unittest.main()
