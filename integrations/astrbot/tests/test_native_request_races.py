"""Real framework dispatch and send boundaries; no QQ, model or game I/O."""

import asyncio
import time
import unittest
from dataclasses import replace
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock

import test_qq_pipeline as pipeline
from astrbot.api import message_components as C
from astrbot_plugin_dst_companion.gateway import Gateway, QueryError, check_submission
from astrbot_plugin_dst_companion.jobs import Actions
from astrbot_plugin_dst_companion.routing import Intent
from astrbot_plugin_dst_companion.state import Target
from test_companion import actor, mcp
from test_generic_operations import install_test_storage
from test_v41 import Panel
import test_workbench as workbench
from test_workbench import recipe


class NativeRequestRaceTests(unittest.IsolatedAsyncioTestCase):
    asyncSetUp = pipeline.QQPipelineTests.asyncSetUp
    asyncTearDown = pipeline.QQPipelineTests.asyncTearDown
    event = pipeline.QQPipelineTests.event
    dispatch = pipeline.QQPipelineTests.dispatch

    async def native_fixture(self, model=False):
        self.bot.config.update(permission_mode="group", access_policy="simple")
        self.ctx.astrbot_config["admins_id"] = ["123"]
        storage = install_test_storage(self.bot)
        old = self.event([C.At(qq="456"), C.Plain(
            "分析后复活 KU_EXAMPLE" if model else "复活 KU_EXAMPLE")])
        if model:
            await self.dispatch(old)
            await self.bot.on_model_request(old, NS(system_prompt="fixture", func_tool=None))
            await self.bot.on_model_tool(old, NS(name="dst_run"), {})
        item = recipe("player.resurrect", "low")
        self.bot.directory.seed(self.bot.actor(old), item)

        async def get(who, *args):
            self.bot.directory.seed(who, item)
            return item

        self.bot.directory.get = get
        wire = Panel()
        original = self.bot.gateway.transport

        async def transport(name, args, timeout, write):
            if name in {"api_runBotCapability", "api_getBotCapabilityResult"}:
                return await wire.transport(name, args, timeout, write)
            return await original(name, args, timeout, write)

        self.bot.gateway.transport = transport
        return old, item, wire, storage

    async def test_cancelled_direct_action_never_sends_after_submit_lock(self):
        old, item, wire, storage = await self.native_fixture()
        entered = asyncio.Event()
        actions = self.bot.engine.actions
        original = actions.submit_capability

        async def submit(*args, **kwargs):
            entered.set()
            return await original(*args, **kwargs)

        actions.submit_capability = submit
        await actions.lock.acquire()
        task = asyncio.create_task(self.dispatch(old))
        try:
            await asyncio.wait_for(entered.wait(), 2)
            self.assertEqual(wire.executions, 0)
            reply = await self.dispatch(self.event([C.At(qq="456"), C.Plain("取消")]))
            self.assertIn("已取消", reply[-1])
            actions.lock.release()
            await asyncio.wait_for(task, 2)
            self.assertEqual(wire.executions, 0)
            record = next(iter(storage["actions_v1"].values()))
            self.assertEqual(record["state"], "not_submitted")
            self.assertFalse(record["data"]["mayHaveExecuted"])
            request_id = record["requestId"]
            # Duplicate platform delivery must read the local rejection, not
            # query a nonexistent server result or generate a new write.
            text = await original(self.bot.actor(old), item, Target("r1"),
                                  {"player_id": "KU_EXAMPLE"}, time.monotonic() + 5)
            self.assertIn("尚未发送", text)
            self.assertEqual(next(iter(storage["actions_v1"].values()))["requestId"], request_id)
            self.assertFalse(wire.calls)
        finally:
            if actions.lock.locked():
                actions.lock.release()
            await asyncio.gather(task, return_exceptions=True)

    async def test_superseded_model_fixed_action_never_sends_after_submit_lock(self):
        old, _, wire, storage = await self.native_fixture(model=True)
        entered = asyncio.Event()
        actions = self.bot.engine.actions
        original = actions.submit_capability

        async def submit(*args, **kwargs):
            entered.set()
            return await original(*args, **kwargs)

        actions.submit_capability = submit
        await actions.lock.acquire()
        task = asyncio.create_task(self.bot.tasks.perform(
            old, "dst_run", capability_id="player.resurrect", room="r1", player="KU_EXAMPLE"))
        try:
            await asyncio.wait_for(entered.wait(), 2)
            await self.dispatch(self.event([C.At(qq="456"), C.Plain("帮助")]))
            self.assertTrue(old.is_stopped())
            actions.lock.release()
            await asyncio.wait_for(task, 2)
            self.assertEqual(wire.executions, 0)
            self.assertEqual(next(iter(storage["actions_v1"].values()))["state"], "not_submitted")
            self.assertIsNone(self.bot.state.pending_form(self.bot.actor(old).key))
        finally:
            if actions.lock.locked():
                actions.lock.release()
            await asyncio.gather(task, return_exceptions=True)

    async def test_native_transport_refresh_rechecks_request_before_wire(self):
        old, item, _, _ = await self.native_fixture(model=True)
        who = self.bot.actor(old)
        entered, release = asyncio.Event(), asyncio.Event()
        self.bot.config.update(panel_url="https://panel.example/mcp", panel_token="synthetic-credential")

        async def refresh():
            entered.set()
            await release.wait()

        self.bot.private_panel.refresh = refresh
        self.bot.private_panel.full_access = True
        self.bot.private_panel.call_tool = AsyncMock()
        self.bot.gateway.transport = self.bot.transport
        task = asyncio.create_task(self.bot.gateway.call(
            who, "api_runBotCapability", {"path": {"roomId": "r1", "capabilityId": item["id"]},
                                          "body": {"parameters": {"player_id": "KU_EXAMPLE"}}},
            time.monotonic() + 5, write=True))
        try:
            await asyncio.wait_for(entered.wait(), 2)
            new = self.event([C.At(qq="456"), C.Plain("帮助")])
            self.bot.claim_request(who, new)
            release.set()
            with self.assertRaises(QueryError) as error:
                await asyncio.wait_for(task, 2)
            self.assertEqual(error.exception.code, "REQUEST_SUPERSEDED")
            self.bot.private_panel.call_tool.assert_not_awaited()
        finally:
            release.set()
            await asyncio.gather(task, return_exceptions=True)


