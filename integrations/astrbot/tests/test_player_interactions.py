"""Player selection and everyday actions, without models or real game writes."""

import copy
import json
import time
import unittest
from dataclasses import replace
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock

from test_companion import actor
from test_workbench import recipe

from astrbot_plugin_dst_companion.engine import Engine
from astrbot_plugin_dst_companion.gateway import QueryError
from astrbot_plugin_dst_companion.jobs import Actions
from astrbot_plugin_dst_companion.queries import Queries
from astrbot_plugin_dst_companion.routing import Intent, parse
from astrbot_plugin_dst_companion.state import State, Target
from astrbot_plugin_dst_companion.capabilities import Directory


class PlayerInteractionTests(unittest.IsolatedAsyncioTestCase):
    def test_located_room_clause_is_not_consumed_as_a_player_name(self):
        for room, ident in [("营地", "r1"), ("远山", "r2")]:
            for verb, enabled in [("打开", True), ("关闭", False)]:
                value = parse(f"请在{room}为示例玩家{verb}无敌模式", {"营地": "r1", "远山": "r2"})
                self.assertEqual(value.room, ident)
                self.assertEqual(value.parameters, {"player_id": "示例玩家", "mode": "super_god", "enabled": enabled})
        self.assertIsNone(parse("请在未知房间为示例玩家打开无敌模式", {"营地": "r1"}))

    def fixture(self, items=None):
        gateway = NS(call=AsyncMock())
        state = State()
        actions = NS(submit_capability=AsyncMock(return_value="指令已执行"),
                     submitted_result=AsyncMock(return_value={"status": "succeeded", "complete": True}))
        directory = Directory(gateway)
        directory.items = AsyncMock(return_value=items or [recipe(risk="low")])
        engine = Engine(gateway, state, directory=directory, actions=actions)
        engine.queries.rooms = AsyncMock(return_value=[{"id": "r1", "name": "示例房间"}])
        engine.queries.resolve = AsyncMock(return_value={"status": "resolved", "match": "exact_name",
            "items": [{"id": "KU_TEST123", "name": "示例玩家"}]})
        manager = replace(actor(role="game-manage"), workbench_mode="all")
        return engine, manager

    async def test_verified_quote_selects_resurrection_and_remembers_target_for_repeat(self):
        engine, manager = self.fixture()
        target = Target("r1", "KU_TEST123", "示例玩家")
        result = await engine.handle(manager, "复活", woken=True, quote="notice", reference=target)
        self.assertEqual(result.outcome, "matched")
        self.assertEqual(engine.actions.submit_capability.call_args.args[3], {"player_id": "KU_TEST123"})
        self.assertEqual(engine.state.target(manager.key), target)
        result = await engine.handle(manager, "再次复活", woken=True)
        self.assertEqual(result.outcome, "matched")
        self.assertEqual(engine.actions.submit_capability.await_count, 2)
        self.assertIsNone(engine.state.pending_form(manager.key))
        engine.queries.resolve.assert_not_awaited()

    async def test_repeat_without_context_or_another_user_needs_player_not_nickname_again(self):
        engine, manager = self.fixture()
        for text in ("复活", "再次复活", "再复活一次"):
            result = await engine.handle(manager, text, woken=True)
            self.assertEqual(result.outcome, "needs_selection")
            self.assertIn("玩家昵称", result.text)
        engine.queries.resolve.assert_not_awaited()
        engine.actions.submit_capability.assert_not_awaited()
        seq = engine.state.begin(manager.key)
        engine.state.remember(manager.key, seq, Target("r1", "KU_TEST123"), "已执行")
        other = replace(manager, key=(*manager.key[:-1], "another-user"), user="another-user")
        result = await engine.handle(other, "再次复活", woken=True)
        self.assertEqual(result.outcome, "needs_selection")
        engine.actions.submit_capability.assert_not_awaited()

    async def test_uncertain_execution_cannot_be_repeated_using_remembered_target(self):
        engine, manager = self.fixture()
        engine.actions.submitted_result.return_value = {"status": "uncertain", "complete": False}
        await engine.handle(manager, "复活KU_TEST123", woken=True)
        self.assertIsNone(engine.state.target(manager.key))
        result = await engine.handle(manager, "再次复活", woken=True)
        self.assertEqual(result.outcome, "needs_selection")
        self.assertEqual(engine.actions.submit_capability.await_count, 1)

    async def test_quote_cannot_override_an_explicit_player_or_authorize_readers(self):
        engine, manager = self.fixture()
        result = await engine.handle(manager, "复活KU_OTHER123", woken=True, quote="notice",
                                     reference=Target("r1", "KU_TEST123"))
        self.assertEqual(result.outcome, "matched")
        self.assertEqual(engine.actions.submit_capability.call_args.args[3], {"player_id": "KU_OTHER123"})
        engine.actions.submit_capability.reset_mock()
        result = await engine.handle(actor(), "复活", woken=True, quote="notice",
                                     reference=Target("r1", "KU_TEST123"))
        self.assertEqual(result.outcome, "handled_error")
        engine.actions.submit_capability.assert_not_awaited()

    async def test_pending_player_prompt_accepts_verified_notification_quote(self):
        for kind in ("resurrection", "inventory"):
            with self.subTest(kind=kind):
                engine, manager = self.fixture()
                target = Target("r1", "KU_TEST123", "示例玩家")
                if kind == "inventory":
                    engine.queries.run = AsyncMock(return_value=("背包结果", target))
                await engine.handle(manager, "复活" if kind == "resurrection" else "查背包", woken=True)
                result = await engine.handle(manager, "他", woken=True, quote="death-notice", reference=target)
                self.assertEqual(result.outcome, "matched")
                self.assertEqual(result.target.player, "KU_TEST123")

    async def test_general_online_list_cannot_select_the_only_player_for_resurrection(self):
        engine, manager = self.fixture()
        engine.queries.run = AsyncMock(return_value=("在线一人", Target("r1", "KU_TEST123")))
        await engine.handle(manager, "谁在线", woken=True)
        self.assertEqual(engine.state.target(manager.key).player, "")
        result = await engine.handle(manager, "复活", woken=True)
        self.assertEqual(result.outcome, "needs_selection")
        engine.actions.submit_capability.assert_not_awaited()

    async def test_optional_feedback_defaults_without_prompt_but_required_switch_does_not(self):
        item = recipe("workbench.set_player_mode", "high")
        item["parameters"]["properties"].update(
            enabled={"type": "boolean"}, mode={"type": "string", "enum": ["super_god"]},
            feedback={"type": "string", "enum": ["panel_detail", "off"], "default": "panel_detail"})
        item["parameters"]["required"] += ["mode", "enabled"]
        engine, manager = self.fixture([item])
        result = await engine.handle(manager, "给KU_TEST123开启无敌模式", woken=True)
        self.assertEqual(result.outcome, "matched")
        self.assertEqual(engine.actions.submit_capability.call_args.args[3],
                         {"player_id": "KU_TEST123", "mode": "super_god", "enabled": True, "feedback": "panel_detail"})
        engine.actions.submit_capability.reset_mock()
        result = await engine.execute(manager, Intent("capability", capability=item["id"],
                                      parameters={"player_id": "KU_TEST123", "mode": "super_god"}), Target("r1"))
        self.assertEqual(result.outcome, "needs_selection")
        engine.actions.submit_capability.assert_not_awaited()

    async def test_reversible_player_mode_executes_its_frozen_preview_in_one_reply(self):
        item = recipe("workbench.set_player_mode", "high")
        item["parameters"]["properties"].update(
            mode={"type": "string", "enum": ["super_god", "creative"]},
            enabled={"type": "boolean"},
            phase={"type": "string", "enum": ["preview", "execute"]},
            preview_id={"type": "string", "maxLength": 128},
            confirmed={"type": "boolean", "enum": [True]},
        )
        item["parameters"]["required"] += ["mode", "enabled"]
        item["interaction"] = {"kind": "preview-confirm-v1", "resultVersion": 2, "effectVersion": "1"}
        item["tier"], item["confirm"] = "R1", "none"
        engine, manager = self.fixture([item])
        manager = replace(manager, workbench_mode="standard", write_allowlist=("player.resurrect",))
        preview = {"version": 2, "state": "preview", "previewId": "preview-safe", "matched": 1,
                   "detailsComplete": True, "observedAt": 100, "expiresAt": 130}
        engine.actions.submit_capability.side_effect = ["预览完成", "已确认生效：1"]
        engine.actions.submitted_result.side_effect = [
            {"status": "succeeded", "complete": True, "data": {"batch": preview}},
            {"status": "succeeded", "complete": True, "data": {"batch": {"state": "completed", "verified": 1}}},
        ]
        reply = await engine.handle(manager, "给示例玩家开启无敌模式", woken=True)
        self.assertEqual(reply.outcome, "matched", reply.text)
        self.assertNotIn("请在 30 秒", reply.text)
        self.assertIsNone(engine.state.pending_form(manager.key))
        self.assertEqual(engine.actions.submit_capability.await_count, 2)
        calls = engine.actions.submit_capability.call_args_list
        self.assertEqual(calls[0].kwargs["journal_step"], "mode-preview")
        self.assertEqual(calls[0].args[3]["phase"], "preview")
        self.assertEqual(calls[1].kwargs["journal_step"], "mode-execute")
        self.assertEqual(calls[1].args[3]["preview_id"], "preview-safe")
        self.assertEqual(calls[1].args[3]["confirmed"], True)

    async def test_published_single_player_modes_all_execute_once_without_a_reply(self):
        catalog = json.loads((Path(__file__).resolve().parents[3] /
                              "internal/capreg/testdata/bot_catalog_snapshot.json").read_text())
        item = next(row for row in catalog if row["id"] == "workbench.set_player_mode")
        modes = (("普通无敌", "invincible"), ("制造", "creative"), ("超级无敌", "super_god"),
                 ("锁血", "health_lock"), ("一击必杀", "one_hit"), ("隐身", "invisible"),
                 ("踏水", "water_walk"), ("穿墙", "noclip"))
        requests = [(f"给示例玩家{verb}{name}模式", mode, enabled)
                    for name, mode in modes for verb, enabled in (("开启", True), ("关闭", False))]
        requests.append(("给示例玩家解除全部模式", "clear_all", False))
        for text, mode, enabled in requests:
            with self.subTest(text=text):
                engine, manager = self.fixture([item])
                manager = replace(manager, workbench_mode="standard", write_allowlist=("player.resurrect",))
                preview = {"version": 2, "state": "preview", "previewId": "preview-safe", "matched": 1,
                           "detailsComplete": True, "observedAt": 100, "expiresAt": 130}
                engine.actions.submit_capability.side_effect = ["预览完成", "已确认生效：1"]
                engine.actions.submitted_result.side_effect = [
                    {"status": "succeeded", "complete": True, "data": {"batch": preview}},
                    {"status": "succeeded", "complete": True,
                     "data": {"batch": {"state": "completed", "verified": 1}}},
                ]
                reply = await engine.handle(manager, text, woken=True)
                self.assertEqual(reply.outcome, "matched", reply.text)
                self.assertIsNone(engine.state.pending_form(manager.key))
                self.assertEqual(engine.actions.submit_capability.await_count, 2)
                first, second = engine.actions.submit_capability.call_args_list
                self.assertEqual(first.args[3]["phase"], "preview")
                self.assertEqual(first.args[3]["mode"], mode)
                self.assertEqual(first.args[3]["enabled"], enabled)
                self.assertEqual(second.args[3]["phase"], "execute")
                self.assertEqual(second.args[3]["mode"], mode)
                self.assertEqual(second.args[3]["confirmed"], True)

    def test_player_mode_aliases_and_clear_all_are_direct_requests(self):
        for phrase, mode in (("一击秒杀", "one_hit"), ("必杀", "one_hit"),
                             ("隐形", "invisible"), ("水上漂", "water_walk"),
                             ("无碰撞", "noclip")):
            with self.subTest(phrase=phrase):
                self.assertEqual(parse(f"给示例玩家开启{phrase}模式", {}).parameters["mode"], mode)
        for text in ("给示例玩家解除全部模式", "解除示例玩家全部模式", "关闭全部面板模式"):
            with self.subTest(text=text):
                intent = parse(text, {})
                self.assertEqual(intent.capability, "workbench.set_player_mode")
                self.assertEqual(intent.parameters["mode"], "clear_all")
                self.assertIs(intent.parameters["enabled"], False)

    async def test_reversible_mode_does_not_execute_an_ambiguous_preview(self):
        item = recipe("workbench.set_player_mode", "high")
        item["parameters"]["properties"].update(
            mode={"type": "string", "enum": ["super_god"]}, enabled={"type": "boolean"},
            phase={"type": "string", "enum": ["preview", "execute"]},
            preview_id={"type": "string", "maxLength": 128}, confirmed={"type": "boolean", "enum": [True]})
        item["parameters"]["required"] += ["mode", "enabled"]
        item["interaction"] = {"kind": "preview-confirm-v1", "resultVersion": 2, "effectVersion": "1"}
        item["tier"], item["confirm"] = "R1", "none"
        engine, manager = self.fixture([item])
        preview = {"version": 2, "state": "preview", "previewId": "preview-safe", "matched": 2,
                   "detailsComplete": True, "observedAt": 100, "expiresAt": 130}
        engine.actions.submitted_result.return_value = {"status": "succeeded", "complete": True, "data": {"batch": preview}}
        reply = await engine.handle(manager, "给示例玩家开启无敌模式", woken=True)
        self.assertEqual(reply.outcome, "handled_error", reply.text)
        self.assertEqual(engine.actions.submit_capability.await_count, 1)

    async def test_reversible_mode_never_executes_without_a_complete_preview(self):
        item = recipe("workbench.set_player_mode", "high")
        item["parameters"]["properties"].update(
            mode={"type": "string", "enum": ["super_god"]}, enabled={"type": "boolean"},
            phase={"type": "string", "enum": ["preview", "execute"]},
            preview_id={"type": "string", "maxLength": 128}, confirmed={"type": "boolean", "enum": [True]})
        item["parameters"]["required"] += ["mode", "enabled"]
        item["interaction"] = {"kind": "preview-confirm-v1", "resultVersion": 2, "effectVersion": "1"}
        item["tier"], item["confirm"] = "R1", "none"
        for receipt in (None, {"status": "running", "complete": False},
                        {"status": "failed", "complete": True, "data": {"batch": {"state": "preview", "previewId": "sample"}}},
                        {"status": "succeeded", "complete": True, "data": {"batch": {"state": "preview"}}}):
            with self.subTest(receipt=receipt):
                engine, manager = self.fixture([item])
                engine.actions.submitted_result.return_value = receipt
                reply = await engine.handle(manager, "给示例玩家开启无敌模式", woken=True)
                self.assertEqual(reply.outcome, "handled_error", reply.text)
                self.assertIn("尚未修改", reply.text)
                self.assertEqual(engine.actions.submit_capability.await_count, 1)
                self.assertIsNone(engine.state.target(manager.key))

    async def test_reversible_mode_does_not_claim_success_without_execution_receipt(self):
        item = recipe("workbench.set_player_mode", "high")
        item["parameters"]["properties"].update(
            mode={"type": "string", "enum": ["super_god"]}, enabled={"type": "boolean"},
            phase={"type": "string", "enum": ["preview", "execute"]},
            preview_id={"type": "string", "maxLength": 128}, confirmed={"type": "boolean", "enum": [True]})
        item["parameters"]["required"] += ["mode", "enabled"]
        item["interaction"] = {"kind": "preview-confirm-v1", "resultVersion": 2, "effectVersion": "1"}
        item["tier"], item["confirm"] = "R1", "none"
        engine, manager = self.fixture([item])
        preview = {"version": 2, "state": "preview", "previewId": "preview-safe", "matched": 1,
                   "detailsComplete": True, "observedAt": 100, "expiresAt": 130}
        engine.actions.submit_capability.side_effect = ["预览完成", "请求已提交"]
        engine.actions.submitted_result.side_effect = [
            {"status": "succeeded", "complete": True, "data": {"batch": preview}}, None]
        reply = await engine.handle(manager, "给示例玩家开启无敌模式", woken=True)
        self.assertEqual(reply.outcome, "handled_error", reply.text)
        self.assertIn("结果尚未确认", reply.text)
        self.assertEqual(engine.actions.submit_capability.await_count, 2)
        self.assertIsNone(engine.state.target(manager.key))

    async def test_preview_and_execution_have_distinct_persistent_message_identities(self):
        item = recipe("workbench.set_player_mode", "high")
        item["parameters"]["properties"].update(
            mode={"type": "string", "enum": ["super_god"]}, enabled={"type": "boolean"},
            phase={"type": "string", "enum": ["preview", "execute"]},
            preview_id={"type": "string", "maxLength": 128}, confirmed={"type": "boolean", "enum": [True]})
        item["parameters"]["required"] += ["mode", "enabled"]
        gateway = NS(call=AsyncMock(return_value={"status": "succeeded", "complete": True, "data": {}}),
                     authorize_receipt=AsyncMock())
        directory = Directory(gateway)
        directory.items = AsyncMock(return_value=[item])
        preview = {"status": "succeeded", "complete": True, "data": {"batch": {"state": "preview"}}}
        executed = {"status": "succeeded", "complete": True, "data": {"batch": {"state": "completed"}}}
        directory.execute = AsyncMock(side_effect=[preview, executed])
        records = {}

        async def load():
            return copy.deepcopy(records)

        async def save(value):
            records.clear()
            records.update(copy.deepcopy(value))

        actions = Actions(gateway, load, save, directory=directory)
        self.addAsyncCleanup(actions.close)
        manager = replace(actor(role="game-manage"), workbench_mode="standard")
        target = Target("r1", "KU_TEST123")
        first = {"player_id": "KU_TEST123", "mode": "super_god", "enabled": True, "phase": "preview"}
        second = {**first, "phase": "execute", "preview_id": "preview-safe", "confirmed": True}
        for step, parameters in (("mode-preview", first), ("mode-execute", second)):
            await actions.submit_capability(manager, item, target, parameters, time.monotonic() + 5, journal_step=step)
        self.assertEqual(directory.execute.await_count, 2)
        self.assertEqual(len(records), 2)
        self.assertEqual(await actions.submitted_result(manager, journal_step="mode-preview"), preview)
        self.assertEqual(await actions.submitted_result(manager, journal_step="mode-execute"), executed)
        for step, parameters in (("mode-preview", first), ("mode-execute", second)):
            await actions.submit_capability(manager, item, target, parameters, time.monotonic() + 5, journal_step=step)
        self.assertEqual(directory.execute.await_count, 2)

    async def test_simple_modes_prefer_authorized_dedicated_capability_including_off(self):
        for word, enabled, mode, capability in (("开启", True, "无敌", "player.god_mode"),
                ("关闭", False, "无敌", "player.god_mode"), ("开启", True, "制造", "player.creative_mode")):
            with self.subTest(word=word, mode=mode):
                item = recipe(capability)
                item["parameters"]["properties"] = {"playerId": {"type": "string", "maxLength": 128}, "enabled": {"type": "boolean"}}
                item["parameters"]["required"] = ["playerId", "enabled"]
                item["parameterHints"] = {"playerId": "player"}
                engine, manager = self.fixture([item])
                manager = replace(manager, write_allowlist=(capability,))
                result = await engine.handle(manager, f"给玩家示例玩家{word}{mode}模式", woken=True)
                self.assertEqual(result.outcome, "matched")
                self.assertEqual(engine.actions.submit_capability.call_args.args[1]["id"], capability)
                self.assertEqual(engine.actions.submit_capability.call_args.args[3], {"playerId": "KU_TEST123", "enabled": enabled})

    async def test_disabled_mode_capability_is_not_executed(self):
        engine, manager = self.fixture()
        result = await engine.handle(manager, "给示例玩家开启无敌模式", woken=True)
        self.assertEqual(result.outcome, "handled_error")
        engine.actions.submit_capability.assert_not_awaited()

    async def test_mode_fallback_keeps_the_same_effect_when_dedicated_is_not_authorized(self):
        catalog = json.loads((Path(__file__).resolve().parents[3] /
                              "internal/capreg/testdata/bot_catalog_snapshot.json").read_text())
        items = [row for row in catalog if row["id"] in
                 {"workbench.set_player_mode", "player.god_mode", "player.creative_mode"}]
        for phrase, mode in (("无敌", "super_god"), ("上帝", "super_god"),
                             ("普通无敌", "invincible"), ("仅免伤", "invincible"),
                             ("制造", "creative")):
            for verb, enabled in (("开启", True), ("关闭", False)):
                with self.subTest(phrase=phrase, verb=verb):
                    engine, manager = self.fixture(items)
                    manager = replace(manager, workbench_mode="standard", write_allowlist=())
                    preview = {"version": 2, "state": "preview", "previewId": "preview-safe", "matched": 1,
                               "detailsComplete": True, "observedAt": 100, "expiresAt": 130}
                    engine.actions.submitted_result.side_effect = [
                        {"status": "succeeded", "complete": True, "data": {"batch": preview}},
                        {"status": "succeeded", "complete": True, "data": {"batch": {"state": "completed", "verified": 1}}},
                    ]
                    result = await engine.handle(manager, f"给示例玩家{verb}{phrase}模式", woken=True)
                    self.assertEqual(result.outcome, "matched", result.text)
                    self.assertEqual(engine.actions.submit_capability.await_count, 2)
                    for call in engine.actions.submit_capability.call_args_list:
                        self.assertEqual(call.args[1]["id"], "workbench.set_player_mode")
                        self.assertEqual(call.args[3]["mode"], mode)
                        self.assertEqual(call.args[3]["enabled"], enabled)

    async def test_ordinary_immunity_never_uses_the_full_god_endpoint(self):
        item = recipe("player.god_mode")
        item["parameters"]["properties"] = {"playerId": {"type": "string"}, "enabled": {"type": "boolean"}}
        item["parameters"]["required"] = ["playerId", "enabled"]
        item["parameterHints"] = {"playerId": "player"}
        engine, manager = self.fixture([item])
        manager = replace(manager, write_allowlist=("player.god_mode",))
        reply = await engine.handle(manager, "给示例玩家开启普通无敌模式", woken=True)
        self.assertEqual(reply.outcome, "handled_error")
        engine.actions.submit_capability.assert_not_awaited()

    async def test_multi_player_operation_does_not_fill_both_players_from_one_quote(self):
        item = recipe("workbench.player_pair")
        item["parameters"]["properties"]["other_player"] = {"type": "string", "maxLength": 128}
        item["parameters"]["required"].append("other_player")
        item["parameterHints"]["other_player"] = "player"
        engine, manager = self.fixture([item])
        result = await engine.execute(manager, Intent("capability", capability=item["id"]), Target("r1", "KU_TEST123"))
        self.assertEqual(result.outcome, "needs_selection")
        self.assertEqual(engine.state.pending_form(manager.key)["parameters"], {})
        engine.actions.submit_capability.assert_not_awaited()

    async def test_new_player_quote_cannot_confirm_an_old_dangerous_action(self):
        item = recipe("workbench.clear_player_inventory", "critical")
        engine, manager = self.fixture([item])
        await engine.execute(manager, Intent("capability", capability=item["id"],
                             parameters={"player_id": "KU_TEST123"}), Target("r1"))
        result = await engine.handle(manager, "确认", woken=True, quote="other-notice", reference=Target("r1", "KU_OTHER123"))
        self.assertEqual(result.outcome, "handled_error")
        engine.actions.submit_capability.assert_not_awaited()

    def test_reported_grant_phrases_and_modes_have_deterministic_targets(self):
        for text in ("给玩家lcy 发五个草", "给玩家lcy发五个草"):
            with self.subTest(text=text):
                intent = parse(text, {})
                self.assertEqual(intent.capability, "player.give_item")
                self.assertEqual(intent.parameters, {"playerId": "玩家lcy", "item": "草", "count": 5})
        for text in ("给玩家lcy开启无敌模式", "给 玩家lcy 开启无敌模式", "给lcy开启无敌", "给lcy开启上帝模式"):
            with self.subTest(text=text):
                intent = parse(text, {})
                self.assertEqual(intent.capability, "workbench.set_player_mode")
                self.assertTrue(intent.parameters["enabled"])
        for text in ("不要给lcy开启无敌", "如果给lcy开启无敌", "给lcy开启无敌然后重启"):
            self.assertIsNone(parse(text, {}))


