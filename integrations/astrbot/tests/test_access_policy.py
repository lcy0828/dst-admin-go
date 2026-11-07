"""Simple access policy and one confirmation policy for every entry point."""

import copy
import unittest
from dataclasses import replace
from unittest.mock import AsyncMock

from test_companion import actor
from test_v41 import Panel, capability

from astrbot_plugin_dst_companion.capabilities import Directory, permitted
from astrbot_plugin_dst_companion.decision import (
    confirmation,
    confirmation_reply,
    looks_like_confirmation,
    tier,
)
from astrbot_plugin_dst_companion.engine import Engine
from astrbot_plugin_dst_companion.gateway import Gateway
from astrbot_plugin_dst_companion.jobs import Actions
from astrbot_plugin_dst_companion.policy import Policy
from astrbot_plugin_dst_companion.routing import Intent
from astrbot_plugin_dst_companion.state import State, Target

PLAYER = {"type": "object", "properties": {"player_id": {"type": "string", "maxLength": 128}},
          "required": ["player_id"], "additionalProperties": False}
WORLD = {"type": "object", "properties": {"days": {"type": "integer", "minimum": 1, "maximum": 10},
                                          "worldId": {"type": "string", "maxLength": 255}},
         "required": ["days", "worldId"], "additionalProperties": False}
RESET = {"type": "object", "properties": {
    "backup": {"type": "boolean", "default": True},
    "confirmation": {"type": "string", "minLength": 1, "maxLength": 255},
}, "required": ["confirmation"], "additionalProperties": False}


def item(id, schema, risk="", hints=None, title="操作"):
    value = capability(id, "write", copy.deepcopy(schema))
    value.update(title=title, risk=risk, parameterHints=hints or {})
    return value


def manager(role="game-manage"):
    return replace(actor(role=role), write_allowlist=("*",), workbench_mode="all")


class TierTests(unittest.TestCase):
    def test_tiers_follow_effect_not_entry_point(self):
        revive = item("player.resurrect", PLAYER, "low", {"player_id": "player"})
        skip = item("workbench.skip_days", WORLD, "high", {"worldId": "world"})
        reset = item("room.reset", RESET)
        wipe = item("workbench.clear_player_inventory", PLAYER, "critical", {"player_id": "player"})
        self.assertEqual((tier(revive), confirmation(revive)), ("R1", "none"))
        self.assertEqual((tier(skip), confirmation(skip)), ("R2", "once"))
        self.assertEqual((tier(reset), confirmation(reset)), ("R3", "typed"))
        self.assertEqual((tier(wipe), confirmation(wipe)), ("R3", "typed"))
        self.assertEqual(tier(capability()), "R0")

    def test_server_metadata_wins(self):
        skip = item("workbench.skip_days", WORLD, "high", {"worldId": "world"})
        skip.update(tier="R2", confirm="none")
        self.assertEqual(confirmation(skip), "none")

    def test_typed_confirmation_and_backup_choice(self):
        form = {"confirm": "typed", "phrase": "确认重置", "item": {"id": "room.reset"}}
        self.assertEqual(confirmation_reply("确认重置", form), {})
        self.assertEqual(confirmation_reply("确认重置。", form), {})
        self.assertEqual(confirmation_reply("确认重置 不备份", form), {"backup": False})
        self.assertEqual(confirmation_reply("确认重置不要备份", form), {"backup": False})
        self.assertEqual(confirmation_reply("确认重置 备份", form), {"backup": True})
        self.assertEqual(confirmation_reply("确认重置 测试服", form, "测试服"), {})
        self.assertIsNone(confirmation_reply("确认", form))
        self.assertIsNone(confirmation_reply("确认重置 随便", form))
        once = {"confirm": "once"}
        self.assertEqual(confirmation_reply("确认", once), {})
        self.assertIsNone(confirmation_reply("确认重置", once))
        self.assertTrue(looks_like_confirmation("确认重置 不要备份"))
        self.assertFalse(looks_like_confirmation("重置世界"))


class AccessTests(unittest.TestCase):
    def test_everyone_reads_and_admins_write_everything(self):
        member = replace(actor(), visibility=frozenset({"status", "performance", "world", "backups"}),
                         write_allowlist=(), workbench_mode="off")
        perf = capability("room.performance")
        perf["category"] = "performance"
        wipe = item("workbench.clear_player_inventory", PLAYER, "critical", {"player_id": "player"})
        reset = item("room.reset", RESET)
        self.assertTrue(permitted(member, perf))
        self.assertFalse(permitted(member, reset))
        for value in (reset, wipe):
            self.assertTrue(permitted(manager(), value))
            self.assertTrue(permitted(manager(), value, {"player_id": "KU_1"}))
        denied = replace(manager(), disabled_capabilities=("room.reset",))
        self.assertFalse(permitted(denied, reset))
        self.assertTrue(manager().may_write("room.restart"))
        self.assertFalse(replace(actor(), write_allowlist=("*",)).may_write("room.restart"))