class WriteGuardTests(unittest.IsolatedAsyncioTestCase):
    async def test_every_write_uses_shared_guard_after_queue_even_if_waiter_cancels(self):
        for name in ("api_runBotCapability", "api_executeRoomTeleport", "api_runRoomAction"):
            with self.subTest(name=name):
                valid = True

                def guard(who):
                    if not valid:
                        raise QueryError("REQUEST_SUPERSEDED")

                transport = AsyncMock()
                gateway = Gateway(transport, AsyncMock(), limit=1, write_guard=guard)
                await gateway.limit.acquire()
                task = asyncio.create_task(gateway.call(
                    actor(), name, {}, time.monotonic() + 5, write=True, guard=lambda: None))
                try:
                    for _ in range(20):
                        if gateway.pending:
                            break
                        await asyncio.sleep(0)
                    self.assertTrue(gateway.pending)
                    pending = list(gateway.pending.values())
                    task.cancel()
                    await asyncio.gather(task, return_exceptions=True)
                    self.assertFalse(pending[0].cancelled())
                    valid = False
                    gateway.limit.release()
                    result = await asyncio.wait_for(asyncio.gather(*pending, return_exceptions=True), 2)
                    self.assertIsInstance(result[0], QueryError)
                    self.assertEqual(result[0].code, "REQUEST_SUPERSEDED")
                    transport.assert_not_awaited()
                finally:
                    await gateway.close()

    async def test_teleport_and_legacy_rejections_remain_known_unsent_and_recoverable(self):
        for kind in ("teleport", "legacy"):
            with self.subTest(kind=kind):
                transport = AsyncMock()

                def guard(who):
                    raise QueryError("REQUEST_SUPERSEDED")

                gateway = Gateway(transport, AsyncMock(), write_guard=guard)
                records = {}
                actions = Actions(gateway, AsyncMock(side_effect=lambda: records.copy()),
                                  AsyncMock(side_effect=lambda values: records.update(values)))
                who = replace(actor(role="game-manage"), workbench_mode="standard")
                try:
                    with self.assertRaises(QueryError) as error:
                        if kind == "teleport":
                            await actions.submit_teleport(who, recipe(), Target("r1"),
                                                          {"id": "preview"}, time.monotonic() + 5)
                        else:
                            await actions.submit(who, Intent("restart"), Target("r1"), time.monotonic() + 5)
                    self.assertEqual(error.exception.code, "REQUEST_SUPERSEDED")
                    record = next(iter(records.values()))
                    self.assertEqual(record["state"], "not_submitted")
                    if kind == "teleport":
                        # Receipt policy is still checked before a local result.
                        gateway.authorize_receipt = AsyncMock()
                        actions.directory = NS(lookup=lambda *args: recipe())
                    self.assertIn("尚未发送", await actions.latest(who, "r1", time.monotonic() + 5))
                    transport.assert_not_awaited()
                finally:
                    await actions.close()
                    await gateway.close()

    async def test_sent_native_write_survives_cancel_and_recovers_original_request(self):
        panel, engine = await workbench.WorkbenchTests.fixture(self, recipe(risk="low"))
        engine.gateway.authorize_receipt = engine.gateway.authorize.__self__.authorize_receipt
        who = replace(actor(role="game-manage"), workbench_mode="standard")
        await engine.directory.items(who, time.monotonic() + 5)
        entered, release = asyncio.Event(), asyncio.Event()
        valid = True

        def guard(who):
            if not valid:
                raise QueryError("REQUEST_SUPERSEDED")

        original = engine.gateway.transport

        async def transport(name, args, timeout, write):
            check_submission()
            value = await original(name, args, timeout, write)
            if write:
                entered.set()
                await release.wait()
            return value

        engine.gateway.write_guard = guard
        engine.gateway.transport = transport
        task = asyncio.create_task(engine.actions.submit_capability(
            who, recipe(risk="low"), Target("r1"), {"player_id": "KU_EXAMPLE"}, time.monotonic() + 5))
        try:
            await asyncio.wait_for(entered.wait(), 2)
            request_id = next(iter(panel.requests))
            valid = False
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)
            record = next(iter((await engine.actions.load()).values()))
            self.assertEqual(record["state"], "uncertain")
            pending = list(engine.gateway.pending.values())
            release.set()
            await asyncio.wait_for(asyncio.gather(*pending), 2)
            await engine.actions.read(who, record, time.monotonic() + 5)
            self.assertEqual(panel.executions, 1)
            self.assertEqual([args["path"]["requestId"] for name, args in panel.calls
                              if name == "api_getBotCapabilityResult"], [request_id])
        finally:
            release.set()
            await asyncio.gather(task, return_exceptions=True)
