"""Template boundaries, persistence and real AstrBot commands; no external sends."""

import asyncio
import copy
import importlib.util
import json
import sys
import unittest
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from test_companion import FakeGateway, actor

from astrbot_plugin_dst_companion.broadcast import BroadcastEngine
from astrbot_plugin_dst_companion.configuration import ConfigEditor, Configuration, fingerprint
from astrbot_plugin_dst_companion.engine import Engine
from astrbot_plugin_dst_companion.gateway import QueryError
from astrbot_plugin_dst_companion.message_templates import (
    DEFINITIONS,
    MessageTemplates,
    render_text,
    revision,
    rows_from,
    sample_values,
    stored_snapshot,
    validate_templates,
)
from astrbot_plugin_dst_companion.onboarding import Enrollment
from astrbot_plugin_dst_companion.state import State

AVAILABLE = importlib.util.find_spec("astrbot") is not None


class TemplateTests(unittest.IsolatedAsyncioTestCase):
    def templates(self):
        self.config = {"message_templates": rows_from({"status": "全局 {body}"})}
        self.enrollment = Enrollment(AsyncMock())
        self.enrollment.records = {
            "a": {"enabled": True, "messageTemplates": rows_from({"status": "本群 {body}"})},
            "b": {"enabled": True},
        }
        return MessageTemplates(self.config, self.enrollment)

    def test_precedence_group_isolation_and_global_preview(self):
        t = self.templates()
        self.assertEqual(t.render("a", "status", {"body": "证据"}), "本群 证据")
        self.assertEqual(t.render("b", "status", {"body": "证据"}), "全局 证据")
        self.assertEqual(t.selected("a", "status", global_scope=True)[1], "全局覆盖")
        self.assertEqual(t.selected("a", "action")[1], "内置默认")
        self.enrollment.records["a"]["messageTemplates"] = []
        self.assertEqual(t.selected("a", "status")[1], "全局覆盖")
        self.config["message_templates"] = []
        t.refresh()
        self.assertEqual(t.selected("a", "status")[1], "内置默认")

    def test_all_defaults_and_framework_form_templates_are_valid(self):
        schema = json.loads(
            (
                Path(__file__).resolve().parents[1]
                / "astrbot_plugin_dst_companion/_conf_schema.json"
            ).read_text()
        )
        rows = []
        for kind, definition in DEFINITIONS.items():
            entry = schema["message_templates"]["templates"][kind]
            self.assertEqual(entry["items"]["body"]["type"], "text")
            row = {"__template_key": kind, "type": kind, "body": entry["items"]["body"]["default"]}
            rows.append(row)
            self.assertEqual(row["body"], definition.default)
            self.assertTrue(render_text(kind, definition.default, sample_values(kind)))
        self.assertEqual(len(validate_templates(rows)), len(DEFINITIONS))

    def test_invalid_fields_expression_injection_and_budget_are_rejected(self):
        for source in (
            "{body.__class__}",
            "{body[0]}",
            "{body!r}",
            "{body:20}",
            "{unknown}",
            "{}",
            "{",
            "[CQ:at,qq=all]{body}",
            "{body}" * 33,
            "{body}" * 2,
            "x" * 2001,
            "\ud800{body}",
            "\x01{body}",
            "{title}",
        ):
            with self.subTest(source=repr(source)):
                with self.assertRaises(ValueError):
                    validate_templates(rows_from({"status": source}))
        for rows in (
            [{"type": "evil", "body": "x"}],
            [{"type": [], "body": "x"}],
            [{"type": "status", "body": "{body}", "readOnly": True}],
            [{"__template_key": "unknown", "type": "status", "body": "{body}"}],
            rows_from({"status": "{body}"}) * 2,
            {},
            [None],
        ):
            with self.assertRaises((ValueError, TypeError)):
                validate_templates(rows)

    def test_values_are_text_not_second_pass_templates_and_indent_is_preserved(self):
        t = self.templates()
        result = t.render("a", "status", {"body": "  └ {room_name}\n[CQ:at,qq=all]\n0"})
        self.assertIn("  └ {room_name}", result)
        self.assertNotIn("[CQ:", result)
        self.assertIn("0", result)
        result = render_text("status", "{{字面}} {body}", {"body": "正文"})
        self.assertEqual(result, "{字面} 正文")

    def test_old_invalid_config_falls_back_without_breaking_authority(self):
        t = self.templates()
        self.config["message_templates"] = [{"type": "status", "body": "{body.x}"}]
        t.refresh()
        self.assertIn("无效覆盖", t.selected("b", "status")[1])
        self.assertIn("证据", t.render("b", "status", {"body": "证据"}))
        values = Configuration({"configuration_version": 3, "message_templates": [None]}).effective(
            "b"
        )
        self.assertEqual(values["writeAllowlist"], ())
        self.assertEqual(t.selected("b", "inventory", legacy="旧版 {body}")[1], "旧版查询模板")
        self.assertIn("证据", t.render("b", "inventory", {"body": "证据"}, legacy="{title}"))

    def test_snapshot_is_bounded_cached_and_immutable(self):
        encoded = json.dumps(rows_from({"status": "{body}"}))
        before = stored_snapshot.cache_info().hits
        self.assertIs(stored_snapshot(encoded), stored_snapshot(encoded))
        self.assertGreater(stored_snapshot.cache_info().hits, before)
        with self.assertRaises(TypeError):
            stored_snapshot(encoded)[0]["status"] = "x"
        self.assertEqual(stored_snapshot.cache_info().maxsize, 128)

    def test_output_fits_sender_budget_and_preserves_receipt_tail(self):
        for kind in ("action", "inventory", "chat_batch"):
            required = DEFINITIONS[kind].required
            values = sample_values(kind)
            values[required] = "内容" * 3000 + "\n结果尚未确认；存档时间未确认；不要重复提交。"
            source = "装饰" * 900 + "{" + required + "}"
            result = render_text(kind, source, values)
            self.assertLess(len(result), 3100)
            self.assertIn("结果尚未确认", result)
            self.assertIn("不要重复提交", result)
            self.assertIn("已节选", result)

    async def test_group_persistence_failure_and_conflict_preserve_other_settings(self):
        t = self.templates()
        before = copy.deepcopy(self.enrollment.records)
        self.enrollment.save.side_effect = OSError()
        with self.assertRaises(OSError):
            await self.enrollment.update_templates("a", [], revision(t.rows("a")))
        self.assertEqual(self.enrollment.records, before)
        self.enrollment.save = AsyncMock(return_value=False)
        with self.assertRaises(QueryError):
            await self.enrollment.update_templates("a", [], revision(t.rows("a")))
        self.assertEqual(self.enrollment.records, before)
        self.enrollment.save = AsyncMock()
        with self.assertRaises(QueryError):
            await self.enrollment.update_templates("a", [], "stale")
        self.enrollment.save.assert_not_awaited()
        await self.enrollment.update_templates("a", [], revision(t.rows("a")))
        self.assertEqual(self.enrollment.records["b"], before["b"])
        self.assertTrue(self.enrollment.records["a"]["enabled"])

    def test_template_revisions_do_not_change_authorization_or_lose_room_identity(self):
        original = {"configuration_version": 3}
        changed = {**original, "message_templates": rows_from({"status": "标题 {body}"})}
        self.assertEqual(
            Configuration(original).effective("g"), Configuration(changed).effective("g")
        )
        for suffix in ("1", "2"):
            label = "同名房间" * 18 + " · opaque-" + suffix
            result = render_text("status", "{title}\n{body}", {"title": label, "body": "数据"})
            self.assertIn("opaque-" + suffix, result)

    async def test_template_change_does_not_restart_feeds_or_reseed_cursors(self):
        from astrbot_plugin_dst_companion.notifications import Notifications

        record = {"notices": {"presence": True, "chat": True}}
        catalog = NS(rows=[{"id": "r", "name": "示例服"}], refresh=AsyncMock())
        p = NS(
            config={},
            catalog=catalog,
            connection_identity=lambda: "source",
            connections=NS(broadcast=lambda _: "read"),
            effective=lambda _: {"rooms": ("r",)},
        )
        hub = Notifications(p)
        hub.subscriptions = lambda: {"group": record}
        hub.source, hub.http = "source", object()
        engine = NS(rooms={"r": "示例服"}, options={**record["notices"], "leave_grace_seconds": 10})
        hub.engines = {"group": engine}
        feed = NS(cancel=lambda: self.fail("template edit must not cancel feed"))
        hub.tasks = {"r": feed}
        hub.ready = {("group", "r")}
        hub.stop_feeds = AsyncMock()
        record["messageTemplates"] = rows_from({"chat_batch": "新 {lines}"})
        await hub._reconcile()
        self.assertIs(hub.engines["group"], engine)
        self.assertIs(hub.tasks["r"], feed)
        self.assertEqual(hub.ready, {("group", "r")})
        hub.stop_feeds.assert_not_awaited()

    async def test_global_failed_save_rolls_back_and_does_not_publish_candidate(self):
        t = self.templates()
        before = copy.deepcopy(self.config)
        candidate = {**self.config, "message_templates": rows_from({"status": "新 {body}"})}
        editor = ConfigEditor(self.config, AsyncMock(return_value=False))
        with self.assertRaises(QueryError):
            await editor.apply(candidate, fingerprint(before), ["a"])
        self.assertEqual(self.config, before)
        self.assertTrue(t.render("b", "status", {"body": "证据"}).startswith("全局"))
        editor.save = AsyncMock(return_value=True)
        await editor.apply(candidate, fingerprint(before), ["a"])
        self.assertTrue(t.render("b", "status", {"body": "证据"}).startswith("全局"))
        t.refresh()
        self.assertTrue(t.render("b", "status", {"body": "证据"}).startswith("新"))
        with self.assertRaises(ValueError):
            await editor.apply(
                {**candidate, "message_templates": [None]}, fingerprint(candidate), ["a"]
            )

    async def test_query_template_preserves_snapshot_evidence_and_requires_no_extra_reads(self):
        t = self.templates()
        self.enrollment.records["group"] = {
            "messageTemplates": rows_from({"inventory": "🎒 {player}\n{body}"})
        }
        gateway = FakeGateway()
        engine = Engine(
            gateway,
            State(),
            message_renderer=lambda session, kind, values, **kw: t.render(
                session, kind, values, **kw
            ),
        )
        reply = await engine.handle(actor(), "查背包 玩家 A", woken=True)
        self.assertIn("存档", reply.text)
        self.assertIn("2026-01-01 08:00:00", reply.text)
        self.assertIn("树枝", reply.text)
        self.assertEqual(len(gateway.calls), 3)

    async def test_chat_switch_uses_new_template_only_for_new_queue_entries(self):
        t = self.templates()
        messages = [
            {
                "id": "old",
                "kind": "say",
                "occurredAt": "2026-10-31T04:00:00Z",
                "playerName": "甲",
                "content": "旧",
            }
        ]

        async def read(*args):
            return {"items": list(reversed(messages))}

        engine = BroadcastEngine(
            read,
            None,
            AsyncMock(),
            {"r": "示例服"},
            clock=lambda: 1793419200,
            message_renderer=lambda kind, values: t.render("a", kind, values),
        )
        # Derive clock from ISO to avoid brittle hardcoded epoch.
        from astrbot_plugin_dst_companion.broadcast import timestamp

        engine.clock = lambda: timestamp(messages[-1]["occurredAt"])
        await engine.poll_chat("r")  # Seed: no old messages replayed.
        self.assertEqual(engine.state["outbox"], [])
        messages.append({**messages[0], "id": "one", "content": "第一条"})
        await engine.poll_chat("r")
        first = copy.deepcopy(engine.state["outbox"][0])
        self.enrollment.records["a"]["messageTemplates"] = rows_from(
            {"chat_batch": "新模板 {lines}", "chat_line": "{time} {player_name}：{message}"}
        )
        messages.extend(
            {**messages[0], "id": str(i), "content": "新聊天" + str(i)} for i in range(7)
        )
        await engine.poll_chat("r")
        self.assertEqual(engine.state["outbox"][0], first)
        self.assertEqual([n["related"]["chatCount"] for n in engine.state["outbox"]], [1, 5, 2])
        self.assertTrue(engine.state["outbox"][1]["text"].startswith("新模板"))
        self.assertLess(len(engine.state["outbox"][1]["text"]), 7000)
        self.assertIn("12:00:00", engine.state["outbox"][1]["text"])

    def test_cannot_hide_uncertainty_or_amplify_output_without_fallback(self):
        values = sample_values("action")
        source = "{title}" * 32 + "{body}"
        out = render_text("action", source, values)
        self.assertIn("结果尚未确认", out)
        self.assertIn("不要重复提交", out)
        self.assertIn("来源", out)
        self.assertLess(len(out), 7000)


