"""Historical reads and save-source selection use the real read gateway."""

import asyncio
import sys
import time
import unittest
from dataclasses import replace
from datetime import datetime
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock

sys.path.insert(0, str(Path(__file__).resolve().parent))
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from test_companion import FakeGateway, actor, mcp
from astrbot_plugin_dst_companion.engine import Engine
from astrbot_plugin_dst_companion.gateway import Gateway, QueryError, unpack, read_result
from astrbot_plugin_dst_companion.generic import GenericOperations
from astrbot_plugin_dst_companion.policy import Policy
from astrbot_plugin_dst_companion.routing import parse, read_request_intent
from astrbot_plugin_dst_companion.state import State, Target
from astrbot_plugin_dst_companion import formatters


def failed_receipt(data, status="failed"):
    # Invoke failures carry their error directly; successful native API results
    # use httpStatus/data. Keep both real wire formats in regression tests.
    return NS(isError=False, structuredContent={"status": status, "data": data})


class HistoricalReadTests(unittest.IsolatedAsyncioTestCase):
    async def test_room_history_never_resolves_a_time_clause_or_uses_old_player_focus(self):
        class HistoryGateway(FakeGateway):
            async def call(self, who, name, args, deadline, **kw):
                if name == "api_listRoomPlayerActivity":
                    self.calls.append((name, args))
                    return {"items": [], "coverage": [{"complete": False}]}
                return await super().call(who, name, args, deadline, **kw)

        gateway, state, who = HistoryGateway(), State(), actor()
        state.remember(who.key, state.begin(who.key), Target("r1", "KU_SYNTH001", "旧目标"), "旧查询")
        engine = Engine(gateway, state)
        reply = await engine.handle(who, "查询玩家最近两天做了什么", woken=True)
        self.assertEqual(reply.outcome, "matched", reply.text)
        self.assertIn("全员活动", reply.text)
        self.assertIn("48 小时", reply.text)
        self.assertIn("采集缺口", reply.text)
        self.assertIn("不代表玩家没有活动", reply.text)
        self.assertFalse(any(n == "api_resolveRoomPlayer" for n, _ in gateway.calls))
        q = next(args["query"] for n, args in gateway.calls if n == "api_listRoomPlayerActivity")
        self.assertNotIn("userId", q)
        self.assertEqual((datetime.fromisoformat(q["to"]) - datetime.fromisoformat(q["from"])).total_seconds(), 48 * 3600)
        self.assertEqual(q["limit"], 20)

    async def test_named_offline_player_and_quoted_notice_filter_history(self):
        class HistoryGateway(FakeGateway):
            async def call(self, who, name, args, deadline, **kw):
                if name == "api_listRoomPlayerActivity":
                    self.calls.append((name, args))
                    return {"items": [{"name": "示例玩家", "userId": "KU_SYNTH001", "worldId": "Master",
                                       "occurredAt": "2026-01-01T01:00:00Z", "kind": "action.harvest",
                                       "fields": {"targetPrefab": "grass", "count": 2}}]}
                return await super().call(who, name, args, deadline, **kw)

        for text, quoted in [("查询示例玩家最近两天做了什么", False), ("查询他过去48小时的活动记录", True), ("查询玩家最近两天做了什么", True)]:
            with self.subTest(text=text, quoted=quoted):
                gw, state, who = HistoryGateway(), State(), actor()
                gw.candidates = [{"id": "KU_SYNTH001", "name": "示例玩家", "online": False}]
                if quoted:
                    state.associate(who.scope, "notice", Target("r1", "KU_SYNTH001", "示例玩家"))
                reply = await Engine(gw, state).handle(who, text, woken=True, quote="notice" if quoted else "")
                self.assertEqual(reply.outcome, "matched", reply.text)
                self.assertIn("采集", reply.text)
                q = next(args["query"] for n, args in gw.calls if n == "api_listRoomPlayerActivity")
                self.assertEqual(q["userId"], "KU_SYNTH001")
                self.assertFalse(any(n == "api_listRoomPlayers" for n, _ in gw.calls))

    def test_history_routing_is_bounded_and_does_not_steal_other_requests(self):
        for q in ["查询玩家最近两天做了什么", "全员最近2天活动", "查询示例玩家过去48小时的活动记录", "1服玩家活动"]:
            with self.subTest(q=q):
                self.assertEqual(read_request_intent(q, {"1服": "r1"}).kind, "activity")
        self.assertEqual(parse("查询示例玩家的背包", {}).kind, "inventory")
        for q in ["分析玩家最近两天做了什么", "查询玩家最近两天做了什么然后重启", "查询玩家最近99周做了什么",
                  "帮我总结示例玩家最近两天做了什么", "查询总结示例玩家最近两天做了什么", "查询示例玩家最近两天的活动和轨迹"]:
            self.assertIsNone(parse(q, {}), q)

    async def test_explicit_all_overrides_quote_and_slow_world_labels_do_not_erase_history(self):
        class HistoryGateway(FakeGateway):
            async def call(self, who, name, args, deadline, **kw):
                if name == "api_listRoomPlayerActivity":
                    self.calls.append((name, args))
                    return {"items": [{"kind": "action.harvest", "worldId": "unknown-world", "name": "示例玩家"}]}
                if name == "api_listRoomWorlds":
                    self.calls.append((name, args))
                    self.until = deadline
                    await asyncio.Event().wait()
                return await super().call(who, name, args, deadline, **kw)
        gw, state, who = HistoryGateway(), State(), actor()
        state.associate(who.scope, "notice", Target("r1", "KU_SYNTH001", "示例玩家"))
        started = time.monotonic()
        reply = await Engine(gw, state).handle(who, "查询全员最近两天活动记录", woken=True, quote="notice")
        self.assertEqual(reply.outcome, "matched", reply.text)
        self.assertIn("全员活动", reply.text)
        self.assertIn("采集", reply.text)
        self.assertIn("世界名称未确认", reply.text)
        self.assertNotIn("unknown-world", reply.text)
        self.assertLess(gw.until - started, 2)
        self.assertLess(time.monotonic() - started, 3)
        self.assertNotIn("userId", next(a["query"] for n, a in gw.calls if n == "api_listRoomPlayerActivity"))

    async def test_trajectory_requires_player_and_uses_compact_positions_for_offline_player(self):
        class HistoryGateway(FakeGateway):
            async def call(self, who, name, args, deadline, **kw):
                if name == "api_getRoomPlayerTrajectory":
                    self.calls.append((name, args))
                    return {"items": [{"worldId": "m", "occurredAt": "2026-01-01T01:00:00Z",
                                       "position": {"x": 12.5, "z": -3.0}}], "coverage": []}
                return await super().call(who, name, args, deadline, **kw)

        gw, state, who = HistoryGateway(), State(), actor()
        gw.candidates = [{"id": "KU_SYNTH001", "name": "示例玩家", "online": False}]
        engine = Engine(gw, state)
        reply = await engine.handle(who, "最近两天移动轨迹", woken=True)
        self.assertEqual(reply.outcome, "needs_selection", reply.text)
        self.assertFalse(any(n == "api_getRoomPlayerTrajectory" for n, _ in gw.calls))
        reply = await engine.handle(who, "示例玩家")
        self.assertIn("X 12.5 / Z -3.0", reply.text)
        self.assertIn("地面", reply.text)
        self.assertIn("不会把跨世界", reply.text)
        args = next(a for n, a in gw.calls if n == "api_getRoomPlayerTrajectory")
        self.assertEqual(args["query"]["projection"], "compact")
        self.assertEqual(args["query"]["userId"], "KU_SYNTH001")
        self.assertEqual((datetime.fromisoformat(args["query"]["to"]) - datetime.fromisoformat(args["query"]["from"])).total_seconds(), 48 * 3600)

    def test_history_names_and_default_period_do_not_strip_nickname_suffixes(self):
        for question, kind, player, hours in [
            ("查询超级玩家的活动记录", "activity", "超级玩家", 24),
            ("查询超级玩家最近3小时的战斗记录", "combat", "超级玩家", 3),
            ("查轨迹 示例玩家", "trajectory", "示例玩家", 24),
            ("查询全员过去1周战斗汇总", "combat", "", 168),
        ]:
            with self.subTest(question=question):
                intent = read_request_intent(question, {})
                self.assertEqual((intent.kind, intent.player, intent.parameters["hours"]), (kind, player, hours))

    def test_long_history_keeps_coverage_warning_and_unknown_damage_is_not_zero(self):
        result = {"items": [{"name": "合成" * 60, "kind": "action.harvest", "worldId": "opaque-id",
                            "fields": {"targetPrefab": "synthetic_prefab_" * 12}}
                           for _ in range(20)], "hasMore": True, "coverage": [{"complete": False}]}
        reply = formatters.activity(result, "", 48)
        self.assertLess(len(reply), 2100)
        self.assertIn("采集缺口", reply)
        self.assertIn("未完整展开", reply)
        self.assertNotIn("opaque-id", reply)
        reply = formatters.activity({"items": [{"direction": "incoming", "targetPrefab": "hound", "hits": 2,
                                               "kills": 0, "unknownDamageHits": 2}]}, "示例玩家", 24, view="combat")
        self.assertIn("伤害未确认", reply)
        self.assertNotIn("伤害 0", reply)


