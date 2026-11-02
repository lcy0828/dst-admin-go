"""Regressions for room selection and the scope of conversational read queries."""

import unittest
from dataclasses import replace
from types import SimpleNamespace
from unittest.mock import AsyncMock

from test_companion import FakeGateway, P, actor
from test_message_display import StatusGateway

from astrbot_plugin_dst_companion import formatters as fmt
from astrbot_plugin_dst_companion.engine import Engine
from astrbot_plugin_dst_companion.routing import Intent
from astrbot_plugin_dst_companion.state import State, Target


class RoomSelectionTests(unittest.IsolatedAsyncioTestCase):
    async def test_unknown_room_name_offers_selection_and_continues_the_original_read(self):
        for question, expected_api in (
            ("1服信息", "api_listWorldStates"),
            ("一服状态", "api_listWorldStates"),
            ("查询1服房间状态", "api_listWorldStates"),
            ("查看1服状态", "api_listWorldStates"),
            ("1服现在状态怎么样", "api_listWorldStates"),
            ("查一下1服在线玩家", "api_listRoomPlayers"),
            ("1服有几个人", "api_listRoomPlayers"),
            ("1服现在多少人在线", "api_listRoomPlayers"),
        ):
            with self.subTest(question=question):
                gateway, state = StatusGateway(), State()
                engine = Engine(gateway, state, default_room="r1")
                result = await engine.handle(actor(), question, woken=True)
                self.assertEqual(result.outcome, "needs_selection")
                self.assertIn("请选择要查询的房间", result.text)
                self.assertEqual([name for name, _ in gateway.calls], ["api_listRooms"])
                self.assertEqual(state.choices[actor().key]["intent"].room_name, "")
                result = await engine.handle(actor(), "1")
                self.assertEqual(result.outcome, "matched")
                self.assertEqual(result.target.room, "r1")
                self.assertIn(expected_api, [name for name, _ in gateway.calls])
                self.assertNotIn(actor().key, state.choices)

    async def test_player_query_survives_room_selection(self):
        gateway, state = FakeGateway(), State()
        engine = Engine(gateway, state)
        result = await engine.handle(actor(rooms=("r1", "r2")), "1服查背包 示例玩家", woken=True)
        self.assertEqual(result.outcome, "needs_selection")
        result = await engine.handle(actor(rooms=("r1", "r2")), "2")
        self.assertEqual(result.outcome, "matched")
        reads = [args for name, args in gateway.calls if name == "api_resolveRoomPlayer"]
        self.assertEqual(reads, [{"path": {"roomId": "r2"}, "query": {"query": "示例玩家"}}])

    async def test_selection_rechecks_scope_and_unknown_write_target_is_not_redirected(self):
        gateway, state = FakeGateway(), State()
        engine = Engine(gateway, state)
        await engine.handle(actor(rooms=("r1", "r2")), "1服信息", woken=True)
        result = await engine.handle(actor(rooms=("r2",)), "1")
        self.assertIn("未开放该房间", result.text)
        self.assertTrue(all(name == "api_listRooms" for name, _ in gateway.calls))

        actions = SimpleNamespace(submit=AsyncMock())
        engine = Engine(gateway, State(), actions=actions)
        manager = replace(actor(role="game-manage"), write_allowlist=("room.restart",))
        result = await engine.execute(manager, Intent("restart", room_name="1服"), Target("r1"))
        self.assertIn("没有唯一匹配", result.text)
        self.assertFalse(result.choice)
        actions.submit.assert_not_awaited()

    async def test_menu_does_not_advertise_an_unconfigured_room_alias(self):
        result = await Engine(FakeGateway(), State()).handle(actor(), "菜单", woken=True)
        self.assertIn("有哪些服、谁在线、查背包 昵称", result.text)
        self.assertNotIn("1服谁在线", result.text)


