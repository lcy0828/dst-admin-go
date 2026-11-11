"""Notification -> real adapter receipt -> reload -> reply -> saved inventory."""

import asyncio
import copy
import importlib.util
import sys
import time
import unittest
from dataclasses import replace
from datetime import datetime, timezone
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock

sys.path.insert(0, str(Path(__file__).resolve().parent))
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

import test_astrbot_adapter as adapters
import test_references as reference_tests
from test_companion import FakeGateway, actor
from astrbot_plugin_dst_companion.broadcast import BroadcastEngine
from astrbot_plugin_dst_companion.display import REFERENCE_TTL
from astrbot_plugin_dst_companion.engine import Engine
from astrbot_plugin_dst_companion.event_notices import accept_page
from astrbot_plugin_dst_companion.formatters import inventory
from astrbot_plugin_dst_companion.gateway import QueryError
from astrbot_plugin_dst_companion.notifications import Notifications
from astrbot_plugin_dst_companion.references import References, legacy_notice_identity
from astrbot_plugin_dst_companion.routing import Intent
from astrbot_plugin_dst_companion.state import State


class SaveReads(FakeGateway):
    def __init__(self):
        super().__init__()
        self.resolution = {"status": "ambiguous", "match": "substring", "total": 1,
                           "coverage": "current_directory", "hasMore": False,
                           "items": [{"id": "KU_SYNTH001", "name": "示例玩家完整昵称", "online": False}]}
        self.sources = [
            {"sourceId": "a" * 64, "worldId": "Master", "worldName": "Master", "fileModifiedAt": "2026-01-02T00:00:00Z"},
            {"sourceId": "b" * 64, "worldId": "Caves", "worldName": "Caves", "fileModifiedAt": "2026-01-01T00:00:00Z"},
        ]
        self.explicit_error = None

    async def call(self, who, name, args, deadline, **kw):
        assert not kw.get("write"), "Read paths must not mutate the game"
        if name == "api_resolveRoomPlayer":
            self.calls.append((name, copy.deepcopy(args)))
            result = copy.deepcopy(self.resolution)
            if args["query"]["query"] == "KU_SYNTH001":
                result.update(status="resolved", match="exact_id")
            return result
        if name == "api_getRoomPlayerInventory":
            self.calls.append((name, copy.deepcopy(args)))
            if "sourceId" not in args["query"]:
                raise QueryError("SOURCE_AMBIGUOUS", details={"candidates": copy.deepcopy(self.sources)})
            if self.explicit_error:
                raise QueryError(self.explicit_error)
            return {"source": "save", "status": "complete", "worldName": args["query"]["worldId"],
                    "fileModifiedAt": next(s["fileModifiedAt"] for s in self.sources if s["sourceId"] == args["query"]["sourceId"]),
                    "inventory": [{"prefab": "cutgrass", "count": 5}]}
        return await super().call(who, name, args, deadline, **kw)


