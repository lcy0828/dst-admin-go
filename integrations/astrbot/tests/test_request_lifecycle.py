"""New requests retire old DST model runs through the actual AstrBot adapter.

All QQ sends, panel operations and model responses are intercepted fixtures.
"""

import asyncio
import copy
import gc
import importlib.util
import json
import sys
import time
import unittest
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from astrbot_plugin_dst_companion.routing import parse

AVAILABLE = importlib.util.find_spec("astrbot") is not None
if AVAILABLE:
    import test_qq_pipeline as pipeline
    from astrbot.api import message_components as C
    from astrbot.api.event import MessageChain
    from astrbot.core.agent.hooks import BaseAgentRunHooks
    from astrbot.core.agent.message import Message, TextPart, ThinkPart
    from astrbot.core.agent.run_context import ContextWrapper
    from astrbot.core.agent.runners.tool_loop_agent_runner import ToolLoopAgentRunner
    from astrbot.core.astr_agent_run_util import run_agent
    from astrbot.core.provider.entities import LLMResponse, ProviderRequest


class PlayerListGrammarTests(unittest.TestCase):
    def test_list_aliases_preserve_room_and_named_profile(self):
        aliases = {"1服": "r1", "2服": "r2"}
        for text, room in (
            ("查人", ""), ("差人", ""), ("请查人", ""), ("查人呀", ""),
            ("帮我查一下人", ""), ("在线名单", ""), ("查人数", ""),
            ("查询在线名单", ""), ("一服查人", "r1"), ("2服查下人", "r2"),
        ):
            with self.subTest(text=text):
                intent = parse(text, aliases)
                self.assertEqual((intent.kind, intent.room, intent.player), ("players", room, ""))
        for name in ("示例玩家", "差人", "查人", "呀", "小呀"):
            intent = parse("1服查人 " + name, aliases)
            self.assertEqual((intent.kind, intent.room, intent.player), ("profile", "r1", name))

    def test_negation_discussion_and_compound_questions_remain_model_input(self):
        for text in ("不要查人", "为什么查人失败", "分析一下在线名单", "查人然后重启房间", "查人和查背包有什么区别"):
            with self.subTest(text=text):
                self.assertIsNone(parse(text, {}))


