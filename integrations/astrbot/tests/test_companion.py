import asyncio
import copy
import hashlib
import json
import sys
import time
import unittest
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from astrbot_plugin_dst_companion.engine import Engine
from astrbot_plugin_dst_companion.gateway import Gateway, QueryError, unpack
from astrbot_plugin_dst_companion.jobs import Actions, outcome
from astrbot_plugin_dst_companion.policy import Actor, Policy
from astrbot_plugin_dst_companion.routing import parse
from astrbot_plugin_dst_companion.state import State, Target


def actor(user="user", rooms=("r1",), role="game-read"):
    return Actor(
        ("adapter", "bot", "group", user),
        ("adapter", "bot", "group"),
        user,
        role,
        rooms,
        event=NS(message_obj=NS(message_id="test-message")),
    )


def mcp(data, status="succeeded", http=200):
    return NS(
        isError=False,
        structuredContent={
            "status": status,
            "data": {"httpStatus": http, "data": data},
        },
    )


P = {
    "id": "KU_example",
    "name": "玩家 A 🦌",
    "online": True,
    "presenceStatus": "live",
    "worldConfirmed": True,
    "worldName": "Master",
    "netScore": 0,
    "fields": {"netScore": {"status": "live"}},
}


class FakeGateway:
    def __init__(self):
        self.calls = []
        self.candidates = [P]
        self.failure = None

    async def call(self, actor, name, args, deadline, **kw):
        self.calls.append((name, copy.deepcopy(args)))
        if self.failure:
            raise self.failure
        if name == "api_listRooms":
            return {
                "items": [
                    {"id": "r1", "name": "测试房间"},
                    {"id": "r2", "name": "第二房间"},
                ]
            }
        if name == "api_resolveRoomPlayer":
            return {
                "status": "resolved" if len(self.candidates) == 1 else "ambiguous",
                "items": self.candidates,
                "match": "exact_name",
                "total": len(self.candidates),
            }
        if name == "api_listRoomPlayers":
            return {"items": [P], "online": 1, "total": 1}
        if name == "api_listRoomWorlds":
            return {"items": [{"id": "m", "name": "Master"}]}
        if name == "api_listWorldStates":
            return {
                "items": [
                    {
                        "worldId": "m",
                        "runtimeState": "running",
                        "freshness": "live",
                        "stale": False,
                        "cycles": 12,
                        "season": "autumn",
                        "observedAt": "2026-01-01T01:00:00Z",
                    }
                ]
            }
        if name == "api_getRoomPlayerInventory":
            return {
                "source": "save",
                "status": "partial",
                "capturedAt": "2026-01-01T01:00:00Z",
                "savedAt": "2026-01-01T00:00:00Z",
                "inventory": [{"prefab": "twigs", "stackSize": 12}],
            }
        return {"items": []}


class GrammarTests(unittest.TestCase):
    def test_eighty_common_expressions_and_negative_set(self):
        # 8 independent expressions x 10 polite variants. Every positive is a
        # whole intent; negatives preserve default conversation handling.
        phrases = {
            "1服查房": "status",
            "1服谁在线": "players",
            "查背包 玩家 A 🦌": "inventory",
            "玩家 A 🦌背包里有什么": "inventory",
            "查玩家 玩家 A 🦌": "profile",
            "玩家 A 🦌在哪": "profile",
            "查玩家 A 🦌网络": "network",
            "帮助": "help",
        }
        prefixes = [
            "",
            "请",
            "帮我",
            "麻烦",
            "给我",
            "请看看",
            "帮我看看",
            "麻烦看看",
            "给我看下",
            "看下",
        ]
        for text, kind in phrases.items():
            for prefix in prefixes:
                with self.subTest(text=prefix + text):
                    self.assertEqual(parse(prefix + text, {"1服": "r1"}).kind, kind)
        negatives = [
            "不要重启1服",
            "如果1服重启会怎样",
            "查背包功能怎么实现",
            "为什么玩家卡",
            "分析一下1服状态",
            "查背包 玩家甲 然后重启",
            "随便聊聊",
            "备份失败为什么",
            "```重启1服```",
            "如果查玩家甲网络",
        ]
        for negative in negatives:
            for suffix in ["", "吧", "一下", "谢谢", "。", "！", "?", "～"]:
                with self.subTest(negative=negative + suffix):
                    self.assertIsNone(parse(negative + suffix, {"1服": "r1"}))


class EngineTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.gateway = FakeGateway()
        self.state = State()
        self.engine = Engine(self.gateway, self.state, {"1服": "r1", "2服": "r2"})

    async def test_fast_paths_do_not_require_model(self):
        for text in (
            "1服查房",
            "1服谁在线",
            "查玩家 玩家 A 🦌",
            "查背包 玩家 A 🦌",
            "查玩家 A 🦌网络",
            "帮助",
        ):
            result = await self.engine.handle(actor(), text, woken=True)
            self.assertEqual(
                result.outcome, "needs_selection" if text == "帮助" else "matched", result.text
            )
            self.assertTrue(result.text)
        self.assertFalse(
            any(
                name in {"world_console", "api_executeRawRoomCommand"}
                for name, _ in self.gateway.calls
            )
        )

    async def test_wake_media_and_unrecognised_do_not_call_panel(self):
        self.assertEqual((await self.engine.handle(actor(), "谁在线")).outcome, "pass")
        self.assertEqual(
            (await self.engine.handle(actor(), "为什么卡", woken=True)).outcome, "pass"
        )
        self.assertEqual(self.gateway.calls, [])

    async def test_failed_query_does_not_fall_through_or_claim_zero(self):
        self.gateway.failure = QueryError("QUERY_TIMEOUT")
        result = await self.engine.handle(actor(), "1服谁在线", woken=True)
        self.assertEqual(result.outcome, "handled_error")
        self.assertNotIn("0 人", result.text)
        self.assertEqual(len(self.gateway.calls), 1)

    async def test_ambiguity_and_scoped_number_choice(self):
        self.gateway.candidates = [P, {**P, "id": "KU_second", "online": False}]
        result = await self.engine.handle(actor(), "查背包 玩家 A 🦌", woken=True)
        self.assertEqual(result.outcome, "needs_selection")
        self.state.choices[actor().key]["messageId"] = "choice-123"
        self.assertEqual(
            (await self.engine.handle(actor("someone-else"), "1", quote="choice-123")).outcome,
            "pass",
        )
        self.assertEqual(
            (await self.engine.handle(actor(), "1", quote="older-menu")).outcome, "pass"
        )
        # Resolve chosen KU, not the ambiguous nickname.
        self.gateway.candidates = [P]
        result = await self.engine.handle(actor(), "1")
        self.assertEqual(result.outcome, "matched")
        resolve = [args for name, args in self.gateway.calls if name == "api_resolveRoomPlayer"][-1]
        self.assertEqual(resolve["query"]["query"], "KU_example")

    async def test_room_selection_and_explicit_cross_room_target_conflict(self):
        result = await self.engine.handle(actor(rooms=("r1", "r2")), "谁在线", woken=True)
        self.assertEqual(result.outcome, "needs_selection")
        scope = actor(rooms=("r1", "r2")).scope
        self.state.associate(scope, "notice", Target("r1", "KU_example", "玩家 A 🦌"))
        result = await self.engine.handle(
            actor(rooms=("r1", "r2")), "2服查背包", woken=True, quote="notice"
        )
        self.assertEqual(result.outcome, "needs_selection")
        self.assertNotIn("api_getRoomPlayerInventory", [name for name, _ in self.gateway.calls])

    async def test_trusted_quote_fake_quote_and_self_binding(self):
        self.state.associate(actor().scope, "known", Target("r1", "KU_example", "玩家 A 🦌"))
        result = await self.engine.handle(actor(), "查他背包", woken=True, quote="known")
        self.assertEqual(result.outcome, "matched")
        other = Engine(self.gateway, State())
        self.assertEqual(
            (await other.handle(actor(), "查他背包", woken=True, quote="forged")).outcome,
            "needs_selection",
        )
        self.assertIn("绑定", (await other.handle(actor(), "查我", woken=True)).text)

    async def test_inventory_source_and_network_do_not_invent_metrics(self):
        result = await self.engine.handle(actor(), "查背包 玩家 A 🦌", woken=True)
        self.assertIn("存档快照", result.text)
        self.assertIn("partial", result.text)
        self.assertIn("twigs ×12", result.text)
        self.assertIn("存档时间：2026-01-01T00:00:00Z", result.text)
        self.assertNotIn("01:00:00Z", result.text)
        result = await self.engine.handle(actor(), "查玩家 A 🦌网络", woken=True)
        self.assertIn("netScore 0", result.text)
        self.assertIn("不是 Ping", result.text)
        self.assertNotIn("0ms", result.text)

    async def test_no_management_for_members(self):
        self.assertEqual(
            (await self.engine.handle(actor(), "重启1服", woken=True)).outcome, "denied"
        )
        self.assertEqual(self.gateway.calls, [])

    async def test_duplicate_message_not_resent(self):
        self.assertEqual(
            (await self.engine.handle(actor(), "谁在线", woken=True, message_id="msg")).outcome,
            "matched",
        )
        calls = len(self.gateway.calls)
        result = await self.engine.handle(actor(), "谁在线", woken=True, message_id="msg")
        self.assertEqual(result.text, "")
        self.assertEqual(len(self.gateway.calls), calls)


