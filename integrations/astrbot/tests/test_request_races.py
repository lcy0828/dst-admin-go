"""Local review probes: all QQ, model and panel I/O use existing test fixtures."""

import asyncio
import json
import sys
import time
import unittest
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import test_qq_pipeline as pipeline
from astrbot.api import message_components as C
from astrbot.core.provider.entities import LLMResponse
from test_generic_operations import install_test_storage, contract


class GenericRequestRaceTests(unittest.IsolatedAsyncioTestCase):
    asyncSetUp = pipeline.QQPipelineTests.asyncSetUp
    asyncTearDown = pipeline.QQPipelineTests.asyncTearDown
    event = pipeline.QQPipelineTests.event
    dispatch = pipeline.QQPipelineTests.dispatch

    async def model_event(self, text, tool=True):
        event = self.event([C.At(qq="456"), C.Plain(text)])
        await self.dispatch(event)
        self.assertFalse(event.is_stopped())
        self.assertIn(event, self.models)
        await self.bot.on_model_request(event, NS(system_prompt="fixture", func_tool=None))
        if tool:
            await self.bot.on_model_tool(event, NS(name="dst_find"), {})
        return event

    async def test_new_setup_command_should_retire_old_dst_model(self):
        old = await self.model_event("分析一下服务器运行状态")
        output = await self.dispatch(self.event([C.Plain("/dst")]))
        self.assertIn("DST 助手设置", output[-1])
        response = LLMResponse(role="assistant", completion_text="旧查询失败")
        await self.bot.on_model_response(old, response)
        self.assertTrue(old.is_stopped())
        self.assertEqual(response.completion_text, "")

    async def test_natural_dst_question_before_first_tool_should_not_publish_old_failure(self):
        old = await self.model_event(
            "分析一下最近一天的 CPU 内存趋势，告诉我是否需要升级配置", tool=False
        )
        await self.dispatch(self.event([C.At(qq="456"), C.Plain("帮助")]))
        await self.dispatch(self.event([C.Plain("3")]))
        response = LLMResponse(role="assistant", completion_text="面板查询被中断，没拿到在线名单。")
        await self.bot.on_model_response(old, response)
        self.assertEqual(response.completion_text, "")

    async def test_late_generic_input_question_should_not_restore_old_form(self):
        old = await self.model_event("分析一下某个世界的情况")
        install_test_storage(self.bot)
        who = self.bot.actor(old)
        card = contract("inspectFixture")
        self.bot.generic.card = AsyncMock(return_value=card)
        entered, release = asyncio.Event(), asyncio.Event()

        async def parameters(*args):
            entered.set()
            await release.wait()
            return {}, {
                "state": "needs_input",
                "summary": "旧世界选择",
                "field": "path.worldId",
                "choices": [{"id": "m", "name": "Master"}],
            }

        self.bot.generic.parameters = parameters
        task = asyncio.create_task(
            self.bot.generic.perform(old, who, "dst_read", {"capability_id": card["id"]})
        )
        try:
            await asyncio.wait_for(entered.wait(), 2)
            await self.dispatch(self.event([C.At(qq="456"), C.Plain("帮助")]))
            self.assertIsNone(self.bot.state.pending_form(who.key))
            release.set()
            await asyncio.wait_for(task, 2)
            form = self.bot.state.pending_form(who.key)
            self.assertIsNone(form)
        finally:
            release.set()
            await asyncio.gather(task, return_exceptions=True)

    async def test_superseded_write_should_not_start_transport_after_journal_wait(self):
        self.bot.config["permission_mode"] = "group"
        self.bot.config["access_policy"] = "simple"
        self.ctx.astrbot_config["admins_id"] = ["123"]
        old = await self.model_event("重启房间前检查并准备操作")
        records = install_test_storage(self.bot)
        who = self.bot.actor(old)
        self.assertIn(who.role, {"game-manage", "panel-manage"})
        card = contract("restartFixture", "write")
        self.bot.generic.card = AsyncMock(return_value=card)
        self.bot.generic.parameters = AsyncMock(return_value=({}, None))
        entered, release = asyncio.Event(), asyncio.Event()
        reserve = self.bot.generic.reserve

        async def delayed_reserve(*args):
            fresh = await reserve(*args)
            entered.set()
            await release.wait()
            return fresh

        self.bot.generic.reserve = delayed_reserve
        self.bot.generic.request = AsyncMock(
            return_value={"state": "succeeded", "result": {"data": None}}
        )
        task = asyncio.create_task(
            self.bot.generic.perform(old, who, "dst_run", {"capability_id": card["id"]})
        )
        try:
            await asyncio.wait_for(entered.wait(), 2)
            self.bot.generic.request.assert_not_awaited()
            await self.dispatch(self.event([C.At(qq="456"), C.Plain("帮助")]))
            self.assertTrue(old.is_stopped())
            release.set()
            await asyncio.wait_for(task, 2)
            self.bot.generic.request.assert_not_awaited()
            journal = records["generic_requests_v1"]
            self.assertEqual(len(journal), 1)
            self.assertEqual(next(iter(journal.values()))["state"], "not_submitted")
            # The original message remains recorded: it cannot turn into a
            # fresh write if the same platform event is delivered again.
            request_id = next(iter(journal))
            self.assertFalse(await reserve(who, request_id, card["operationId"], {}))
        finally:
            release.set()
            await asyncio.gather(task, return_exceptions=True)

    async def test_late_generic_write_receipt_should_not_restore_old_confirmation(self):
        self.bot.config["permission_mode"] = "group"
        self.bot.config["access_policy"] = "simple"
        self.ctx.astrbot_config["admins_id"] = ["123"]
        old = await self.model_event("重启房间前检查并准备操作")
        records = install_test_storage(self.bot)
        who = self.bot.actor(old)
        self.assertIn(who.role, {"game-manage", "panel-manage"})
        card = contract("restartFixture", "write")
        self.bot.generic.card = AsyncMock(return_value=card)
        self.bot.generic.parameters = AsyncMock(return_value=({}, None))
        entered, release = asyncio.Event(), asyncio.Event()

        async def request(*args, **kwargs):
            entered.set()
            await release.wait()
            return {
                "state": "needs_confirmation",
                "taskRef": "synthetic-confirmation",
                "confirmation": "standard",
            }

        self.bot.generic.request = request
        task = asyncio.create_task(
            self.bot.generic.perform(old, who, "dst_run", {"capability_id": card["id"]})
        )
        try:
            await asyncio.wait_for(entered.wait(), 2)
            await self.dispatch(self.event([C.At(qq="456"), C.Plain("帮助")]))
            self.assertIsNone(self.bot.state.pending_form(who.key))
            release.set()
            await asyncio.wait_for(task, 2)
            form = self.bot.state.pending_form(who.key)
            self.assertIsNone(form)
            self.assertEqual(
                next(iter(records["generic_requests_v1"].values()))["state"], "answered"
            )
        finally:
            release.set()
            await asyncio.gather(task, return_exceptions=True)

    async def write_actor(self):
        self.bot.config["permission_mode"] = "group"
        self.bot.config["access_policy"] = "simple"
        self.ctx.astrbot_config["admins_id"] = ["123"]
        old = await self.model_event("分析后准备重启操作")
        self.bot.generic.available = AsyncMock(return_value=True)
        self.bot.gateway.authorize = AsyncMock()
        return old, self.bot.actor(old)

    async def replace_request(self):
        event = self.event([C.At(qq="456"), C.Plain("帮助")])
        self.bot.claim_request(self.bot.actor(event), event)

    async def test_write_in_gateway_queue_is_rejected_before_transport(self):
        from astrbot_plugin_dst_companion.gateway import QueryError

        old, who = await self.write_actor()
        transport = AsyncMock()
        self.bot.gateway.transport = transport
        await self.bot.gateway.limit.acquire()
        await self.bot.gateway.limit.acquire()
        task = asyncio.create_task(
            self.bot.generic.request(who, "run", operationId="fixture", arguments={})
        )
        try:
            for _ in range(20):
                if self.bot.gateway.pending:
                    break
                await asyncio.sleep(0)
            self.assertTrue(self.bot.gateway.pending)
            await self.replace_request()
            self.assertTrue(old.is_stopped())
            self.bot.gateway.limit.release()
            with self.assertRaises(QueryError) as error:
                await asyncio.wait_for(task, 2)
            self.assertEqual(error.exception.code, "REQUEST_SUPERSEDED")
            transport.assert_not_awaited()
        finally:
            self.bot.gateway.limit.release()
            await asyncio.gather(task, return_exceptions=True)

    async def test_write_revalidates_after_role_authorization(self):
        from astrbot_plugin_dst_companion.gateway import QueryError

        old, who = await self.write_actor()
        entered, release = asyncio.Event(), asyncio.Event()

        async def authorize(*args):
            entered.set()
            await release.wait()

        self.bot.gateway.authorize = authorize
        self.bot.gateway.transport = AsyncMock()
        task = asyncio.create_task(self.bot.generic.request(who, "execute", taskRef="fixture"))
        try:
            await asyncio.wait_for(entered.wait(), 2)
            await self.replace_request()
            release.set()
            with self.assertRaises(QueryError) as error:
                await asyncio.wait_for(task, 2)
            self.assertEqual(error.exception.code, "REQUEST_SUPERSEDED")
            self.bot.gateway.transport.assert_not_awaited()
            self.assertTrue(old.is_stopped())
        finally:
            release.set()
            await asyncio.gather(task, return_exceptions=True)

    async def test_transport_refresh_does_not_send_a_superseded_write(self):
        from astrbot_plugin_dst_companion.gateway import QueryError

        old, who = await self.write_actor()
        entered, release = asyncio.Event(), asyncio.Event()
        self.bot.config["panel_url"] = "https://panel.example/mcp"
        self.bot.config["panel_token"] = "synthetic-test-credential"

        async def refresh():
            entered.set()
            await release.wait()

        self.bot.private_panel.refresh = refresh
        self.bot.private_panel.full_access = True
        self.bot.private_panel.features = {"companion-gateway-v1"}
        self.bot.private_panel.generic_request = AsyncMock()
        self.bot.gateway.transport = self.bot.transport
        task = asyncio.create_task(
            self.bot.generic.request(who, "run", operationId="fixture", arguments={})
        )
        try:
            await asyncio.wait_for(entered.wait(), 2)
            # Use the already resolved actor; changing the fixture connection
            # must not require a new handshake or a real network request.
            event = self.event([C.At(qq="456"), C.Plain("帮助")])
            self.bot.claim_request(who, event)
            release.set()
            with self.assertRaises(QueryError) as error:
                await asyncio.wait_for(task, 2)
            self.assertEqual(error.exception.code, "REQUEST_SUPERSEDED")
            self.bot.private_panel.generic_request.assert_not_awaited()
            self.assertTrue(old.is_stopped())
        finally:
            release.set()
            await asyncio.gather(task, return_exceptions=True)

    async def test_submitted_generic_write_survives_waiter_cancel_and_recovers_original_id(self):
        from test_companion import mcp

        old, who = await self.write_actor()
        records = install_test_storage(self.bot)
        card = contract("restartFixture", "write")
        self.bot.generic.card = AsyncMock(return_value=card)
        self.bot.generic.parameters = AsyncMock(return_value=({}, None))
        entered, release = asyncio.Event(), asyncio.Event()
        sent = []

        async def transport(name, args, timeout, write):
            sent.append(args)
            if args["mode"] == "run":
                self.assertTrue(write)
                entered.set()
                await release.wait()
            else:
                self.assertEqual(args["mode"], "recover")
                self.assertFalse(write)
            return mcp(
                {
                    "state": "succeeded",
                    "taskRef": "original-task",
                    "result": {"data": {"complete": True}},
                }
            )

        self.bot.gateway.transport = transport
        task = asyncio.create_task(
            self.bot.generic.perform(old, who, "dst_run", {"capability_id": card["id"]})
        )
        try:
            await asyncio.wait_for(entered.wait(), 2)
            await self.replace_request()
            self.assertTrue(old.is_stopped())
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)
            self.assertEqual(len(sent), 1)
            request_id = sent[0]["requestId"]
            self.assertEqual(records["generic_requests_v1"][request_id]["state"], "reserved")
            pending = list(self.bot.gateway.pending.values())
            self.assertEqual(len(pending), 1)
            self.assertFalse(pending[0].cancelled())
            release.set()
            await asyncio.wait_for(asyncio.gather(*pending), 2)
            current = self.event([C.At(qq="456"), C.Plain("查询原任务")])
            current_actor = self.bot.actor(current)
            self.bot.claim_request(current_actor, current)
            recovered = json.loads(
                await self.bot.generic.perform(
                    current,
                    current_actor,
                    "dst_result",
                    {"task_ref": "generic-request:" + request_id},
                )
            )
            self.assertEqual(recovered["state"], "succeeded")
            self.assertEqual(recovered["taskRef"], "generic:original-task")
            self.assertEqual([args["mode"] for args in sent], ["run", "recover"])
            self.assertEqual(sent[1]["requestId"], request_id)
            self.assertIsNone(self.bot.state.pending_form(who.key))
        finally:
            release.set()
            await asyncio.gather(task, return_exceptions=True)

    async def test_submission_guards_are_isolated_between_concurrent_calls_and_reads(self):
        from dataclasses import replace
        from astrbot_plugin_dst_companion.gateway import Gateway, QueryError, check_submission
        from test_companion import actor, mcp

        entered, release = asyncio.Event(), asyncio.Event()
        arrivals = []
        invalid = False

        def guard_first():
            if invalid:
                raise QueryError("REQUEST_SUPERSEDED")

        async def transport(name, args, timeout, write):
            arrivals.append(name)
            if len(arrivals) == 3:
                entered.set()
            await release.wait()
            check_submission()
            return mcp({"name": name})

        gateway = Gateway(transport, AsyncMock(), limit=3)
        who = actor()
        callers = [replace(who, key=(*who.key[:-1], str(i))) for i in range(3)]
        tasks = [
            asyncio.create_task(
                gateway.call(
                    callers[i],
                    name,
                    {},
                    time.monotonic() + 5,
                    write=i < 2,
                    guard=guard_first if i == 0 else (lambda: None) if i == 1 else None,
                )
            )
            for i, name in enumerate(("old_write", "other_write", "read"))
        ]
        try:
            await asyncio.wait_for(entered.wait(), 2)
            invalid = True
            release.set()
            results = await asyncio.gather(*tasks, return_exceptions=True)
            self.assertIsInstance(results[0], QueryError)
            self.assertEqual(results[0].code, "REQUEST_SUPERSEDED")
            self.assertEqual(results[1], {"name": "other_write"})
            self.assertEqual(results[2], {"name": "read"})
        finally:
            release.set()
            await asyncio.gather(*tasks, return_exceptions=True)
            await gateway.close()


if __name__ == "__main__":
    unittest.main()
