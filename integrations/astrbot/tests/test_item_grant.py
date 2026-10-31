"""Fixed item delivery: deterministic routing, identity selection and no replay."""

import copy
import sys
import time
import unittest
from dataclasses import replace
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from test_companion import actor
from test_v41 import Panel, capability

from astrbot_plugin_dst_companion.capabilities import Directory
from astrbot_plugin_dst_companion.engine import Engine
from astrbot_plugin_dst_companion.gateway import Gateway, QueryError
from astrbot_plugin_dst_companion.jobs import Actions
from astrbot_plugin_dst_companion.policy import Policy
from astrbot_plugin_dst_companion.presentation import business
from astrbot_plugin_dst_companion.routing import parse
from astrbot_plugin_dst_companion.state import State


class ItemGrantTests(unittest.IsolatedAsyncioTestCase):
    def test_deterministic_whole_message_grammar(self):
        for phrase in (
            "给 测试玩家 发 3 个肉丸",
            "1服给测试玩家发放3个肉丸",
            "请给测试玩家发3个肉丸",
            "给 测试玩家 三个肉丸",
            "给测试玩家 三个肉丸",
            "发给 测试玩家 三个肉丸",
            "送给 测试玩家 3个肉丸",
            "1服给 测试玩家 三个肉丸",
        ):
            intent = parse(phrase, {"1服": "r1"})
            self.assertEqual(intent.capability, "player.give_item")
            self.assertEqual(
                intent.parameters, {"playerId": "测试玩家", "item": "肉丸", "count": 3}
            )
        for phrase in (
            "不要给测试玩家发3个肉丸",
            "如果给测试玩家发3个肉丸",
            "给测试玩家发3个肉丸然后重启",
            "AI: 给测试玩家发3个肉丸",
            "给 阿发 三个肉丸",  # Two possible identities; do not guess.
        ):
            self.assertIsNone(parse(phrase, {}))

    def test_chinese_counts_are_exact_and_keep_server_limits(self):
        for word, count in (
            ("一", 1),
            ("两", 2),
            ("十", 10),
            ("十二", 12),
            ("二十", 20),
            ("三十九", 39),
            ("四十", 40),
            ("四十一", 41),
        ):
            intent = parse("给 测试玩家 " + word + "个肉丸", {})
            self.assertEqual(intent.parameters["count"], count)
        # An unseparated nickname/quantity is not silently split.
        self.assertIsNone(parse("给玩家三三个肉丸", {}))

    async def test_grant_is_explicitly_authorized_and_lost_reply_never_replayed(self):
        panel = Panel()
        item = capability(
            "player.give_item",
            "write",
            {
                "type": "object",
                "properties": {
                    "playerId": {"type": "string", "minLength": 1, "maxLength": 128},
                    "item": {"type": "string", "minLength": 1, "maxLength": 100},
                    "count": {"type": "integer", "minimum": 1, "maximum": 40},
                },
                "required": ["playerId", "item", "count"],
                "additionalProperties": False,
            },
        )
        item["nativeTools"] = ["api_giveRoomPlayerItem", "api_getRoomPlayerItemGrant"]
        item["parameterHints"] = {"playerId": "player"}
        item["resultKind"] = "item-grant"
        panel.items = [item]
        native_calls = []

        async def native(a, name):
            native_calls.append(name)
            return True

        policy = Policy(native)
        gateway = Gateway(panel.transport, policy.authorize)
        directory = Directory(gateway)
        policy.directory = directory
        journal = {}

        async def load():
            return copy.deepcopy(journal)

        async def save(records):
            journal.clear()
            journal.update(copy.deepcopy(records))

        actions = Actions(gateway, load, save, directory=directory)
        engine = Engine(gateway, State(), actions=actions, directory=directory)
        reader = actor()
        intent = parse("给 KU_TEST123 三个肉丸", {})
        result = await engine.execute(reader, intent)
        self.assertNotEqual(result.outcome, "matched")
        self.assertEqual(panel.executions, 0)
        manager = replace(reader, role="game-manage", write_allowlist=("player.give_item",))
        panel.lose_reply = True
        first = await engine.execute(manager, intent)
        self.assertEqual(panel.executions, 1)
        self.assertIn("api_giveRoomPlayerItem", native_calls)
        self.assertIn("api_getRoomPlayerItemGrant", native_calls)
        second = await engine.execute(manager, intent)
        self.assertEqual(panel.executions, 1)
        self.assertTrue(first.text)
        self.assertTrue(second.text)
        with self.assertRaises(QueryError):
            await directory.execute(
                manager,
                item,
                "r1",
                {"playerId": "KU_TEST123", "item": "肉丸", "count": 41},
                time.monotonic() + 8,
            )
        self.assertEqual(panel.executions, 1)
        await actions.close()
        await gateway.close()

    def test_receipt_display_uses_this_grant_not_inventory_total(self):
        body, footer = business(
            {
                "resultKind": "item-grant",
                "status": "uncertain",
                "complete": False,
                "data": {"name": "肉丸", "requested": 3, "message": "结果尚未确认"},
            }
        )
        self.assertIn("请求 3 个", body)
        self.assertNotIn("确认发放", body)
        self.assertIn("请回读原请求", footer)
