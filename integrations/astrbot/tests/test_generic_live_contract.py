"""Run by the Go HTTP fixture with an ephemeral synthetic service credential."""

import json
import os
import time
import unittest

import test_task_tools as adapter
from test_generic_operations import install_test_storage
from astrbot_plugin_dst_companion.engine import Engine
from astrbot_plugin_dst_companion.state import State
from astrbot_plugin_dst_companion.gateway import QueryError


@unittest.skipUnless(os.getenv("DST_TEST_GENERIC_URL"), "requires Go HTTP fixture")
class FrozenPluginHTTPTests(unittest.IsolatedAsyncioTestCase):
    def companion(self):
        return adapter.TaskAdapterTests.companion(self)

    def event(self, role="maintainer"):
        return adapter.TaskAdapterTests.event(self, role)

    async def test_history_and_offline_inventory_share_real_scoped_transport(self):
        bot, event = self.companion(), self.event("admin")
        event.is_admin = lambda: True
        bot.config.update(panel_url=os.environ["DST_TEST_GENERIC_URL"],
                          panel_token=os.environ["DST_TEST_GENERIC_TOKEN"], access_policy="simple")
        await bot.prepare(event)
        who = bot.actor(event)
        engine = Engine(bot.gateway, State(), reader=bot.generic.read_native)
        try:
            reply = await engine.handle(who, "查询示例玩家最近两天做了什么", woken=True)
            self.assertEqual(reply.outcome, "matched", reply.text)
            self.assertIn("采集", reply.text)
            self.assertIn("洞穴", reply.text)
            self.assertIn("采集缺口", reply.text)
            reply = await engine.handle(who, "查询示例玩家最近两天的移动轨迹", woken=True)
            self.assertEqual(reply.outcome, "matched", reply.text)
            self.assertIn("X 12.5 / Z -3.0", reply.text)
            self.assertIn("洞穴", reply.text)
            reply = await engine.handle(who, "查询示例玩家最近两天的战斗记录", woken=True)
            self.assertEqual(reply.outcome, "matched", reply.text)
            self.assertIn("伤害未确认", reply.text)
            reply = await engine.handle(who, "查询示例玩家背包", woken=True)
            self.assertEqual(reply.outcome, "needs_selection", reply.text)
            self.assertIn("多个背包存档", reply.text)
            reply = await engine.handle(who, "2")
            self.assertEqual(reply.outcome, "matched", reply.text)
            self.assertIn("存档快照", reply.text)
            self.assertIn("洞穴", reply.text)
            self.assertIn("草", reply.text)
            # The legacy MCP adapter retains the original error string while
            # exposing the same selectable details. No alternate retry occurs.
            with self.assertRaises(QueryError) as error:
                await bot.gateway.call(who, "api_getRoomPlayerInventory", {
                    "path": {"roomId": "r1", "playerId": "KU_SYNTH001"},
                    "query": {"source": "auto", "refresh": False}}, time.monotonic() + 8)
            self.assertEqual(error.exception.code, "SOURCE_AMBIGUOUS")
            self.assertEqual(len(error.exception.details["candidates"]), 2)
        finally:
            await bot.terminate()

    async def test_new_native_api_over_real_private_transport(self):
        bot, event = self.companion(), self.event("admin")
        install_test_storage(bot)
        event.is_admin = lambda: True
        bot.config.update(panel_url=os.environ["DST_TEST_GENERIC_URL"],
                          panel_token=os.environ["DST_TEST_GENERIC_TOKEN"], access_policy="simple")
        # This user is the synthetic fixture's configured bot maintainer.
        await bot.prepare(event)
        self.assertEqual(bot.actor(event).role, "panel-manage")
        try:
            found = json.loads(await bot.tasks.perform(event, "dst_find", query="readSprout"))
            self.assertEqual(found["items"][0]["id"], "api:readSprout")
            read = json.loads(await bot.tasks.perform(event, "dst_read", capability_id="api:readSprout"))
            self.assertEqual(read["result"]["data"], [False, 0, None, "synthetic"])
            bot.input = lambda _: ("修改合成植物设置", True, "", None)
            arguments = {"body": {"enabled": False, "nullable": None, "composite": 0}}
            result = json.loads(await bot.tasks.perform(event, "dst_run", capability_id="api:changeSprout", parameters=arguments))
            self.assertEqual(result["state"], "succeeded", result)
            self.assertEqual(result["result"]["httpStatus"], 204)
            again = json.loads(await bot.tasks.perform(event, "dst_run", capability_id="api:changeSprout", parameters=arguments))
            self.assertEqual(again["taskRef"], result["taskRef"])
            recovered = json.loads(await bot.tasks.perform(event, "dst_result", task_ref=result["taskRef"]))
            self.assertEqual(recovered["state"], "succeeded")
            bot.directory.items.assert_not_awaited()
        finally:
            await bot.terminate()