class StateTests(unittest.TestCase):
    def test_plain_menu_choices_are_short_lived_user_scoped_and_reject_old_quotes(self):
        from astrbot_plugin_dst_companion.routing import Intent

        now = [100]
        s = State(clock=lambda: now[0])
        key = actor().key
        seq = s.begin(key)
        s.choose(key, Intent("help"), [P], "r1", seq)
        s.choice_sent(key, "latest", seq)
        self.assertTrue(s.handles(key, "1", "", False))
        self.assertTrue(s.handles(key, "取消", "", False))
        self.assertFalse(s.handles(actor("other").key, "1", "", False))
        self.assertFalse(s.handles(key, "1", "old", True))
        self.assertIsNone(s.selection(key, "1", "old", True))
        self.assertFalse(s.handles(key, "²", "", True))
        now[0] = 221
        self.assertFalse(s.handles(key, "1", "", False))
        self.assertIsNone(s.selection(key, "1", "latest", True))

    def test_old_candidate_and_late_receipt_cannot_replace_latest_selection(self):
        from astrbot_plugin_dst_companion.routing import Intent

        s = State()
        key = actor().key
        first = s.begin(key)
        second = s.begin(key)
        self.assertTrue(s.choose(key, Intent("inventory"), [P], "r1", second))
        self.assertFalse(s.choose(key, Intent("inventory"), [P], "r1", first))
        s.choice_sent(key, "old-receipt", first)
        self.assertEqual(s.choices[key]["messageId"], "")
        s.choice_sent(key, True, second)
        self.assertEqual(s.choices[key]["messageId"], "")
        s.choice_sent(key, "latest-receipt", second)
        self.assertEqual(s.choices[key]["messageId"], "latest-receipt")

    def test_slow_old_response_cannot_replace_latest_target(self):
        s = State()
        key = actor().key
        first = s.begin(key)
        second = s.begin(key)
        s.remember(key, second, Target("r1", "KU_second"), "second")
        s.remember(key, first, Target("r1", "KU_first"), "first")
        self.assertEqual(s.target(key).player, "KU_second")
        third = s.begin(key)
        s.suspend(key, third)
        self.assertIsNone(s.target(key))

    def test_expiration_scope_and_boolean_not_a_message_id(self):
        now = [0]
        s = State(clock=lambda: now[0])
        a = actor()
        s.associate(a.scope, True, Target("r1"))
        self.assertIsNone(s.quoted(a.scope, "True"))
        seq = s.begin(a.key)
        s.remember(a.key, seq, Target("r1"), "x")
        now[0] = 601
        self.assertIsNone(s.target(a.key))
        s.associate(a.scope, "m", Target("r1"))
        self.assertIsNone(s.quoted(("another", "bot", "group"), "m"))