class ControlledReadTests(unittest.IsolatedAsyncioTestCase):
    def harness(self, *, inventory_error=None, available=True):
        fixture = FakeGateway()
        fixture.candidates = [{"id": "KU_SYNTH001", "name": "示例玩家", "online": False}]
        calls = []

        async def transport(name, args, timeout, write):
            calls.append((name, args, timeout))
            self.assertFalse(write)
            self.assertEqual(name, "companion_gateway")
            self.assertEqual(args["mode"], "read")
            operation = args["operationId"]
            if operation == "getRoomPlayerInventory" and inventory_error:
                error = inventory_error(args["arguments"])
                if error:
                    return mcp({"state": "failed", "result": {"error": error}})
            if operation == "listRoomPlayerActivity":
                data = {"items": [], "coverage": [{"complete": False}]}
            else:
                data = await fixture.call(actor(), "api_" + operation, args["arguments"], 0)
            return mcp({"state": "succeeded", "result": {"httpStatus": 200, "data": data}})

        native = AsyncMock(return_value=True)
        gateway = Gateway(transport, Policy(native).authorize)
        plugin = NS(gateway=gateway)
        generic = GenericOperations(plugin)
        generic.available = AsyncMock(return_value=available)
        return Engine(gateway, State(), reader=generic.read_native), generic, calls, fixture, native

    async def test_offline_history_shortcut_uses_the_same_controlled_read_transport_as_ai(self):
        engine, generic, calls, fixture, _ = self.harness()
        try:
            reply = await engine.handle(actor(), "查询示例玩家最近两天做了什么", woken=True)
            self.assertEqual(reply.outcome, "matched", reply.text)
            self.assertIn("采集缺口", reply.text)
            args = next(a for _, a, _ in calls if a["operationId"] == "listRoomPlayerActivity")
            self.assertEqual(args["arguments"]["query"]["userId"], "KU_SYNTH001")
            self.assertEqual(args["actor"]["rooms"], ["r1"])
            self.assertEqual(len(args["requestId"]), 64)
            self.assertTrue(all(timeout < 9 for _, _, timeout in calls))
            self.assertFalse(any(n == "api_listRoomPlayers" for n, _ in fixture.calls))
        finally:
            await engine.gateway.close()

    async def test_controlled_inventory_failure_continues_by_explicit_source_and_no_model(self):
        def error(args):
            if "sourceId" in args["query"]:
                self.assertEqual(args["query"]["worldId"], "Caves")
                return None
            return {"code": "SOURCE_AMBIGUOUS", "details": {"candidates": [
                {"sourceId": "a" * 64, "worldId": "Master", "worldName": "Master"},
                {"sourceId": "b" * 64, "worldId": "Caves", "worldName": "Caves"}]}}
        engine, _, calls, _, _ = self.harness(inventory_error=error)
        try:
            reply = await engine.handle(actor(), "查询示例玩家背包", woken=True)
            self.assertIn("多个背包存档", reply.text)
            reply = await engine.handle(actor(), "2")
            self.assertEqual(reply.outcome, "matched", reply.text)
            self.assertIn("存档快照", reply.text)
            self.assertEqual(sum(a["operationId"] == "getRoomPlayerInventory" for _, a, _ in calls), 2)
        finally:
            await engine.gateway.close()

    async def test_legacy_negotiation_allows_fallback_but_failed_read_never_does(self):
        engine, generic, calls, _, _ = self.harness(available=False)
        try:
            value = await generic.read_native(actor(), "api_listRooms", {}, time.monotonic() + 8)
            self.assertIs(value, NotImplemented)
            self.assertEqual(calls, [])
            generic.available.return_value = True
            generic.request = AsyncMock(return_value={"state": "uncertain", "result": {"error": {"code": "SOURCE_AMBIGUOUS"}}})
            with self.assertRaises(QueryError) as error:
                await engine.queries.call(actor(), "api_listRooms", {}, time.monotonic() + 8)
            self.assertEqual(error.exception.code, "RESULT_UNCERTAIN")
            self.assertEqual(calls, [])
            generic.request.assert_awaited_once()
        finally:
            await engine.gateway.close()

    async def test_save_choice_cancel_expiry_scope_and_revocation_do_not_read_again(self):
        def error(args):
            return {"code": "SOURCE_AMBIGUOUS", "details": {"candidates": [
                {"sourceId": "a" * 64, "worldId": "Master", "worldName": "Master"}]}}
        for scenario in ["cancel", "expired", "other_user", "other_group", "config_changed", "permission_revoked"]:
            with self.subTest(scenario=scenario):
                engine, _, calls, _, native = self.harness(inventory_error=error)
                who = actor()
                try:
                    reply = await engine.handle(who, "查询示例玩家背包", woken=True)
                    self.assertEqual(reply.outcome, "needs_selection", reply.text)
                    if scenario == "cancel":
                        reply = await engine.handle(who, "取消")
                        self.assertIn("已取消", reply.text)
                    elif scenario == "expired":
                        now = engine.state.clock()
                        engine.state.clock = lambda: now + 121
                        self.assertEqual((await engine.handle(who, "1")).outcome, "pass")
                    elif scenario in {"other_user", "other_group"}:
                        other = actor("other") if scenario == "other_user" else replace(who, key=("adapter", "bot", "other-group", "user"), scope=("adapter", "bot", "other-group"))
                        self.assertEqual((await engine.handle(other, "1")).outcome, "pass")
                    elif scenario == "config_changed":
                        reply = await engine.handle(replace(who, config_revision="changed"), "1")
                        self.assertEqual(reply.outcome, "handled_error", reply.text)
                    else:
                        native.return_value = False
                        reply = await engine.handle(who, "1")
                        self.assertEqual(reply.outcome, "handled_error", reply.text)
                    self.assertEqual(sum(a["operationId"] == "getRoomPlayerInventory" for _, a, _ in calls), 1)
                finally:
                    await engine.gateway.close()

    async def test_changed_save_and_untyped_failure_are_explained_without_retry(self):
        for code in ["SAVE_CHANGED", "SAVE_INVALID", "PLAYER_SNAPSHOT_NOT_FOUND", "INVENTORY_TIMEOUT", "UNKNOWN_SYNTHETIC"]:
            with self.subTest(code=code):
                engine, _, calls, _, _ = self.harness(inventory_error=lambda args: {"code": code})
                try:
                    reply = await engine.handle(actor(), "查询示例玩家背包", woken=True)
                    self.assertEqual(reply.outcome, "handled_error", reply.text)
                    self.assertNotIn("背包为空", reply.text)
                    self.assertEqual(sum(a["operationId"] == "getRoomPlayerInventory" for _, a, _ in calls), 1)
                finally:
                    await engine.gateway.close()

    def test_pending_empty_and_failed_receipts_do_not_become_empty_history(self):
        for state in ["running", "accepted", "uncertain", "executing", "sent", "timed_out"]:
            with self.subTest(state=state), self.assertRaises(QueryError) as error:
                read_result({"state": state, "result": {"data": {"items": []}}})
            self.assertEqual(error.exception.code, "RESULT_UNCERTAIN")
        for value in [{}, {"state": "succeeded"}, {"state": "succeeded", "result": {"data": None}}]:
            with self.assertRaises(QueryError) as error:
                read_result(value)
            self.assertEqual(error.exception.code, "MCP_RESPONSE_INVALID")


