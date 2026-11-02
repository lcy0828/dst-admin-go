"""Deterministic boundaries discovered during the dialogue acceptance runs."""

import unittest
import scenario_lab as lab
from astrbot_plugin_dst_companion.routing import parse


class DialogueRegressions(unittest.IsolatedAsyncioTestCase):
    async def scenario(self, turns, **changes):
        case = {"id": "regression", "title": "synthetic", "turns": turns, **changes}
        return await lab.Journey().run_scenario(case, lab.Bridge(""))

    async def test_invalid_quantity_does_not_reserve_uncertain_write(self):
        for count in ("0", "-3", "41"):
            result = await self.scenario([f"给 阿青 {count} 个肉丸"])
            self.assertFalse(result["ledger"])
            self.assertFalse(any(c["write"] for c in result["apiCalls"]))
            self.assertIn("参数", result["turns"][0]["output"][-1])

    async def test_private_disabled_cannot_be_bypassed_with_prefix_and_at(self):
        result = await self.scenario([{"text": "/dst 谁在线", "private": True}], config={"allow_private": False})
        self.assertNotIn("api_listRoomPlayers", [c["api"] for c in result["apiCalls"]])

    async def test_natural_name_answer_continues_read_form(self):
        result = await self.scenario(["玩家信息", "我说的是阿青"])
        calls = [c for c in result["apiCalls"] if c["api"] == "api_resolveRoomPlayer"]
        self.assertEqual(calls[-1]["arguments"]["query"]["query"], "阿青")

    def test_comparison_is_not_one_player_name(self):
        self.assertIsNone(parse("比较一下阿青和小满现在谁在线、在哪个世界", {}))

    def test_ordinary_shortcuts_and_literal_names(self):
        for text, kind in [("踢人 阿青", "kick"), ("保存1服", "capability"), ("重启9服", "restart"), ("查玩家 [CQ:at,qq=all]", "profile")]:
            with self.subTest(text=text):
                intent = parse(text, {"1服": "r1"})
                self.assertEqual(intent.kind, kind)
