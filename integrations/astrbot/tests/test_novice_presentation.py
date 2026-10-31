"""Readable output must retain unknown results and forward capability support."""

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from astrbot_plugin_dst_companion.display import chat_text
from astrbot_plugin_dst_companion.presentation import business


class NovicePresentationTests(unittest.TestCase):
    def test_qq_plain_text_preserves_evidence_identifiers_and_links(self):
        original = "## 复活结果\n已提交 **复活指令**，请 __直接确认__。\n`KU_SAMPLE` · voidcloth_kit\n*仍未确认*，数量 2 * 3。\n[查看帮助](https://example.invalid/help)\n```text\n不确定，禁止重发\n```"
        shown = chat_text(original)
        self.assertNotIn("**", shown)
        self.assertNotIn("`", shown)
        self.assertNotIn("##", shown)
        for evidence in (
            "直接确认",
            "仍未确认",
            "KU_SAMPLE",
            "voidcloth_kit",
            "2 * 3",
            "https://example.invalid/help",
            "不确定，禁止重发",
        ):
            self.assertIn(evidence, shown)
        self.assertEqual(chat_text(shown), shown)
        self.assertEqual(chat_text("```结果不确定，请勿重发```"), "结果不确定，请勿重发")
        self.assertEqual(chat_text("** 请确认 **"), "请确认")

    def test_workbench_receipt_does_not_claim_resurrection_or_dump_runtime_protocol(self):
        result = {
            "capabilityId": "player.resurrect",
            "resultKind": "workbench-run",
            "status": "succeeded",
            "complete": True,
            "data": {
                "output": {"text": "[DST-ADMIN-RESURRECT] REQUESTED; verify current player state"}
            },
        }
        body, footer = business(result)
        self.assertIn("复活指令已执行", body)
        self.assertIn("实际效果尚未确认", body)
        self.assertIn("效果待确认", footer)
        self.assertNotIn("DST-ADMIN", body)
        self.assertNotIn("REQUESTED", body)
        self.assertIn("REQUESTED", business(result, diagnostics=True)[0])
        for status in ("pending", "running", "uncertain", "failed", "rejected"):
            body, footer = business({**result, "status": status, "complete": False})
            self.assertNotIn("已执行。", body)
            self.assertNotEqual(footer, "已完成")
        self.assertIn(
            "请勿重复提交", business({**result, "status": "uncertain", "complete": False})[0]
        )

    def test_confirmed_zero_and_unconfirmed_count_are_distinct(self):
        base = {"resultKind": "query-run", "status": "succeeded", "complete": True}
        body, footer = business({**base, "data": {"data": {"prefab": "spiderden", "count": 0}}})
        self.assertIn("0 个", body)
        self.assertNotIn("不完整", footer)
        body, footer = business({**base, "status": "uncertain", "complete": False, "data": None})
        self.assertNotIn("0", body)
        self.assertIn("请勿重复提交", footer)

    def test_new_scalar_results_survive_without_dumping_internal_metadata(self):
        result = {
            "resultKind": "facts",
            "status": "succeeded",
            "complete": True,
            "requestId": "synthetic-request",
            "data": {"answer": 42, "worldId": "encoded-world", "internal": {"private": "value"}},
        }
        body, footer = business(result)
        self.assertIn("42", body)
        self.assertNotIn("encoded-world", body)
        self.assertNotIn("synthetic-request", footer)
        self.assertIn("synthetic-request", business(result, diagnostics=True)[1])

    def test_partial_inventory_warns_and_preserves_save_time(self):
        result = {
            "capabilityId": "player.inventory",
            "status": "partial",
            "complete": False,
            "data": {"source": "save", "status": "partial", "savedAt": "2026-11-01T01:00:00Z"},
        }
        body, footer = business(result)
        self.assertIn("存档时间", body)
        self.assertIn("部分内容未能读取", body)
        self.assertIn("部分结果", footer)

    def test_world_information_does_not_drop_false_or_zero_values(self):
        result = {
            "resultKind": "query-run",
            "status": "succeeded",
            "complete": True,
            "data": {"data": {"cycles": 0, "israining": False, "season": "autumn"}},
        }
        body, _ = business(result)
        self.assertIn("已度过天数：0", body)
        self.assertIn("下雨：否", body)
        self.assertIn("季节：秋", body)