class InventoryInteractionTests(unittest.IsolatedAsyncioTestCase):
    async def test_unique_nickname_returns_latest_save_first_and_other_user_cannot_switch(self):
        gateway, state, who = SaveReads(), State(), actor()
        engine = Engine(gateway, state)
        reply = await engine.handle(who, "查询示例背包", woken=True)
        self.assertEqual(reply.outcome, "matched", reply.text)
        for expected in ("草 ×5", "地面", "最近存档", "查看其他背包存档", "不代表下线时"):
            self.assertIn(expected, reply.text)
        self.assertNotIn("请确认玩家", reply.text)
        self.assertTrue(reply.choice)
        self.assertEqual(state.target(who.key).player, "KU_SYNTH001")
        self.assertEqual((await engine.handle(actor("another-user"), "2")).outcome, "pass")
        reply = await engine.handle(who, "2")
        self.assertEqual(reply.outcome, "matched", reply.text)
        self.assertIn("洞穴", reply.text)
        reads = [a for n, a in gateway.calls if n == "api_getRoomPlayerInventory"]
        self.assertEqual(len(reads), 3)  # auto, exact latest, explicit other
        self.assertEqual(reads[-1]["query"], {"source": "save", "refresh": False, "worldId": "Caves", "sourceId": "b" * 64})
        self.assertTrue(all(a["path"]["playerId"] == "KU_SYNTH001" for a in reads))

    async def test_nonunique_or_incomplete_directory_still_asks_for_player(self):
        for change in ({"total": 2, "hasMore": True}, {"coverage": "partial"}, {"_aiTruncated": True}, {"total": 0}):
            with self.subTest(change=change):
                gateway = SaveReads()
                gateway.resolution.update(change)
                reply = await Engine(gateway, State()).handle(actor(), "查询示例背包", woken=True)
                self.assertEqual(reply.outcome, "needs_selection", reply.text)
                self.assertFalse(any(n == "api_getRoomPlayerInventory" for n, _ in gateway.calls))

    async def test_equal_missing_future_times_or_multiple_sessions_require_source_selection(self):
        for scenario in ("equal", "missing", "future", "same_world"):
            with self.subTest(scenario=scenario):
                gateway = SaveReads()
                if scenario == "equal": gateway.sources[1]["fileModifiedAt"] = gateway.sources[0]["fileModifiedAt"]
                elif scenario == "missing": gateway.sources[0].pop("fileModifiedAt")
                elif scenario == "future": gateway.sources[0]["fileModifiedAt"] = "2099-01-01T00:00:00Z"
                else: gateway.sources[1]["worldId"] = "Master"
                reply = await Engine(gateway, State()).handle(actor(), "查询示例背包", woken=True)
                self.assertEqual(reply.outcome, "needs_selection", reply.text)
                self.assertIn("无法确认哪份较新", reply.text)
                self.assertEqual(sum(n == "api_getRoomPlayerInventory" for n, _ in gateway.calls), 1)

    async def test_latest_save_failure_is_never_replaced_with_another_source(self):
        for code in ("SAVE_CHANGED", "SAVE_INVALID", "RESULT_UNCERTAIN", "SOURCE_AMBIGUOUS"):
            with self.subTest(code=code):
                gateway = SaveReads()
                gateway.explicit_error = code
                reply = await Engine(gateway, State()).handle(actor(), "查询示例背包", woken=True)
                self.assertEqual(reply.outcome, "handled_error", reply.text)
                reads = [a for n, a in gateway.calls if n == "api_getRoomPlayerInventory"]
                self.assertEqual(len(reads), 2)
                self.assertEqual(reads[-1]["query"]["sourceId"], "a" * 64)

    def test_saved_amounts_are_not_percentages_or_unlabelled_energy_values(self):
        text = inventory({"source": "save", "status": "complete", "inventory": [
            {"prefab": "goldenaxe", "metrics": {"uses": {"value": 87.25, "unit": "uses"}}},
            {"prefab": "umbrella", "metrics": {"fuel": {"value": 2483, "unit": "fuel"}}},
            {"prefab": "lantern", "metrics": {"fuel": {"value": 0, "ratio": 0}}},
        ]})
        self.assertIn("剩余约 87 次", text)
        self.assertIn("燃料 0%", text)
        self.assertNotIn("2483", text)
        self.assertNotIn("87.25", text)
        self.assertNotIn("完整", text)
        self.assertIn("未显示百分比", text)