class GatewayTests(unittest.IsolatedAsyncioTestCase):
    async def test_three_result_layers(self):
        for value in (
            mcp({}, "executing"),
            mcp({}, "uncertain"),
            mcp({}, http=403),
            NS(isError=True),
        ):
            with self.assertRaises(QueryError):
                unpack(value)
        self.assertEqual(unpack(mcp({"id": "j", "status": "queued"}, http=202))["status"], "queued")
        self.assertEqual(outcome({"status": "queued"}), "已排队")
        self.assertEqual(
            outcome({"status": "succeeded", "outcome": "none", "progress": 100}),
            "结果尚未确认",
        )
        self.assertEqual(outcome({"status": "succeeded", "outcome": "full"}), "已完成")

    async def test_hot_reads_merged_waiter_cancel_isolated_and_bounded(self):
        calls = 0
        auth = 0
        gate = asyncio.Event()

        async def authorize(*args):
            nonlocal auth
            auth += 1

        async def transport(*args):
            nonlocal calls
            calls += 1
            await gate.wait()
            return mcp({"items": []})

        g = Gateway(transport, authorize)
        tasks = [
            asyncio.create_task(g.call(actor(str(i)), "api_listRooms", {}, time.monotonic() + 2))
            for i in range(20)
        ]
        await asyncio.sleep(0.02)
        tasks[0].cancel()
        gate.set()
        results = await asyncio.gather(*tasks, return_exceptions=True)
        self.assertEqual(calls, 1)
        self.assertGreaterEqual(auth, 39)
        self.assertTrue(all(isinstance(r, dict) for r in results[1:]))
        await g.close()

    async def test_policy_rechecks_native_and_unknown_tools_fail_closed(self):
        async def native(actor, name):
            return name != "api_getRoomPlayerInventory"

        policy = Policy(native)
        for name, args in [
            ("api_getRoomPlayerInventory", {"path": {"roomId": "r1"}}),
            ("api_executeRawRoomCommand", {}),
            ("api_listRoomPlayers", {"path": {"roomId": "r2"}}),
            ("call_api", {"operationId": "runRoomAction"}),
        ]:
            with self.assertRaises(QueryError):
                await policy.authorize(actor(), name, args)

    async def test_deadline_covers_queue_and_reload(self):
        gate = asyncio.Event()

        async def transport(*args):
            await gate.wait()
            return mcp({})

        async def auth(*args):
            pass

        g = Gateway(transport, auth, limit=1)
        with self.assertRaises(QueryError):
            await g.call(actor(), "api_listRooms", {}, time.monotonic() + 0.02)
        await g.close()
        with self.assertRaises(QueryError):
            await g.call(actor(), "api_listRooms", {}, time.monotonic() + 1)

    async def test_permission_revoked_in_queue_prevents_write(self):
        allowed = True
        transport = AsyncMock(return_value=mcp({}))

        async def authorize(*args):
            if not allowed:
                raise QueryError("PERMISSION_DENIED")

        g = Gateway(transport, authorize, limit=1)
        await g.limit.acquire()
        task = asyncio.create_task(
            g.call(
                actor(role="game-manage"),
                "api_createRoomBackup",
                {},
                time.monotonic() + 1,
                write=True,
            )
        )
        await asyncio.sleep(0)
        allowed = False
        g.limit.release()
        with self.assertRaises(QueryError):
            await task
        transport.assert_not_awaited()
        await g.close()


class JournalTests(unittest.IsolatedAsyncioTestCase):
    async def test_persist_before_send_uncertain_no_replay_and_failure_blocks_send(
        self,
    ):
        journal = {}
        seen = []

        async def load():
            return copy.deepcopy(journal)

        async def save(records):
            journal.clear()
            journal.update(copy.deepcopy(records))
            seen.append("save")

        class G:
            async def call(self, *args, **kwargs):
                seen.append("send")
                raise ConnectionError()

        actions = Actions(G(), load, save)
        from astrbot_plugin_dst_companion.routing import Intent

        with self.assertRaises(QueryError):
            await actions.submit(
                actor(role="game-manage"),
                Intent("backup"),
                Target("r1"),
                time.monotonic() + 1,
            )
        self.assertEqual(seen[:2], ["save", "send"])
        text = await Actions(G(), load, save).submit(
            actor(role="game-manage"),
            Intent("backup"),
            Target("r1"),
            time.monotonic() + 1,
        )
        self.assertIn("没有重新提交", text)
        self.assertEqual(seen.count("send"), 1)

        async def broken(records):
            raise OSError("disk unavailable")

        with self.assertRaises(OSError):
            await Actions(G(), lambda: asyncio.sleep(0, result={}), broken).submit(
                actor(role="game-manage"),
                Intent("backup"),
                Target("r1"),
                time.monotonic() + 1,
            )
        self.assertEqual(seen.count("send"), 1)


