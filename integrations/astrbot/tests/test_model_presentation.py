"""Trusted query cards survive model rewriting; analysis/unrelated tools stay intact."""

import importlib.util
import sys
import unittest
from dataclasses import replace
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock, patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from test_companion import actor

from astrbot_plugin_dst_companion.engine import Reply
from astrbot_plugin_dst_companion.model_presentation import ModelPresentation, requested_intent
from astrbot_plugin_dst_companion.state import Target

AVAILABLE = importlib.util.find_spec("astrbot") is not None
if AVAILABLE:
    import test_qq_pipeline as pipeline
    from astrbot.api import message_components as C
    from astrbot.api.event import MessageChain
    from astrbot.core.agent.message import Message, TextPart
    from astrbot.core.astr_agent_hooks import MainAgentHooks
    from astrbot.core.provider.entities import LLMResponse
    from astrbot.core.star.star import star_map
    from astrbot.core.star.star_handler import EventType, star_handlers_registry


class ModelPresentationTests(unittest.TestCase):
    def start(self, text="AI: 1服状态"):
        self.p = ModelPresentation()
        self.event = NS()
        self.a = actor()
        self.p.begin(self.event, self.a, "source", text, {"1服": "r1", "2服": "r2"})
        self.p.tool(self.event, "dst_rooms")
        self.p.tool(self.event, "dst_room_query")
        return self.p

    def test_explicit_one_query_uses_only_its_trusted_result(self):
        p = self.start()
        p.remember(
            self.event,
            self.a,
            "source",
            "status",
            Reply("matched", "🟢 查询卡片\n来源：面板", Target("r1")),
            "r1",
        )
        self.assertEqual(p.final(self.event, self.a, "source"), "🟢 查询卡片\n来源：面板")
        self.assertIsNone(p.final(NS(), self.a, "source"))
        self.assertIsNone(ModelPresentation().get(self.event))

    def test_query_failures_and_selection_prompts_cannot_turn_into_zero(self):
        for outcome in ("handled_error", "needs_selection", "denied"):
            p = self.start("AI: 1服玩家信息 示例玩家")
            text = "查询尚未确认，不能视为0；请提供或确认玩家昵称。"
            p.remember(self.event, self.a, "source", "profile", Reply(outcome, text), "r1")
            self.assertEqual(p.final(self.event, self.a, "source"), text)

    def test_source_or_permission_change_suppresses_the_old_result(self):
        p = self.start()
        p.remember(
            self.event, self.a, "source", "status", Reply("matched", "旧结果", Target("r1")), "r1"
        )
        for a, source in ((None, "source"), (replace(self.a, rooms=()), "source"), (self.a, "new")):
            self.assertIn("不再展示", p.final(self.event, a, source))

    def test_other_tools_multiple_results_or_wrong_target_do_not_replace_analysis(self):
        variants = [("foreign",), ("multiple",), ("wrong_room",), ("wrong_kind",)]
        for (mode,) in variants:
            p = self.start()
            reply = Reply("matched", "卡片", Target("r2" if mode == "wrong_room" else "r1"))
            p.remember(
                self.event,
                self.a,
                "source",
                "players" if mode == "wrong_kind" else "status",
                reply,
                "r1",
            )
            if mode == "foreign":
                p.tool(self.event, "weather")
            if mode == "multiple":
                p.remember(self.event, self.a, "source", "status", reply, "r1")
            self.assertIsNone(p.final(self.event, self.a, "source"))
        p = self.start("AI: 查背包 指定玩家")
        p.remember(
            self.event,
            self.a,
            "source",
            "inventory",
            Reply("matched", "他人背包", Target("r1", "KU_OTHER", "另一个玩家")),
            "r1",
        )
        self.assertIsNone(p.final(self.event, self.a, "source"))

    def test_analysis_multi_room_and_writes_are_not_display_takeovers(self):
        for text in (
            "AI: 为什么玩家卡",
            "AI: 分析一下1服状态",
            "所有服状态",
            "给 示例玩家 三个肉丸",
            "普通聊天",
        ):
            with self.subTest(text=text):
                self.assertIsNone(requested_intent(text, {"1服": "r1"}))
        self.assertEqual(requested_intent("问AI: 玩家背包", {}).kind, "inventory")

    def test_event_state_and_tool_budget_are_bounded(self):
        p = self.start()
        for _ in range(20):
            p.remember(
                self.event, self.a, "source", "status", Reply("matched", "x", Target("r1")), "r1"
            )
            p.tool(self.event, "dst_room_query")
        self.assertEqual(len(p.get(self.event).records), 2)
        self.assertIsNone(p.final(self.event, self.a, "source"))