@unittest.skipUnless(AVAILABLE, "requires installed AstrBot")
class RequestLifecycleTests(unittest.IsolatedAsyncioTestCase):
    if AVAILABLE:
        asyncSetUp = pipeline.QQPipelineTests.asyncSetUp
        asyncTearDown = pipeline.QQPipelineTests.asyncTearDown
        event = pipeline.QQPipelineTests.event
        dispatch = pipeline.QQPipelineTests.dispatch

    async def model_event(self, text="帮我分析服务器现在是谁在线", **identity):
        event = self.event([C.At(qq="456"), C.Plain(text)], **identity)
        await self.dispatch(event)
        await self.bot.on_model_request(event, NS(system_prompt="原人格", func_tool=None))
        return event

    async def use_dst(self, event, name="dst_find"):
        await self.bot.on_model_tool(event, NS(name=name), {})

    async def test_common_player_list_phrases_use_one_read_and_no_model(self):
        for text in ("查人", "差人", "一服查人", "查一下人", "帮我查人呀", "在线名单"):
            with self.subTest(text=text):
                before = len(self.panel.calls)
                shown = await self.dispatch(self.event([C.At(qq="456"), C.Plain(text)]))
                self.assertIn("在线 1", shown[-1])
                calls = self.panel.calls[before:]
                self.assertEqual(sum(name == "api_listRoomPlayers" for name, _ in calls), 1)
                self.assertFalse(any(name == "api_resolveRoomPlayer" for name, _ in calls))
        self.assertEqual(self.models, [])
        self.assertEqual(self.other_handlers, [])

    async def test_new_help_and_number_suppress_old_final_stream_media_and_error(self):
        old = await self.model_event()
        await self.use_dst(old)
        menu = await self.dispatch(self.event([C.At(qq="456"), C.Plain("帮助")]))
        self.assertIn("可用查询", menu[-1])
        self.assertTrue(old.is_stopped())
        self.assertTrue(old.get_extra("agent_stop_requested"))
        shown = await self.dispatch(self.event([C.Plain("3")]))
        self.assertIn("在线 1", shown[-1])
        for extra in ({}, {"is_chunk": True}, {"tools_call_name": ["dst_read"]},
                      {"result_chain": MessageChain().message("过时结果")}, {"role": "err"}):
            response = LLMResponse(role="assistant", completion_text="查询被中断，没拿到名单", reasoning_content="旧推理")
            for key, value in extra.items():
                setattr(response, key, value)
            await self.bot.on_model_response(old, response)
            await self.bot.on_model_done(old, NS(messages=[]), response)
            self.assertEqual(response.completion_text, "")
            self.assertIsNone(response.result_chain)
            self.assertEqual(response.reasoning_content, "")
        old.bot.call_action.assert_not_awaited()
        self.assertEqual(len(self.models), 1)

    async def test_obsolete_final_is_not_saved_as_conversation_evidence(self):
        old = await self.model_event()
        await self.use_dst(old)
        await self.dispatch(self.event([C.At(qq="456"), C.Plain("查人")]))
        context = NS(messages=[Message(role="assistant", content=[TextPart(text="旧查询失败"), ThinkPart(think="旧结论")])])
        response = LLMResponse(role="assistant", completion_text="旧查询失败")
        await self.bot.on_model_done(old, context, response)
        stored = context.messages[-1].content
        self.assertFalse(any(isinstance(part, ThinkPart) for part in stored))
        self.assertNotIn("旧查询失败", stored[-1].text)
        self.assertIn("后续请求替代", stored[-1].text)
        self.assertEqual(response.completion_text, "")

    async def test_plugin_reload_stops_its_dst_runs_and_keeps_unrelated_chat(self):
        old = await self.model_event()
        await self.use_dst(old)
        chat = await self.model_event("今天天气真好", user="999")
        await self.bot.on_model_tool(chat, NS(name="weather"), {})
        await self.bot.terminate()
        self.assertTrue(old.is_stopped())
        self.assertFalse(chat.is_stopped())
        self.assertEqual(len(self.bot.model_presentation.active), 0)

    async def test_pending_model_stops_before_its_first_dst_call(self):
        old = await self.model_event("你怎么看这件事")
        await self.dispatch(self.event([C.At(qq="456"), C.Plain("查人")]))
        self.assertTrue(old.is_stopped())
        await self.use_dst(old)
        self.assertTrue(old.is_stopped())
        self.bot.tasks.find = AsyncMock()
        result = json.loads(await self.bot.tasks.perform(old, "dst_find", query="在线玩家"))
        self.assertEqual(result["state"], "superseded")
        self.bot.tasks.find.assert_not_awaited()

    async def test_late_model_request_cannot_reactivate_old_dst_event(self):
        old = await self.model_event("AI: 谁在线")
        receipt = self.bot.model_presentation.get(old)
        await self.dispatch(self.event([C.At(qq="456"), C.Plain("查人")]))
        await self.bot.on_model_request(old, NS(system_prompt="原人格", func_tool=None))
        self.assertIs(self.bot.model_presentation.get(old), receipt)
        self.assertTrue(receipt.superseded)
        self.assertTrue(old.is_stopped())

    async def test_other_user_or_session_never_stops_ongoing_query(self):
        old = await self.model_event()
        await self.use_dst(old)
        await self.dispatch(self.event([C.At(qq="456"), C.Plain("查人")], user="999"))
        self.bot.config["allowed_sessions"].append("adapter:GroupMessage:other")
        await self.dispatch(self.event([C.At(qq="456"), C.Plain("查人")], session="other"))
        self.assertFalse(old.is_stopped())
        self.assertFalse(old.get_extra("agent_stop_requested"))

    async def test_unrelated_chat_is_not_cancelled_or_reformatted(self):
        old = await self.model_event("今天天气真好")
        await self.bot.on_model_tool(old, NS(name="weather"), {})
        await self.dispatch(self.event([C.At(qq="456"), C.Plain("查人")]))
        response = LLMResponse(role="assistant", completion_text="**普通聊天**")
        await self.bot.on_model_response(old, response)
        self.assertEqual(response.completion_text, "**普通聊天**")
        self.assertFalse(old.is_stopped())

    async def test_tracking_is_weak_bounded_and_removed_on_completion(self):
        presentation = self.bot.model_presentation
        event = await self.model_event()
        key = (self.bot.actor(event).key, str(event.message_obj.message_id))
        self.assertIn(key, presentation.active)
        await self.bot.on_model_done(event, NS(messages=[]), LLMResponse(role="assistant", completion_text="完成"))
        self.assertNotIn(key, presentation.active)
        for _ in range(280):
            transient = self.event([C.At(qq="456"), C.Plain("普通聊天")])
            presentation.begin(transient, self.bot.actor(transient), "source", "普通聊天", {})
        self.assertLessEqual(len(presentation.active), 256)
        del transient
        gc.collect()
        presentation.supersede(self.bot.actor(event).key, "next-message")
        self.assertEqual(len(presentation.active), 0)

    async def test_real_runner_cancels_blocked_provider_and_sends_no_stale_reply(self):
        old = await self.model_event()
        entered, cancelled = asyncio.Event(), asyncio.Event()

        async def blocked_model(**kwargs):
            entered.set()
            try:
                await asyncio.Event().wait()
            finally:
                cancelled.set()

        bot = self.bot

        class Hooks(BaseAgentRunHooks):
            async def on_agent_done(self, context, response):
                await bot.on_model_response(old, response)
                await bot.on_model_done(old, context, response)

        provider = NS(provider_config={"id": "fixture", "max_context_tokens": 0}, text_chat=AsyncMock(side_effect=blocked_model))
        runner = ToolLoopAgentRunner()
        await runner.reset(provider=provider, request=ProviderRequest(prompt="fixture"),
                           run_context=ContextWrapper(context=NS(event=old)),
                           tool_executor=NS(execute=AsyncMock()), agent_hooks=Hooks())
        output = []

        async def consume():
            async for chain in run_agent(runner, show_tool_use=False):
                if chain:
                    output.append(chain)

        task = asyncio.create_task(consume())
        try:
            await asyncio.wait_for(entered.wait(), 2)
            shown = await self.dispatch(self.event([C.At(qq="456"), C.Plain("查人")]))
            self.assertIn("在线 1", shown[-1])
            await asyncio.wait_for(task, 2)
            self.assertTrue(cancelled.is_set())
            self.assertEqual(output, [])
            self.assertEqual(runner.final_llm_resp.completion_text, "")
            provider.text_chat.assert_awaited_once()
            runner.tool_executor.execute.assert_not_awaited()
            old.bot.call_action.assert_not_awaited()
        finally:
            if not task.done():
                task.cancel()
            await asyncio.gather(task, return_exceptions=True)

    async def test_submitted_write_remains_recoverable_after_model_stops(self):
        from astrbot_plugin_dst_companion.gateway import Gateway
        from astrbot_plugin_dst_companion.jobs import Actions
        from astrbot_plugin_dst_companion.state import Target
        from test_companion import mcp

        old = await self.model_event("AI: 给示例玩家一个草")
        await self.use_dst(old, "dst_run")
        entered, finish = asyncio.Event(), asyncio.Event()
        sent = []

        async def transport(name, args, timeout, write):
            self.assertTrue(write)
            sent.append(args)
            entered.set()
            await finish.wait()
            return mcp({"status": "succeeded"})

        gateway = Gateway(transport, AsyncMock())
        actor = self.bot.actor(old)
        records = {}

        async def load():
            return copy.deepcopy(records)

        async def save(rows):
            records.clear()
            records.update(copy.deepcopy(rows))

        async def execute(who, item, room, parameters, deadline, request_id):
            return await gateway.call(who, "fixture_write", {"requestId": request_id}, deadline, write=True)

        actions = Actions(gateway, load, save, directory=NS(execute=execute))
        item = {"id": "player.give_item", "schemaRevision": "fixture", "nativeTools": [],
                "parameters": {"type": "object", "properties": {}}}
        task = asyncio.create_task(actions.submit_capability(actor, item, Target("r1"), {}, time.monotonic() + 5))
        try:
            await asyncio.wait_for(entered.wait(), 2)
            await self.dispatch(self.event([C.At(qq="456"), C.Plain("查人")]))
            self.assertTrue(old.is_stopped())
            task.cancel()  # What AstrBot does to its in-flight tool waiter.
            await asyncio.gather(task, return_exceptions=True)
            self.assertEqual(len(records), 1)
            record = next(iter(records.values()))
            self.assertEqual(record["state"], "uncertain")
            self.assertTrue(record["requestId"])
            pending = list(gateway.pending.values())
            self.assertEqual(len(pending), 1)
            self.assertFalse(pending[0].cancelled())
            finish.set()
            await asyncio.wait_for(asyncio.gather(*pending), 2)
            self.assertEqual(len(sent), 1)  # No retry or cancellation request.
            self.assertEqual(sent[0]["requestId"], record["requestId"])
        finally:
            finish.set()
            await actions.close()
            await gateway.close()