@unittest.skipUnless(AVAILABLE, "requires installed AstrBot")
class TemplateAdapterTests(unittest.IsolatedAsyncioTestCase):
    if AVAILABLE:
        import test_unified_plugin as unified

        asyncSetUp = unified.UnifiedAdapterTests.asyncSetUp
        asyncTearDown = unified.UnifiedAdapterTests.asyncTearDown
        event = unified.UnifiedAdapterTests.event
        output = unified.UnifiedAdapterTests.output
        enable = unified.UnifiedAdapterTests.enable

    async def test_preview_and_permission_guard_require_no_panel_or_model(self):
        event = self.event("/dst 模板 预览 房间概况")
        await self.bot.on_message(event)
        self.assertIn("模拟预览", self.output(event))
        self.assertEqual(self.calls, [])
        event = self.event("/dst 模板 编辑 聊天播报 {lines}", admin=False)
        await self.bot.on_message(event)
        self.assertIn("仅允许 AstrBot 管理员", self.output(event))
        self.assertEqual(self.bot.template_editor.drafts, {})

    async def test_edit_preview_save_restore_and_group_isolation(self):
        await self.enable()
        self.calls.clear()
        group = "adapter:GroupMessage:789"
        before = copy.deepcopy(self.bot.enrollment.records[group])
        for command, expected in (
            ("编辑 聊天播报 💬 自定义 {room_name}\n{lines}", "草稿"),
            ("预览 聊天播报", "自定义 示例服"),
            ("保存", "已保存"),
        ):
            event = self.event("/dst 模板 " + command)
            await self.bot.on_message(event)
            self.assertIn(expected, self.output(event))
        self.assertEqual(self.bot.message_templates.selected(group, "chat_batch")[1], "本群覆盖")
        self.assertEqual(
            self.bot.message_templates.selected("adapter:GroupMessage:other", "chat_batch")[1],
            "内置默认",
        )
        self.assertEqual(
            {
                k: v
                for k, v in self.bot.enrollment.records[group].items()
                if k != "messageTemplates"
            },
            before,
        )
        self.assertEqual(self.calls, [])
        for command in ("恢复 聊天播报", "保存"):
            event = self.event("/dst 模板 " + command)
            await self.bot.on_message(event)
        self.assertEqual(self.bot.message_templates.selected(group, "chat_batch")[1], "内置默认")

    async def test_global_optimistic_conflict_does_not_overwrite_intervening_config(self):
        event = self.event("/dst 模板 全局 编辑 管理回执 📋 {body}")
        await self.bot.on_message(event)
        self.bot.config["mode"] = "shortcut"
        event = self.event("/dst 模板 全局 保存")
        await self.bot.on_message(event)
        self.assertIn("CONFIG_CONFLICT", self.output(event))
        self.assertEqual(self.bot.config["message_templates"], [])
        self.assertEqual(self.bot.config["mode"], "shortcut")

    async def test_edit_arriving_during_save_is_preserved_as_a_new_draft(self):
        await self.enable()
        group = "adapter:GroupMessage:789"
        await self.bot.on_message(self.event("/dst 模板 编辑 聊天播报 第一版 {lines}"))
        entered, release = asyncio.Event(), asyncio.Event()

        async def save(*args):
            entered.set()
            await release.wait()

        self.bot.enrollment.save = save
        saving = asyncio.create_task(self.bot.on_message(self.event("/dst 模板 保存")))
        await asyncio.wait_for(entered.wait(), 1)
        await self.bot.on_message(self.event("/dst 模板 编辑 聊天播报 第二版 {lines}"))
        release.set()
        await saving
        draft = next(iter(self.bot.template_editor.drafts.values()))
        self.assertIn("第二版", draft["rows"][0]["body"])
        self.assertIn("第一版", self.bot.message_templates.selected(group, "chat_batch")[0])
        event = self.event("/dst 模板 保存")
        await self.bot.on_message(event)
        self.assertIn("已保存", self.output(event))
        self.assertIn("第二版", self.bot.message_templates.selected(group, "chat_batch")[0])

    async def test_invalid_edit_draft_expiry_and_disabled_group_are_safe(self):
        event = self.event("/dst 模板 编辑 房间概况 {body}")
        await self.bot.on_message(event)
        self.assertIn("已启用的群", self.output(event))
        await self.enable()
        event = self.event("/dst 模板 编辑 管理回执 看起来成功了 {title}")
        await self.bot.on_message(event)
        self.assertIn("必须保留", self.output(event))
        self.assertEqual(self.bot.template_editor.drafts, {})
        event = self.event("/dst 模板 编辑 聊天播报 {lines}")
        await self.bot.on_message(event)
        self.bot.template_editor.clock = lambda: float("inf")
        event = self.event("/dst 模板 保存")
        await self.bot.on_message(event)
        self.assertIn("没有待保存草稿", self.output(event))
        self.assertEqual(self.calls[-1][0], "api_listRooms")