@unittest.skipUnless(AVAILABLE, "requires installed AstrBot")
class ModelCardAdapterTests(unittest.IsolatedAsyncioTestCase):
    if AVAILABLE:
        asyncSetUp = pipeline.QQPipelineTests.asyncSetUp
        asyncTearDown = pipeline.QQPipelineTests.asyncTearDown
        event = pipeline.QQPipelineTests.event
        dispatch = pipeline.QQPipelineTests.dispatch

    async def query(self, text="AI: 1服状态", kind="status", player=""):
        self.bot.prepare = AsyncMock(return_value=True)
        event = self.event([C.At(qq="456"), C.Plain(text)])
        request = NS(system_prompt="原人格", func_tool=None)
        await self.bot.on_model_request(event, request)
        name = "dst_player_query" if player else "dst_room_query"
        await self.bot.on_model_tool(event, NS(name=name), {})
        card = await self.bot.models.query(event, kind, "r1", player)
        return event, request, card

    async def test_actual_llm_response_keeps_card_instead_of_model_prose(self):
        event, request, card = await self.query()
        response = LLMResponse(role="assistant", completion_text="面板没什么信息，在线0人。")
        await self.bot.on_model_response(event, response)
        self.assertEqual(response.completion_text, card)
        self.assertIn("世界状态", card)
        self.assertIn("采样", card)
        self.assertTrue(request.system_prompt.startswith("原人格"))
        self.assertIn("DST 回复排版", request.system_prompt)
        self.assertIn("单项查询直接给出卡片", request.system_prompt)
        self.assertIn("不代表没有历史玩家", request.system_prompt)

    async def test_management_model_text_is_plain_without_rewriting_evidence(self):
        self.bot.prepare = AsyncMock(return_value=True)
        event = self.event([C.At(qq="456"), C.Plain("再次复活")])
        await self.bot.on_model_request(event, NS(system_prompt="原人格", func_tool=None))
        await self.bot.on_model_tool(event, NS(name="dst_game_action"), {})
        original = "## 复活结果\n已提交 **复活指令**；玩家 `KU_SAMPLE` 的生死变化尚未确认，**请勿重复提交**。"
        response = LLMResponse(role="assistant", completion_text=original)
        await self.bot.on_model_response(event, response)
        shown = response.completion_text
        self.assertNotIn("**", shown)
        self.assertNotIn("`", shown)
        self.assertIn("生死变化尚未确认", shown)
        self.assertIn("请勿重复提交", shown)
        self.assertIn("KU_SAMPLE", shown)
        run = NS(messages=[Message(role="assistant", content=[TextPart(text=original)])])
        await self.bot.on_model_done(event, run, response)
        self.assertEqual(run.messages[-1].content[0].text, shown)
        await self.bot.on_model_response(event, response)
        self.assertEqual(response.completion_text, shown)

    async def test_plain_text_conversion_is_scoped_to_current_dst_request(self):
        self.bot.prepare = AsyncMock(return_value=True)
        for mode in ("unrelated", "source_changed", "permission_changed"):
            event = self.event([C.At(qq="456"), C.Plain("今天天气怎么样")])
            await self.bot.on_model_request(event, NS(system_prompt="原人格", func_tool=None))
            if mode != "unrelated":
                await self.bot.on_model_tool(event, NS(name="dst_game_action"), {})
            if mode == "permission_changed":
                self.bot.config["allowed_sessions"] = []
            original = "**保持原文**"
            response = LLMResponse(role="assistant", completion_text=original)
            if mode == "source_changed":
                with patch.object(self.bot, "connection_identity", return_value="another-source"):
                    await self.bot.on_model_response(event, response)
            else:
                await self.bot.on_model_response(event, response)
            self.assertEqual(response.completion_text, original)

    async def test_direct_qq_send_removes_markup_from_custom_templates(self):
        event = self.event([C.At(qq="456"), C.Plain("帮助")])
        await self.bot.send_reply(event, "**已收到指令**\n玩家 `KU_SAMPLE`；结果未确认，请勿重发。")
        payload = event.bot.call_action.call_args.kwargs["message"][-1]
        self.assertEqual(payload["type"], "text")
        self.assertEqual(
            payload["data"]["text"], "已收到指令\n玩家 KU_SAMPLE；结果未确认，请勿重发。"
        )

    async def test_real_agent_done_hooks_update_response_and_conversation_history(self):
        event, _, card = await self.query()
        original = "模型将卡片改写成了散文"
        response = LLMResponse(role="assistant", completion_text=original)
        run = NS(
            context=NS(event=event),
            messages=[Message(role="assistant", content=[TextPart(text=original)])],
        )
        hooks = []
        for meta in star_handlers_registry:
            if meta.handler_module_path == type(self.bot).__module__ and meta.event_type in {
                EventType.OnLLMResponseEvent,
                EventType.OnAgentDoneEvent,
            }:
                from copy import copy

                item = copy(meta)
                item.handler = getattr(self.bot, item.handler_name)
                hooks.append(item)
        with (
            patch.object(
                star_handlers_registry,
                "get_handlers_by_event_type",
                side_effect=lambda event_type, **_: [
                    h for h in hooks if h.event_type == event_type
                ],
            ),
            patch.dict(
                star_map,
                {
                    type(self.bot).__module__: NS(
                        name="fixture", display_name="fixture", reserved=False, activated=True
                    )
                },
            ),
        ):
            await MainAgentHooks().on_agent_done(run, response)
        self.assertEqual(response.completion_text, card)
        self.assertEqual(run.messages[-1].content[0].text, card)
        self.assertFalse(event.bot.call_action.called)

    async def test_offline_inventory_retains_chinese_source_and_save_time(self):
        self.panel.candidates = [
            {**self.panel.candidates[0], "online": False, "presenceStatus": "live"}
        ]
        event, _, card = await self.query("AI: 查背包 玩家 A 🦌", "inventory", "玩家 A 🦌")
        response = LLMResponse(role="assistant", completion_text="只有树枝12个")
        await self.bot.on_model_response(event, response)
        self.assertEqual(response.completion_text, card)
        self.assertIn("存档快照", card)
        self.assertIn("树枝", card)
        self.assertIn("2026-01-01 08:00:00", card)

    async def test_unrelated_tools_images_streams_and_complex_analysis_are_preserved(self):
        for mode in ("unrelated", "image", "stream"):
            event, _, _ = await self.query()
            response = LLMResponse(role="assistant", completion_text="保留模型回答")
            if mode == "unrelated":
                await self.bot.on_model_tool(event, NS(name="weather"), {})
            if mode == "image":
                response.result_chain = MessageChain(
                    chain=[
                        C.Plain("保留模型回答"),
                        C.Image.fromURL("https://example.invalid/demo.png"),
                    ]
                )
            if mode == "stream":
                event.set_extra("enable_streaming", True)
            await self.bot.on_model_response(event, response)
            self.assertEqual(response.completion_text, "保留模型回答")
        event, _, _ = await self.query("AI: 分析一下1服状态")
        response = LLMResponse(role="assistant", completion_text="保留原因分析")
        await self.bot.on_model_response(event, response)
        self.assertEqual(response.completion_text, "保留原因分析")

    async def test_permission_revocation_after_tool_result_does_not_show_cached_data(self):
        event, _, _ = await self.query()
        self.bot.config["allowed_sessions"] = []
        response = LLMResponse(role="assistant", completion_text="旧世界信息")
        await self.bot.on_model_response(event, response)
        self.assertIn("不再展示", response.completion_text)
        self.assertNotIn("旧世界信息", response.completion_text)
