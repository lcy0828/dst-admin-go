"""A pending prompt never swallows a new request; receipts name their target."""

import unittest

from astrbot_plugin_dst_companion.engine import target_line
from astrbot_plugin_dst_companion.presentation import business
from astrbot_plugin_dst_companion.routing import Intent, interrupts_form
from astrbot_plugin_dst_companion.state import Target

ITEMS = [
    {"id": "room.performance", "title": "性能报表", "aliases": ["性能", "CPU", "内存"]},
    {"id": "workbench.skip_days", "title": "跳过天数", "aliases": ["跳过一天", "跳天"]},
    {"id": "player.give_item", "title": "发放玩家物品", "aliases": ["发物品", "肉丸"]},
]
PLAYER_FORM = {"type": "parameter", "field": "playerId", "messageId": "prompt-1",
               "item": {"id": "player.activity", "parameterHints": {"playerId": "player"}},
               "intent": Intent("capability", capability="player.activity")}
ITEM_FORM = {"type": "parameter", "field": "item", "messageId": "prompt-2",
             "item": {"id": "player.give_item", "parameterHints": {"playerId": "player"}}}


class InterruptionTests(unittest.TestCase):
    def test_new_requests_interrupt_a_pending_nickname(self):
        for text in ("查看地图", "看看地图", "跳过一天", "最近一天CPU怎么样", "mori在哪？"):
            with self.subTest(text=text):
                self.assertTrue(interrupts_form(text, PLAYER_FORM, "", ITEMS))

    def test_real_answers_fill_the_slot(self):
        for text in ("mori", "示例玩家乙", "KU_SYNTH001", "玩家一号"):
            with self.subTest(text=text):
                self.assertFalse(interrupts_form(text, PLAYER_FORM, "", ITEMS))

    def test_quoting_the_prompt_always_answers_it(self):
        self.assertFalse(interrupts_form("跳过一天", PLAYER_FORM, "prompt-1", ITEMS))

    def test_value_slots_accept_capability_words_and_confirmations_are_untouched(self):
        self.assertFalse(interrupts_form("肉丸", ITEM_FORM, "", ITEMS))
        self.assertFalse(interrupts_form("查看地图", {"type": "confirmation"}, "", ITEMS))


class ReceiptTests(unittest.TestCase):
    ITEM = {"id": "player.resurrect", "parameterHints": {"player_id": "player"}}

    def test_receipt_names_target_and_source(self):
        self.assertEqual(target_line(self.ITEM, Target("r1", "KU_1", "mori", source="explicit")), "对象：mori")
        self.assertEqual(target_line(self.ITEM, Target("r1", "KU_1", "mori", source="focus")),
                         "对象：mori（沿用你刚才操作的玩家）")
        self.assertEqual(target_line(self.ITEM, Target("r1", "KU_1", "", source="quote")),
                         "对象：KU_1（引用的消息）")
        self.assertEqual(target_line({"id": "room.restart", "parameterHints": {}}, Target("r1", "KU_1", "mori")), "")

    def test_source_does_not_change_target_identity(self):
        self.assertEqual(Target("r1", "KU_1", "mori", source="quote"), Target("r1", "KU_1", "mori"))

    def test_full_item_grant_is_one_plain_line(self):
        body, _ = business({"resultKind": "item-grant", "status": "succeeded", "complete": True,
                            "data": {"name": "齿轮", "requested": 5, "granted": 5,
                                     "message": "已生成并发放请求数量的物品；堆叠合并后的去向以回执审计为准"}})
        self.assertIn("已发放 齿轮 ×5", body)
        self.assertNotIn("审计", body)
        self.assertNotIn("请求", body)


if __name__ == "__main__":
    unittest.main()