class PlayerDescriptorTests(unittest.IsolatedAsyncioTestCase):
    async def test_descriptor_fallback_is_shared_by_reads_and_writes(self):
        result = {"status": "resolved", "items": [{"id": "KU_TEST123", "name": "示例玩家"}]}
        gateway = NS(call=AsyncMock(side_effect=[{"status": "not_found", "items": []}, result]))
        self.assertEqual(await Queries(gateway).resolve(actor(), "r1", "玩家示例玩家", time.monotonic()+3), result)
        self.assertEqual([call.args[2]["query"]["query"] for call in gateway.call.call_args_list], ["玩家示例玩家", "示例玩家"])

    async def test_existing_full_nickname_ambiguity_and_unavailable_never_strip_prefix(self):
        for result in ({"status": "resolved", "items": [{"id": "KU_FULL123", "name": "玩家示例玩家"}]},
                       {"status": "ambiguous", "items": []}, {"status": "unavailable", "items": []}):
            with self.subTest(result=result):
                gateway = NS(call=AsyncMock(return_value=copy.deepcopy(result)))
                self.assertEqual(await Queries(gateway).resolve(actor(), "r1", "玩家示例玩家", time.monotonic()+3), result)
                gateway.call.assert_awaited_once()

    async def test_read_failure_never_guesses_another_identity(self):
        gateway = NS(call=AsyncMock(side_effect=QueryError("PERMISSION_DENIED")))
        with self.assertRaises(QueryError):
            await Queries(gateway).resolve(actor(), "r1", "玩家示例玩家", time.monotonic()+3)
        gateway.call.assert_awaited_once()