@unittest.skipUnless(importlib.util.find_spec("astrbot"), "requires AstrBot adapters")
class QuoteFlowTests(unittest.IsolatedAsyncioTestCase):
    async def test_historical_death_respawn_and_boss_death_share_all_player_query_routes(self):
        for title, body in (("玩家死亡", "示例玩家已死亡。"),
                            ("玩家复活", "示例玩家已从幽灵状态复活。"),
                            ("玩家死亡", "示例玩家被龙蝇击败。")):
            with self.subTest(title=title, body=body):
                fixture = reference_tests.ReferenceTests()
                references = fixture.fixture()
                text = f"📣 示例房间 · {title}\n━━━━━━━━━━━━\n已观测\n地面 · {body}\n🕒 01/01 03:58:24（延迟 15 秒送达）"
                fixture.reply.message_str = "QQ 截短的引用预览…"
                fixture.original.update(time=time.time()-38*3600, message=[{"type": "text", "data": {"text": text}}])
                saved = fixture.plugin.notifications.saved["adapter:GroupMessage:789"]["state"]
                saved["deliveries"] = [{"key": "notice/activity:synthetic-event", "status": "sent", "at": fixture.original["time"]-0.5}]
                saved["rooms"] = {"r1": {"notice_seen": ["activity:synthetic-event"]}}
                for intent in (Intent("activity"), Intent("inventory"), Intent("trajectory"), Intent("combat"),
                               Intent("capability", capability="player.resurrect")):
                    target = await references.resolve(fixture.actor, "42", intent)
                    self.assertEqual(target.player, "示例玩家")
                class HistoryReads(SaveReads):
                    async def call(self, who, name, args, deadline, **kw):
                        if name == "api_listRoomPlayerActivity":
                            self.calls.append((name, copy.deepcopy(args)))
                            return {"items": [], "coverage": []}
                        return await super().call(who, name, args, deadline, **kw)
                gateway = HistoryReads()
                who = replace(actor(), event=fixture.actor.event, scope=fixture.actor.scope,
                              visibility=actor().visibility | {"activity"})
                result = await Engine(gateway, State()).handle(who, "查询玩家活动", woken=True, quote="42", reference=target)
                self.assertEqual(result.outcome, "matched", result.text)
                query = next(a["query"] for n, a in gateway.calls if n == "api_listRoomPlayerActivity")
                self.assertEqual(query["userId"], "KU_SYNTH001")

    async def test_historical_player_event_needs_original_sender_and_its_room_event_journal(self):
        for scenario in ("wrong_group", "wrong_bot", "no_event_journal", "wrong_room_event", "unrelated_delivery", "unknown_delivery", "expired", "anonymous", "copied_text"):
            with self.subTest(scenario=scenario):
                fixture = reference_tests.ReferenceTests()
                references = fixture.fixture()
                name = "一位玩家" if scenario == "anonymous" else "示例玩家"
                text = f"📣 示例房间 · 玩家死亡\n━━━━━━━━━━━━\n已观测\n地面 · {name}已死亡。\n🕒 01/01 03:58:24"
                fixture.reply.message_str = text
                fixture.original.update(time=time.time()-38*3600, message=[{"type": "text", "data": {"text": text}}])
                saved = fixture.plugin.notifications.saved["adapter:GroupMessage:789"]["state"]
                saved["deliveries"] = [{"key": "notice/activity:synthetic-event", "status": "sent", "at": fixture.original["time"]}]
                saved["rooms"] = {"r1": {"notice_seen": ["activity:synthetic-event"]}}
                if scenario == "wrong_group": fixture.original["group_id"] = 999
                elif scenario == "wrong_bot": fixture.original["sender"]["user_id"] = 999
                elif scenario == "no_event_journal": saved.pop("rooms")
                elif scenario == "wrong_room_event": saved["rooms"] = {"other-room": {"notice_seen": ["activity:synthetic-event"]}}
                elif scenario == "unrelated_delivery": saved["deliveries"][0]["key"] = "notice/another-event"
                elif scenario == "unknown_delivery": saved["deliveries"][0]["status"] = "unknown"
                elif scenario == "expired": fixture.original["time"] = time.time()-REFERENCE_TTL-1
                elif scenario == "copied_text": fixture.actor.event.get_messages = lambda: []
                self.assertIsNone(await references.resolve(fixture.actor, "42", Intent("activity")))

    async def test_event_feed_production_sender_and_reload_preserve_death_and_respawn_identity(self):
        from astrbot.api import message_components as C

        for kind, title, suffix in (("player.death", "玩家死亡", "已死亡。"),
                                    ("player.respawned", "玩家复活", "已从幽灵状态复活。")):
            with self.subTest(kind=kind):
                fixture = adapters.AdapterTests()
                bot = fixture.companion()
                self.addAsyncCleanup(bot.terminate)
                session, source = "adapter:GroupMessage:789", bot.connections.source()
                bot.config["allowed_sessions"] = [session]
                storage = {}
                bot.put_kv_data = AsyncMock(side_effect=lambda key, val: storage.update({key: copy.deepcopy(val)}))
                bot.get_kv_data = AsyncMock(side_effect=lambda key, default: copy.deepcopy(storage.get(key, default)))
                client = NS(call_action=AsyncMock(side_effect=[{"user_id": 456}, {"message_id": 42}]))
                bot.context.get_platform_inst = lambda _: NS(get_client=lambda: client)
                engine = BroadcastEngine(AsyncMock(), None, lambda state: bot.notifications.save(session, source, state),
                    {"r1": "示例房间"}, options={"players": True}, emit_notice=lambda text, related: bot.send_notice(session, text, related))
                event = {"id": "activity:synthetic-event", "roomId": "r1", "category": "players", "kind": kind,
                         "status": "observed", "title": title, "message": "示例玩家"+suffix,
                         "occurredAt": datetime.now(timezone.utc).isoformat(),
                         "facts": {"playerId": "KU_SYNTH001", "playerName": "示例玩家", "worldId": "world"}}
                await accept_page(engine, "r1", {"items": [event], "nextCursor": "next", "hasMore": False})
                await engine.deliver_one()
                self.assertEqual(client.call_action.await_count, 2)
                bot.state = State()
                bot.notifications = Notifications(bot)
                await bot.notifications.load()
                message = fixture.event([C.Reply(id="42", sender_id="456", message_str="任意已编辑模板"), C.Plain("查询玩家活动")])
                message.unified_msg_origin = session
                who = replace(actor(), scope=("adapter", "456", session), event=message)
                target = await References(bot).resolve(who, "42", Intent("activity"))
                self.assertEqual(target.player, "KU_SYNTH001")
                message.bot.call_action.assert_not_awaited()

    def test_legacy_event_templates_do_not_accept_partial_or_unrelated_messages(self):
        for body in ("未确认玩家", "示例玩家可能死亡。", "示例玩家已死亡。\n另一个玩家也死了"):
            text = f"📣 示例房间 · 玩家死亡\n━━━━━━━━━━━━\n已观测\n地面 · {body}\n🕒 01/01 03:58:24"
            self.assertIsNone(legacy_notice_identity(text))

    async def test_current_presence_production_sender_and_durable_receipt_survive_reload(self):
        from astrbot.api import message_components as C

        fixture = adapters.AdapterTests()
        bot = fixture.companion()
        self.addAsyncCleanup(bot.terminate)
        session = "adapter:GroupMessage:789"
        bot.config["allowed_sessions"] = [session, "adapter:GroupMessage:123_789"]
        source = bot.connections.source()
        storage = {}
        bot.put_kv_data = AsyncMock(side_effect=lambda key, val: storage.update({key: copy.deepcopy(val)}))
        bot.get_kv_data = AsyncMock(side_effect=lambda key, default: copy.deepcopy(storage.get(key, default)))
        client = NS(call_action=AsyncMock(side_effect=[{"user_id": 456}, {"message_id": 42}]))
        bot.context.get_platform_inst = lambda _: NS(get_client=lambda: client)
        engine = BroadcastEngine(AsyncMock(return_value={"id": "KU_SYNTH001", "name": "示例玩家完整昵称", "prefab": "walter", "online": False}),
            None, lambda state: bot.notifications.save(session, source, state), {"r1": "示例房间"},
            emit_notice=lambda text, related: bot.send_notice(session, text, related))
        engine.stats = AsyncMock(return_value={"online": 0, "max_players": 12})
        engine.room("r1")["pending"]["KU_SYNTH001"] = engine.pending_presence("r1/world/left", "player.left", "world", time.time(), 0)
        await engine.resolve_presence()
        await engine.deliver_one()
        await engine.deliver_one()
        self.assertEqual(client.call_action.await_count, 2)  # login and one send
        sent_text = client.call_action.await_args.kwargs["message"][0]["data"]["text"]
        self.assertTrue(sent_text.startswith("👋 玩家离开"))
        self.assertEqual(storage["broadcast_state_v2"][session]["state"]["references"][0]["related"]["playerId"], "KU_SYNTH001")
        bot.state = State()
        bot.notifications = Notifications(bot)
        await bot.notifications.load()
        event = fixture.event([C.Reply(id="42", sender_id="456", message_str=sent_text), C.At(qq="456"), C.Plain("查询背包")])
        event.unified_msg_origin = "adapter:GroupMessage:123_789"
        who = replace(actor(), scope=("adapter", "456", event.unified_msg_origin), event=event)
        text, woken, quote, error = bot.input(event)
        self.assertFalse(error)
        self.assertTrue(woken)
        reference = await References(bot).resolve(who, quote, Intent("inventory"))
        self.assertEqual(reference.player, "KU_SYNTH001")
        event.bot.call_action.assert_not_awaited()  # receipt path needs no get_msg
        gateway = SaveReads()
        reply = await Engine(gateway, bot.state).handle(who, text, woken=woken, quote=quote, reference=reference)
        self.assertEqual(reply.outcome, "matched", reply.text)
        self.assertIn("草 ×5", reply.text)
        self.assertTrue(all(a["path"]["playerId"] == "KU_SYNTH001" for n, a in gateway.calls if n == "api_getRoomPlayerInventory"))
        self.assertIsNotNone(bot.actor(event))
        await bot.send_reply(event, reply.text, reply.target)
        refs = storage["broadcast_state_v2"][session]["state"]["references"]
        self.assertEqual(refs[-1]["messageId"], "987")
        self.assertEqual(refs[-1]["related"]["playerId"], "KU_SYNTH001")

    async def test_37_hour_old_current_notice_recovers_only_after_original_and_delivery_checks(self):
        for scenario in ("valid", "wrong_group", "wrong_bot", "no_delivery", "other_room", "expired"):
            with self.subTest(scenario=scenario):
                fixture = reference_tests.ReferenceTests()
                references = fixture.fixture()
                # Exact production template, shortened room label, old receipt
                # already pruned by the previous 24-hour implementation.
                fixture.plugin.engine.queries.rooms.return_value = [{"id": "r1", "name": "测试营地「示例服」「长期」"}]
                text = "👋 玩家离开 · 示例服\n━━━━━━━━━━━━\n👤 示例玩家 · 沃尔特 · 地面\n角色年龄：30 天\n在线 0/12 人"
                fixture.reply.message_str = text
                fixture.original.update(time=time.time()-37*3600, message=[{"type": "text", "data": {"text": text}}])
                saved = fixture.plugin.notifications.saved["adapter:GroupMessage:789"]["state"]
                saved["deliveries"][0]["at"] = fixture.original["time"]-0.5
                if scenario == "wrong_group": fixture.original["group_id"] = 999
                elif scenario == "wrong_bot": fixture.original["sender"]["user_id"] = 999
                elif scenario == "no_delivery": saved["deliveries"] = []
                elif scenario == "other_room": fixture.actor.rooms = ("other",)
                elif scenario == "expired": fixture.original["time"] = time.time()-REFERENCE_TTL-1
                result = await references.resolve(fixture.actor, "42", Intent("inventory"))
                if scenario == "valid":
                    self.assertEqual(result.player, "示例玩家")  # Engine resolves nickname, not an invented KU
                else:
                    self.assertIsNone(result)

    async def test_query_reply_reference_is_not_overwritten_by_next_notification_save(self):
        from astrbot_plugin_dst_companion.state import Target

        fixture = adapters.AdapterTests()
        bot = fixture.companion()
        self.addAsyncCleanup(bot.terminate)
        session, source, storage = "adapter:GroupMessage:789", bot.connections.source(), {}
        bot.put_kv_data = AsyncMock(side_effect=lambda key, val: storage.update({key: copy.deepcopy(val)}))
        engine = BroadcastEngine(AsyncMock(), None, lambda state: bot.notifications.save(session, source, state), {"r1": "示例房间"})
        bot.notifications.engines[session] = engine
        bot.notifications.source = bot.connection_identity()
        row = {"messageId": "99", "selfId": "456", "at": time.time()-37*3600,
               "related": {"roomId": "r1", "playerId": "KU_SYNTH001"}}
        await bot.notifications.remember_reference(session, source, row)
        await engine.persist()
        self.assertEqual(storage["broadcast_state_v2"][session]["state"]["references"], [row])
        async with bot.notifications.lock:
            # A settings refresh replaces an engine while a query reply is
            # being recorded. The replacement must retain that reply too.
            recording = asyncio.create_task(bot.notifications.remember_reference(session, source, {**row, "messageId": "100"}))
            await asyncio.sleep(0)
            self.assertFalse(recording.done())
            replacement = BroadcastEngine(AsyncMock(), None, engine.save, engine.rooms, state=engine.state)
            bot.notifications.engines[session] = replacement
        await recording
        await replacement.persist()
        refs = storage["broadcast_state_v2"][session]["state"]["references"]
        self.assertEqual([r["messageId"] for r in refs], ["99", "100"])
        bot.state.associate(("adapter", "456", session), "99", Target("r1", "KU_SYNTH001"), source=source)
        bot.state.clock = lambda: time.monotonic() + 37*3600
        self.assertIsNotNone(bot.state.quoted(("adapter", "456", session), "99", source=source))