class InventorySourceTests(unittest.IsolatedAsyncioTestCase):
    async def test_player_then_save_source_selection_reads_the_selected_world_without_replay(self):
        fake = FakeGateway()
        calls = []
        who = actor()
        sources = [{"sourceId": ch * 64, "worldId": wid, "worldName": wid,
                    "fileModifiedAt": "2026-01-01T01:00:00Z"}
                   for ch, wid in [("a", "Master"), ("b", "Caves")]]

        async def native(*_):
            return True

        async def transport(name, args, timeout, write):
            calls.append((name, args))
            self.assertFalse(write)
            if name == "api_resolveRoomPlayer":
                exact = args["query"]["query"] == "KU_SYNTH001"
                return mcp({"status": "resolved" if exact else "ambiguous", "match": "exact_id" if exact else "substring",
                            "items": [{"id": "KU_SYNTH001", "name": "示例玩家", "online": False}]})
            if name == "api_getRoomPlayerInventory":
                if "sourceId" not in args["query"]:
                    return failed_receipt({"error": "legacy failure string", "platformError": {"code": "SOURCE_AMBIGUOUS",
                                "message": "无法完成玩家背包读取", "details": {"candidates": sources}}})
                self.assertEqual(args["query"], {"source": "save", "refresh": False, "worldId": "Caves", "sourceId": "b" * 64})
                self.assertEqual(args["path"]["playerId"], "KU_SYNTH001")
                return mcp({"source": "save", "status": "complete", "worldName": "Caves", "inventory": [{"prefab": "cutgrass", "count": 5}]})
            return mcp(await fake.call(who, name, args, timeout))

        gateway = Gateway(transport, Policy(native).authorize)
        engine = Engine(gateway, State())
        try:
            first = await engine.handle(who, "查询示例 背包", woken=True)
            self.assertIn("请确认玩家", first.text)
            second = await engine.handle(who, "1")
            self.assertEqual(second.outcome, "needs_selection", second.text)
            self.assertIn("多个背包存档", second.text)
            self.assertIn("地面", second.text)
            self.assertIn("洞穴", second.text)
            self.assertNotIn("a" * 64, second.text)
            self.assertEqual((await engine.handle(actor("other"), "2")).outcome, "pass")
            final = await engine.handle(who, "2")
            self.assertEqual(final.outcome, "matched", final.text)
            self.assertIn("存档快照", final.text)
            self.assertIn("洞穴", final.text)
            self.assertEqual(sum(n == "api_getRoomPlayerInventory" for n, _ in calls), 2)
        finally:
            await gateway.close()

    def test_failure_details_survive_both_protocols_but_uncertainty_wins(self):
        failure = {"code": "SOURCE_AMBIGUOUS", "details": {"candidates": [{"sourceId": "a" * 64}]}}
        for data in [{"error": "legacy", "platformError": failure}, {"error": failure}]:
            with self.subTest(data=data):
                with self.assertRaises(QueryError) as ctx:
                    unpack(failed_receipt(data))
                self.assertEqual(ctx.exception.code, "SOURCE_AMBIGUOUS")
                self.assertEqual(ctx.exception.details, failure["details"])
                with self.assertRaises(QueryError) as ctx:
                    unpack(failed_receipt(data, status="uncertain"))
                self.assertEqual(ctx.exception.code, "RESULT_UNCERTAIN")
