"""Run by the Go HTTP fixture with an ephemeral synthetic service credential."""

import json
import os
import unittest

import test_task_tools as adapter
from test_generic_operations import install_test_storage


@unittest.skipUnless(os.getenv("DST_TEST_GENERIC_URL"), "requires Go HTTP fixture")
class FrozenPluginHTTPTests(unittest.IsolatedAsyncioTestCase):
    def companion(self):
        return adapter.TaskAdapterTests.companion(self)

    def event(self, role="maintainer"):
        return adapter.TaskAdapterTests.event(self, role)

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