class FlowTests(unittest.IsolatedAsyncioTestCase):
    async def fixture(self, *items):
        panel = Panel()
        panel.items = list(items)
        policy = Policy(AsyncMock(return_value=True))
        gateway = Gateway(panel.transport, policy.authorize)
        directory = Directory(gateway)
        policy.directory = directory
        records = {}

        async def load():
            return copy.deepcopy(records)

        async def save(value):
            records.clear()
            records.update(copy.deepcopy(value))

        actions = Actions(gateway, load, save, directory=directory)
        self.addAsyncCleanup(actions.close)
        self.addAsyncCleanup(gateway.close)
        return panel, Engine(gateway, State(), directory=directory, actions=actions)

    def submitted(self, panel):
        return [args for name, args in panel.calls if name == "api_runBotCapability"]

    async def test_reset_requires_typed_phrase_and_can_skip_backup(self):
        panel, engine = await self.fixture(item("room.reset", RESET, title="重置全部世界"))
        admin = manager()
        intent = Intent("capability", capability="room.reset", parameters={})
        result = await engine.execute(admin, intent, Target("r1"))
        self.assertEqual(result.outcome, "needs_selection")
        self.assertIn("确认重置", result.text)
        self.assertIn("先创建重置前备份", result.text)
        self.assertNotIn("confirmation", result.text)
        await engine.handle(admin, "确认", woken=True)
        self.assertEqual(panel.executions, 0)
        result = await engine.handle(admin, "确认重置 不备份", woken=True)
        self.assertEqual(panel.executions, 1, result.text)
        body = self.submitted(panel)[0]["body"]["parameters"]
        self.assertEqual(body, {"backup": False, "confirmation": "房间一"})

    async def test_reset_defaults_to_backup(self):
        panel, engine = await self.fixture(item("room.reset", RESET, title="重置全部世界"))
        admin = manager()
        await engine.execute(admin, Intent("capability", capability="room.reset", parameters={}), Target("r1"))
        await engine.handle(admin, "确认重置", woken=True)
        self.assertEqual(self.submitted(panel)[0]["body"]["parameters"], {"backup": True, "confirmation": "房间一"})

    async def test_model_requested_no_backup_is_shown_before_confirmation(self):
        panel, engine = await self.fixture(item("room.reset", RESET, title="重置全部世界"))
        admin = manager()
        result = await engine.execute(admin, Intent("capability", capability="room.reset",
                                                    parameters={"backup": False}), Target("r1"))
        self.assertIn("不备份", result.text)
        self.assertEqual(panel.executions, 0)

    async def test_room_wide_action_confirms_once(self):
        panel, engine = await self.fixture(item("workbench.skip_days", WORLD, "high", {"worldId": "world"},
                                                title="跳过天数"))
        admin = manager()
        intent = Intent("capability", capability="workbench.skip_days", parameters={"days": 1, "worldId": "Master"})
        result = await engine.execute(admin, intent, Target("r1"))
        self.assertIn("回复「确认」", result.text)
        self.assertEqual(panel.executions, 0)
        await engine.handle(admin, "确认", woken=True)
        self.assertEqual(panel.executions, 1)

    async def test_single_player_action_runs_without_confirmation_on_every_path(self):
        revive = item("player.resurrect", PLAYER, "low", {"player_id": "player"}, title="复活玩家")
        panel, engine = await self.fixture(revive)
        admin = manager()
        await engine.execute(admin, Intent("capability", capability="player.resurrect",
                                           parameters={"player_id": "KU_TEST123"}), Target("r1"))
        self.assertEqual(panel.executions, 1)
        self.assertIsNone(engine.state.pending_form(admin.key))

    async def test_quoted_confirmation_accepts_model_relay_but_not_other_messages(self):
        skip = item("workbench.skip_days", WORLD, "high", {"worldId": "world"}, title="跳过天数")
        intent = Intent("capability", capability="workbench.skip_days", parameters={"days": 1, "worldId": "Master"})
        # The prompt is known (sent by the plugin): quoting another message never confirms.
        panel, engine = await self.fixture(copy.deepcopy(skip))
        admin = manager()
        await engine.execute(admin, intent, Target("r1"))
        engine.state.form_sent(admin.key, "prompt-1", engine.state.pending_form(admin.key))
        await engine.handle(admin, "确认", woken=True, quote="death-notice")
        self.assertEqual(panel.executions, 0)
        await engine.handle(admin, "确认", woken=True, quote="prompt-1")
        self.assertEqual(panel.executions, 1)
        # The model relayed the prompt (unknown message id): quoting the relay confirms.
        panel, engine = await self.fixture(copy.deepcopy(skip))
        await engine.execute(admin, intent, Target("r1"))
        engine.state.pending_form(admin.key)["relayed"] = True
        await engine.handle(admin, "确认", woken=True, quote="model-relay")
        self.assertEqual(panel.executions, 1)

    async def test_member_cannot_trigger_management(self):
        panel, engine = await self.fixture(item("room.reset", RESET, title="重置全部世界"))
        member = replace(actor(), write_allowlist=(), workbench_mode="off")
        result = await engine.execute(member, Intent("capability", capability="room.reset", parameters={}), Target("r1"))
        self.assertNotEqual(result.outcome, "needs_selection")
        self.assertEqual(panel.executions, 0)


if __name__ == "__main__":
    unittest.main()
