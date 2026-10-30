"""Real message recipes and panel response shapes, without personal data."""

import unittest

from test_companion import FakeGateway, actor

from astrbot_plugin_dst_companion.configuration import DEFAULTS, Configuration
from astrbot_plugin_dst_companion.engine import Engine
from astrbot_plugin_dst_companion.formatters import inventory
from astrbot_plugin_dst_companion.routing import parse
from astrbot_plugin_dst_companion.state import State, Target


class ChatExperienceTests(unittest.IsolatedAsyncioTestCase):
    async def test_overview_lists_both_rooms_even_after_a_previous_single_room_query(self):
        gateway, state = FakeGateway(), State()
        engine = Engine(gateway, state)
        a = actor(rooms=("r1", "r2"))
        state.remember(a.key, state.begin(a.key), Target("r1"), "historical")
        result = await engine.handle(a, "查房", woken=True)
        self.assertIn("测试房间", result.text)
        self.assertIn("第二房间", result.text)
        self.assertEqual(result.text.count("在线 1 人"), 2)
        self.assertNotIn("请选择房间", result.text)

    async def test_chinese_room_alias_uses_explicit_mapping_and_supports_inventory_alias(self):
        for text, kind in (
            ("一服信息", "status"),
            ("一服详情", "status"),
            ("一服查装备 示例玩家", "inventory"),
        ):
            intent = parse(text, {"1服": "r1"})
            self.assertEqual((intent.kind, intent.room), (kind, "r1"))
        intent = parse("一服信息", {})
        self.assertEqual((intent.room, intent.room_name), ("", "1服"))

    async def test_inventory_preserves_nested_containers_names_zero_metrics_and_unknown_percentages(
        self,
    ):
        text = inventory(
            {
                "source": "save",
                "status": "partial",
                "worldName": "Caves",
                "equipment": [
                    {
                        "prefab": "krampus_sack",
                        "name": "坎普斯背包",
                        "slot": "equipment/body",
                        "count": 1,
                    }
                ],
                "inventory": [
                    {
                        "prefab": "bundle",
                        "name": "示例容器",
                        "count": 1,
                        "contents": [{"prefab": "twigs", "name": "树枝", "count": 7}],
                        "metrics": {
                            "uses": {"value": 3, "availability": "value_only"},
                            "fuel": {"value": 0, "ratio": 0, "availability": "available"},
                        },
                    }
                ],
            }
        )
        for expected in (
            "装备",
            "身体：坎普斯背包",
            "物品栏",
            "└ 树枝 ×7",
            "次数 3",
            "燃料 0%",
            "存档快照",
        ):
            self.assertIn(expected, text)
        self.assertNotIn("次数 3%", text)
        self.assertNotIn("实时读取", text)

    async def test_new_default_is_mixed_and_explicit_shortcut_configuration_is_preserved(self):
        self.assertEqual(DEFAULTS["mode"], "mixed")
        self.assertEqual(
            Configuration({"configuration_version": 2, "defaults": {"mode": "shortcut"}}).effective(
                "group"
            )["mode"],
            "shortcut",
        )


if __name__ == "__main__":
    unittest.main()
