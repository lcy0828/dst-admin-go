"""Deterministic management, live authorization and human confirmation; no game writes."""

import copy
import json
import unittest
from dataclasses import replace
from unittest.mock import AsyncMock

from test_companion import actor
from test_v41 import Panel, capability

from astrbot_plugin_dst_companion.capabilities import Directory, permitted
from astrbot_plugin_dst_companion.configuration import Configuration
from astrbot_plugin_dst_companion.engine import Engine
from astrbot_plugin_dst_companion.gateway import Gateway
from astrbot_plugin_dst_companion.jobs import Actions
from astrbot_plugin_dst_companion.model_tools import capability_page
from astrbot_plugin_dst_companion.policy import Policy
from astrbot_plugin_dst_companion.routing import Intent, parse
from astrbot_plugin_dst_companion.state import State, Target


def recipe(id="player.resurrect", risk="high"):
    item = capability(
        id,
        "write",
        {
            "type": "object",
            "properties": {"player_id": {"type": "string", "maxLength": 128}},
            "required": ["player_id"],
            "additionalProperties": False,
        },
    )
    item.update(
        title="复活玩家",
        risk=risk,
        resultKind="workbench-run",
        nativeTools=["api_executeBuiltinRoomCommand", "api_getBuiltinRoomCommandRun"],
        parameterHints={"player_id": "player"},
    )
    if risk == "critical":
        item["parameters"]["properties"]["confirmed"] = {"type": "boolean", "enum": [True]}
        item["parameters"]["required"].append("confirmed")
    return item


class WorkbenchTests(unittest.IsolatedAsyncioTestCase):
    async def fixture(self, item):
        panel = Panel()
        panel.items = [item]
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
        engine = Engine(gateway, State(), directory=directory, actions=actions)
        return panel, engine

    async def test_direct_resurrection_prompts_target_then_submits_once_without_model(self):
        panel, engine = await self.fixture(recipe())
        manager = replace(actor(role="game-manage"), workbench_mode="standard")
        result = await engine.handle(manager, "复活玩家", woken=True)
        self.assertEqual(result.outcome, "needs_selection")
        self.assertIn("玩家昵称或 KU ID", result.text)
        self.assertNotIn("player_id", result.text)
        self.assertNotIn("string", result.text)
        self.assertEqual(panel.executions, 0)
        result = await engine.handle(manager, "KU_TEST123", woken=True)
        self.assertEqual(result.outcome, "matched")
        self.assertEqual(panel.executions, 1)
        args = next(args for name, args in panel.calls if name == "api_runBotCapability")
        self.assertEqual(args["body"]["parameters"], {"player_id": "KU_TEST123"})
        await engine.execute(manager, parse("复活 KU_TEST123", {}))
        self.assertEqual(panel.executions, 1)

    def test_negative_conditional_and_compound_do_not_dispatch(self):
        for text in ("不要复活玩家", "如果复活玩家", "复活 玩家然后重启", "AI:复活玩家"):
            self.assertIsNone(parse(text, {}))
        self.assertEqual(parse("1服复活 玩家", {"1服": "r1"}).room, "r1")

    async def test_critical_model_boolean_cannot_replace_human_confirmation(self):
        item = recipe("workbench.clear_player_inventory", "critical")
        panel, engine = await self.fixture(item)
        manager = replace(actor(role="game-manage"), workbench_mode="all")
        intent = Intent(
            "capability",
            capability=item["id"],
            parameters={"player_id": "KU_TEST123", "confirmed": True},
        )
        result = await engine.execute(manager, intent, Target("r1"))
        self.assertEqual(result.outcome, "needs_selection")
        self.assertIn("尚未执行", result.text)
        self.assertEqual(panel.executions, 0)
        await engine.handle(manager, "true", woken=True)
        self.assertEqual(panel.executions, 0)
        await engine.handle(manager, "确认", woken=True)
        self.assertEqual(panel.executions, 1)

    async def test_reader_off_disabled_and_native_permissions_are_enforced(self):
        item = recipe()
        panel, engine = await self.fixture(item)
        for user in (
            actor(),
            replace(actor(role="game-manage"), workbench_mode="off"),
            replace(
                actor(role="game-manage"),
                workbench_mode="standard",
                disabled_capabilities=(item["id"],),
            ),
        ):
            self.assertFalse(permitted(user, item))
            await engine.execute(user, parse("复活 KU_TEST123", {}))
        self.assertEqual(panel.executions, 0)
        self.assertEqual(
            Configuration({"configuration_version": 3}).effective("")["workbenchMode"], "standard"
        )

    def test_management_discovery_has_search_and_pagination(self):
        manager = replace(actor(role="game-manage"), workbench_mode="standard")
        items = [recipe(f"workbench.recipe{i}") for i in range(21)]
        page = json.loads(capability_page(items, manager, '{"offset":8,"limit":8}', "write"))
        self.assertEqual(page["total"], 21)
        self.assertEqual(page["nextOffset"], 16)
        found = json.loads(capability_page(items, manager, '{"search":"recipe20"}', "write"))
        self.assertEqual(found["items"][0]["id"], "workbench.recipe20")
