"""Fixed creature spawning uses the same permissions for shortcuts and AI calls."""

import copy
import json
import sys
import time
import unittest
from dataclasses import replace
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from test_companion import actor
from test_v41 import Panel, capability

from astrbot_plugin_dst_companion.capabilities import Directory
from astrbot_plugin_dst_companion.engine import Engine
from astrbot_plugin_dst_companion.gateway import Gateway, QueryError
from astrbot_plugin_dst_companion.jobs import Actions
from astrbot_plugin_dst_companion.model_tools import ModelTools
from astrbot_plugin_dst_companion.policy import Policy
from astrbot_plugin_dst_companion.presentation import business
from astrbot_plugin_dst_companion.routing import parse
from astrbot_plugin_dst_companion.state import State


def spawn_capability():
    item = capability(
        "player.spawn_entity",
        "write",
        {
            "type": "object",
            "properties": {
                "playerId": {"type": "string", "minLength": 1, "maxLength": 128},
                "entity": {"type": "string", "minLength": 1, "maxLength": 100},
                "count": {"type": "integer", "minimum": 1, "maximum": 5},
            },
            "required": ["playerId", "entity", "count"],
            "additionalProperties": False,
        },
    )
    item["nativeTools"] = ["api_spawnRoomPlayerEntity", "api_getRoomPlayerEntitySpawn"]
    item["parameterHints"] = {"playerId": "player"}
    item["resultKind"] = "entity-spawn"
    return item


class EntitySpawnTests(unittest.IsolatedAsyncioTestCase):
    def test_whole_message_spawning_never_matches_discussion_or_compound_requests(self):
        for phrase in (
            "在 示例玩家 附近生成三只座狼",
            "示例玩家旁边刷1只座狼",
            "1服在示例玩家身边刷出两头猪人",
        ):
            intent = parse(phrase, {"1服": "r1"})
            self.assertEqual(intent.capability, "player.spawn_entity")
            self.assertEqual(intent.parameters["playerId"], "示例玩家")
        for phrase in (
            "不要在示例玩家附近生成3只座狼",
            "如果在示例玩家附近生成3只座狼",
            "在示例玩家附近生成3只座狼然后重启",
            "AI: 在示例玩家附近生成3只座狼",
            "在示例玩家附近生成3只座狼;保存世界",
        ):
            self.assertIsNone(parse(phrase, {}))

    async def asyncSetUp(self):
        self.panel = Panel()
        self.item = spawn_capability()
        self.panel.items = [self.item]
        self.native = AsyncMock(return_value=True)
        self.policy = Policy(self.native)
        self.gateway = Gateway(self.panel.transport, self.policy.authorize)
        self.directory = Directory(self.gateway)
        self.policy.directory = self.directory
        self.journal = {}

        async def load():
            return copy.deepcopy(self.journal)

        async def save(records):
            self.journal.clear()
            self.journal.update(copy.deepcopy(records))

        self.actions = Actions(self.gateway, load, save, directory=self.directory)
        self.engine = Engine(self.gateway, State(), actions=self.actions, directory=self.directory)
        self.reader = actor()
        self.manager = replace(
            self.reader, role="game-manage", write_allowlist=("player.spawn_entity",)
        )

    async def asyncTearDown(self):
        await self.actions.close()
        await self.gateway.close()

    async def test_reader_and_unlisted_manager_cannot_spawn_and_uncertain_call_never_replays(self):
        intent = parse("在 KU_TEST123 附近生成三只座狼", {})
        for caller in (self.reader, replace(self.manager, write_allowlist=())):
            result = await self.engine.execute(caller, intent)
            self.assertNotEqual(result.outcome, "matched")
            self.assertEqual(self.panel.executions, 0)
        self.panel.lose_reply = True
        first = await self.engine.execute(self.manager, intent)
        self.assertEqual(self.panel.executions, 1)
        second = await self.engine.execute(self.manager, intent)
        self.assertTrue(first.text)
        self.assertTrue(second.text)
        self.assertEqual(self.panel.executions, 1)
        tools = [call.args[1] for call in self.native.await_args_list]
        self.assertIn("api_spawnRoomPlayerEntity", tools)
        self.assertIn("api_getRoomPlayerEntitySpawn", tools)
        with self.assertRaises(QueryError):
            await self.directory.execute(
                self.manager,
                self.item,
                "r1",
                {"playerId": "KU_TEST123", "entity": "座狼", "count": 6},
                time.monotonic() + 8,
            )
        self.assertEqual(self.panel.executions, 1)

    async def test_model_wrapper_discovers_and_calls_the_same_bounded_capability(self):
        plugin = NS(
            prepare=AsyncMock(),
            actor=lambda _: self.manager,
            directory=self.directory,
            engine=self.engine,
        )
        model_tools = ModelTools(plugin)
        available = json.loads(await model_tools.dst_game_action(None, "r1", "capabilities"))[
            "items"
        ]
        self.assertEqual([item["id"] for item in available], ["player.spawn_entity"])
        self.panel.lose_reply = True
        result = await model_tools.dst_game_action(
            None,
            "r1",
            "cap:player.spawn_entity",
            parameters=json.dumps({"playerId": "KU_TEST123", "entity": "座狼", "count": 1}),
        )
        self.assertTrue(result)
        self.assertEqual(self.panel.executions, 1)
        plugin.actor = lambda _: self.reader
        denied = await model_tools.dst_game_action(None, "r1", "cap:player.spawn_entity")
        self.assertIn("没有管理权限", denied)
        self.assertEqual(self.panel.executions, 1)

    def test_rejected_spawn_displays_zero_and_unknown_receipt_is_not_success(self):
        body, footer = business(
            {
                "resultKind": "entity-spawn",
                "status": "failed",
                "complete": True,
                "data": {"name": "座狼", "requested": 3, "spawned": 0, "message": "未生成"},
            }
        )
        self.assertIn("生成：0 只", body)
        self.assertIn("执行失败", footer)
        body, footer = business(
            {
                "resultKind": "entity-spawn",
                "status": "uncertain",
                "complete": False,
                "data": {"name": "座狼", "requested": 3},
            }
        )
        self.assertNotIn("确认生成", body)
        self.assertIn("回读原请求", footer)