class NetworkGateway(FakeGateway):
    def __init__(self):
        super().__init__()
        self.candidates = [
            {
                **P,
                "name": "离线示例",
                "online": False,
                "presenceStatus": "offline",
                "fields": {"netScore": {"status": "stale"}},
            }
        ]

    async def call(self, a, name, args, deadline, **kwargs):
        data = await super().call(a, name, args, deadline, **kwargs)
        if name == "api_listRoomPlayers":
            return {
                "online": 1,
                "total": 1,
                "items": [{**P, "id": "KU_online", "name": "在线示例"}],
            }
        return data


class NetworkScopeTests(unittest.IsolatedAsyncioTestCase):
    async def test_general_network_queries_ignore_previous_player_and_match_model_execution(self):
        for question in ("网络档位", "网络质量", "谁卡", "网络延迟"):
            with self.subTest(question=question):
                gateway, state = NetworkGateway(), State()
                engine = Engine(gateway, state)
                await engine.handle(actor(), "查玩家 离线示例", woken=True)
                gateway.calls.clear()
                result = await engine.handle(actor(), question, woken=True)
                self.assertIn("在线示例", result.text)
                self.assertNotIn("离线示例", result.text)
                self.assertEqual(result.target, Target("r1"))
                self.assertEqual(
                    [name for name, _ in gateway.calls], ["api_listRooms", "api_listRoomPlayers"]
                )
                self.assertEqual(gateway.calls[-1][1]["query"]["status"], "online")
                model = await engine.execute(actor(), Intent("network"), Target("r1", P["id"]))
                self.assertEqual(result.text, model.text)

    async def test_explicit_player_pronoun_and_verified_quote_preserve_player_scope(self):
        for question, quote in (("查网络 离线示例", ""), ("查他网络", ""), ("网络质量", "sent")):
            with self.subTest(question=question, quote=quote):
                gateway, state = NetworkGateway(), State()
                engine = Engine(gateway, state)
                await engine.handle(actor(), "查玩家 离线示例", woken=True)
                state.associate(actor().scope, "sent", Target("r1", P["id"], "离线示例"))
                gateway.calls.clear()
                result = await engine.handle(actor(), question, woken=True, quote=quote)
                self.assertIn("离线示例", result.text)
                self.assertIn("历史/未同步", result.text)
                self.assertNotIn("在线示例", result.text)
                self.assertEqual(
                    [name for name, _ in gateway.calls], ["api_listRooms", "api_resolveRoomPlayer"]
                )

    async def test_unknown_network_quote_asks_for_player_instead_of_using_memory_or_room(self):
        gateway, state = NetworkGateway(), State()
        engine = Engine(gateway, state)
        await engine.handle(actor(), "查玩家 离线示例", woken=True)
        gateway.calls.clear()
        result = await engine.handle(actor(), "网络质量", woken=True, quote="unknown")
        self.assertEqual(result.outcome, "needs_selection")
        self.assertIn("要查看哪位玩家", result.text)
        self.assertEqual([name for name, _ in gateway.calls], ["api_listRooms"])
        result = await engine.handle(actor(), "离线示例")
        self.assertEqual(result.outcome, "matched")
        self.assertIn("离线示例", result.text)


class EmptyInventoryTests(unittest.TestCase):
    def test_empty_or_unsupported_sections_do_not_render_blank_headings_or_claim_no_items(self):
        for contents in ([], [{}], [{"unknown": True}]):
            text = fmt.inventory(
                {
                    "source": "save",
                    "status": "partial",
                    "savedAt": "2026-01-01T00:00:00Z",
                    "equipment": contents,
                    "inventory": contents,
                    "activeItem": {},
                }
            )
            for label in ("【装备】", "【物品栏】", "【鼠标手持】"):
                self.assertNotIn(label, text)
            self.assertIn("未返回可展示物品", text)
            self.assertIn("缺失处不代表没有物品", text)
            self.assertIn("存档时间：", text)

    def test_nonempty_section_keeps_heading_and_localized_items(self):
        text = fmt.inventory(
            {
                "source": "save",
                "status": "complete",
                "equipment": [],
                "inventory": [{"prefab": "twigs", "stackSize": 3}],
            }
        )
        self.assertNotIn("【装备】", text)
        self.assertIn("📦【物品栏】\n· 树枝 ×3", text)
        self.assertNotIn("未返回可展示物品", text)