class RecoveryTests(unittest.IsolatedAsyncioTestCase):
    async def test_reading_completed_original_job_does_not_start_a_second_notice(self):
        who = actor(role="game-manage")
        journal = {
            "source": {
                "roomId": "r1",
                "jobId": "example-job",
                "state": "tracking_expired",
                "owner": hashlib.sha256(json.dumps(who.key).encode()).hexdigest(),
            }
        }
        notices = AsyncMock()

        async def save(value):
            journal.clear()
            journal.update(copy.deepcopy(value))

        g = NS(
            call=AsyncMock(
                return_value={
                    "id": "example-job",
                    "roomId": "r1",
                    "status": "succeeded",
                    "outcome": "full",
                }
            )
        )
        actions = Actions(
            g,
            lambda: asyncio.sleep(0, result=copy.deepcopy(journal)),
            save,
            notices,
        )
        text = await actions.latest(who, "r1", time.monotonic() + 1)
        self.assertIn("已完成", text)
        self.assertEqual(journal["source"]["state"], "terminal")
        self.assertEqual(actions.watches, {})
        self.assertIsNone(actions.worker)
        notices.assert_not_awaited()
        await actions.close()

    async def test_expired_failed_read_reports_uncertainty_once_without_resubmitting(self):
        journal = {"source": {"roomId": "r1", "jobId": "example-job"}}
        notices = AsyncMock()

        async def save(value):
            journal.clear()
            journal.update(copy.deepcopy(value))

        g = NS(call=AsyncMock(side_effect=QueryError("PANEL_DISCONNECTED")))
        actions = Actions(
            g,
            lambda: asyncio.sleep(0, result=copy.deepcopy(journal)),
            save,
            notices,
            poll_interval=0.001,
            tracking_seconds=0,
        )
        actions.watch(actor(role="game-manage"), "source", journal["source"])
        await asyncio.wait_for(actions.worker, 1)
        self.assertEqual(journal["source"]["state"], "tracking_expired")
        self.assertEqual(notices.await_count, 1)
        self.assertIn("结果未确认", notices.call_args.args[1])
        g.call.assert_awaited_once()
        self.assertEqual(g.call.call_args.args[1], "api_getJob")
        await actions.close()

    async def test_one_job_watcher_confirms_full_outcome_and_persists_terminal(self):
        journal = {}
        notices = []

        async def load():
            return copy.deepcopy(journal)

        async def save(value):
            journal.clear()
            journal.update(copy.deepcopy(value))

        async def notify(who, text):
            notices.append(text)

        class G:
            def __init__(self):
                self.writes = 0
                self.reads = 0

            async def call(self, actor, name, args, deadline, **kw):
                if kw.get("write"):
                    self.writes += 1
                    return {"id": "example-job", "status": "queued", "outcome": "pending"}
                self.reads += 1
                return {
                    "id": "example-job",
                    "roomId": "r1",
                    "status": "succeeded",
                    "outcome": "full",
                }

        g = G()
        actions = Actions(g, load, save, notify, poll_interval=0.001)
        from astrbot_plugin_dst_companion.routing import Intent

        text = await actions.submit(
            actor(role="game-manage"), Intent("backup"), Target("r1"), time.monotonic() + 1
        )
        self.assertIn("已排队", text)
        await asyncio.sleep(0.02)
        self.assertEqual(g.writes, 1)
        self.assertEqual(g.reads, 1)
        self.assertEqual(len(notices), 1)
        self.assertIn("已完成", notices[0])
        self.assertEqual(next(iter(journal.values()))["state"], "terminal")
        await actions.close()

    async def test_revocation_after_read_blocks_the_result(self):
        checks = 0

        async def auth(*args):
            nonlocal checks
            checks += 1
            if checks > 2:
                raise QueryError("PERMISSION_DENIED")

        async def transport(*args):
            return mcp({"items": []})

        g = Gateway(transport, auth)
        with self.assertRaises(QueryError):
            await g.call(actor(), "api_listRooms", {}, time.monotonic() + 1)
        self.assertEqual(checks, 3)
        await g.close()


class BudgetTests(unittest.TestCase):
    def test_state_data_budget_and_eviction_do_not_reuse_sequences(self):
        state = State()
        state.maximum_bytes = 1024
        first = state.begin(("first",))
        for i in range(50):
            key = (str(i),)
            seq = state.begin(key)
            state.remember(key, seq, Target("r1", str(i)), "很长的结果" * 100)
            self.assertLessEqual(state.bytes, state.maximum_bytes)
        second = state.begin(("first",))
        self.assertGreater(second, first)
        state.remember(("first",), first, Target("r1", "wrong"), "old")
        self.assertIsNone(state.target(("first",)))


if __name__ == "__main__":
    unittest.main()
