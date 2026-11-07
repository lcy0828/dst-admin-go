"""End-to-end verification of registry discovery, slot hiding, performance, and map."""

import json
import sys
import unittest
from dataclasses import replace
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from types import SimpleNamespace as NS
from test_companion import FakeGateway, actor, mcp

from astrbot_plugin_dst_companion.capabilities import Directory
from astrbot_plugin_dst_companion.engine import Engine
from astrbot_plugin_dst_companion.jobs import Actions
from astrbot_plugin_dst_companion.routing import parse
from astrbot_plugin_dst_companion.state import State
from astrbot_plugin_dst_companion.task_tools import TaskTools, discover_item

SNAPSHOT_PATH = (
    Path(__file__).resolve().parents[3]
    / "internal"
    / "capreg"
    / "testdata"
    / "bot_catalog_snapshot.json"
)


def load_snapshot():
    return json.loads(SNAPSHOT_PATH.read_text(encoding="utf-8"))


class SnapshotGateway(FakeGateway):
    def __init__(self, items):
        super().__init__()
        self.snapshot = items
        self.executions = []

    async def call(self, actor, name, args, deadline, **kw):
        self.calls.append((name, args))
        if name == "api_listRooms":
            return {"items": [{"id": "r1", "name": "Camp"}]}
        if name == "api_listBotCapabilities":
            q = args.get("query", {})
            offset = int(q.get("cursor") or 0)
            limit = int(q.get("limit") or 32)
            page = self.snapshot[offset : offset + limit]
            next_cursor = str(offset + limit) if offset + limit < len(self.snapshot) else ""
            return {
                "protocolVersion": q.get("protocolVersion", 1),
                "revision": "snap-v1",
                "items": page,
                **({"nextCursor": next_cursor} if next_cursor else {}),
            }
        if name == "api_listRoomWorlds":
            return {
                "items": [
                    {"id": "Master", "name": "地面", "role": "master"},
                    {"id": "Caves", "name": "洞穴", "role": "caves"},
                ]
            }
        if name == "api_resolveRoomPlayer":
            q = args["query"]["query"]
            if q == "mori":
                return {
                    "status": "resolved",
                    "match": "exact_name",
                    "items": [{"id": "KU_SYNTH001", "name": "mori", "online": True}],
                }
            return {"status": "not_found", "items": []}
        if name == "api_queryBotCapability":
            cid = args["path"]["capabilityId"]
            params = json.loads(args["query"].get("parameters") or "{}")
            self.executions.append((cid, params))
            if cid == "room.performance":
                return {
                    "status": "succeeded",
                    "complete": True,
                    "capabilityId": cid,
                    "resultKind": "facts",
                    "observedAt": "2026-11-05T15:40:00Z",
                    "data": {
                        "from": "2026-11-04T15:40:00Z",
                        "to": "2026-11-05T15:40:00Z",
                        "worlds": [
                            {
                                "worldId": "Master",
                                "worldName": "Master",
                                "averageScore": 98.4,
                                "minimumScore": 91.0,
                                "coverage": 0.96,
                                "peakCPUPercent": 42.5,
                                "peakRSSBytes": 640 * 1024 * 1024,
                            }
                        ],
                        "items": [
                            {
                                "worldId": "Master",
                                "worldName": "Master",
                                "cpuPercent": 28.0,
                                "rssBytes": 512 * 1024 * 1024,
                            }
                        ],
                    },
                }
            if cid == "room.map":
                return {
                    "status": "succeeded",
                    "complete": True,
                    "capabilityId": cid,
                    "resultKind": "facts",
                    "observedAt": "2026-11-05T15:41:00Z",
                    "data": {
                        "total": 1,
                        "rendererAvailable": True,
                        "items": [
                            {
                                "id": "00000000-0000-4000-8000-000000000099",
                                "worldId": "Master",
                                "status": "succeeded",
                                "width": 1024,
                                "height": 1024,
                                "featureCount": 128,
                                "finishedAt": "2026-11-05T15:00:00Z",
                            }
                        ],
                    },
                }
        if name == "api_runBotCapability":
            cid = args["path"]["capabilityId"]
            params = args["body"].get("parameters", {})
            self.executions.append((cid, params))
            if cid == "player.give_item":
                return {
                    "status": "succeeded",
                    "complete": True,
                    "requestId": args["requestId"],
                    "roomId": args["path"]["roomId"],
                    "capabilityId": cid,
                    "resultKind": "item-grant",
                    "data": {
                        "name": params.get("item"),
                        "requested": params.get("count"),
                        "granted": params.get("count"),
                    },
                }
            return {
                "status": "succeeded",
                "complete": True,
                "requestId": args["requestId"],
                "roomId": args["path"]["roomId"],
                "capabilityId": cid,
                "resultKind": "workbench-run" if cid.startswith("workbench.") else "job",
                "jobId": "job-1",
                "data": {"output": {"text": "OK"}},
            }
        return await super().call(actor, name, args, deadline, **kw)


class RegistryDiscoveryAndJourneyTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.snapshot = load_snapshot()

    def test_discover_item_hides_internal_and_presentation_slots_across_entire_catalog(self):
        for item in self.snapshot:
            card = discover_item(item)
            props = card["parameters"].get("properties", {})
            req = card["parameters"].get("required", [])
            for field in ("feedback", "preview_id", "confirmed", "confirmation", "expected_session"):
                self.assertNotIn(field, props, item["id"])
                self.assertNotIn(field, req, item["id"])
            if item["id"] not in {"workbench.set_phase", "workbench.set_moon_phase"}:
                self.assertNotIn("phase", props, item["id"])
            else:
                self.assertIn("phase", props, item["id"])

    def test_direct_routing_covers_skip_days_reset_performance_and_map(self):
        self.assertEqual(
            parse("跳过一天", {}),
            parse("快进 1 天", {}),
        )
        self.assertEqual(parse("跳过一天", {}).capability, "workbench.skip_days")
        self.assertEqual(parse("跳过一天", {}).parameters, {"days": 1})
        self.assertEqual(parse("跳过三天", {}).parameters, {"days": 3})

        self.assertEqual(parse("重置世界", {}).capability, "room.reset")
        self.assertEqual(parse("重置世界", {}).parameters, {})
        self.assertEqual(parse("重置世界 不备份", {}).parameters, {"backup": False})

        perf = parse("查询最近一天服务器的运行状态 cpu 内存指标怎么样", {})
        self.assertIsNotNone(perf)
        self.assertEqual(perf.capability, "room.performance")
        self.assertEqual(perf.parameters, {"window": "24h"})

        for phrase in ("查看地图", "看看地图", "世界地图", "地图"):
            route = parse(phrase, {})
            self.assertIsNotNone(route, phrase)
            self.assertEqual(route.capability, "room.map")

    async def test_user_reported_journeys_execute_against_real_catalog_snapshot(self):
        gw = SnapshotGateway(self.snapshot)
        state = State()
        directory = Directory(gw)
        records = {}

        async def load():
            return dict(records)

        async def save(value):
            records.clear()
            records.update(value)

        actions = Actions(gw, load, save, directory=directory)
        self.addAsyncCleanup(actions.close)
        engine = Engine(gw, state, default_room="r1", actions=actions, directory=directory)
        all_views = frozenset(
            {"status", "players", "profile", "inventory", "network", "mods", "performance", "backups", "world"}
        )
        admin = replace(
            actor(role="game-manage"),
            visibility=all_views,
            write_allowlist=frozenset({"*"}),
            workbench_mode="all",
        )
        member = replace(
            actor(role="game-read"),
            visibility=all_views,
            write_allowlist=frozenset(),
            workbench_mode="off",
        )

        seq = 0

        async def send(who, text, **kw):
            nonlocal seq
            seq += 1
            who = replace(who, event=NS(message_obj=NS(message_id=f"msg-{seq}")))
            return await engine.handle(who, text, **kw)

        # #4: "给玩家mori 发五个草" and "给玩家mori发五个草" resolve "mori" and execute R1 without confirmation
        for text in ("给玩家mori 发五个草", "给玩家mori发五个草"):
            reply = await send(admin, text, woken=True)
            self.assertEqual(reply.outcome, "matched", f"{text}: {reply.text}")
            self.assertIn("对象：mori", reply.text)
            self.assertIn("已发放 草 ×5", reply.text)
            self.assertEqual(gw.executions[-1], ("player.give_item", {"playerId": "KU_SYNTH001", "item": "草", "count": 5}))

        # #5: "给玩家mori开启无敌模式" upgrades to player.god_mode and never asks for feedback
        reply = await send(admin, "给玩家mori开启无敌模式", woken=True)
        self.assertEqual(reply.outcome, "matched", reply.text)
        self.assertNotIn("panel_detail", reply.text)
        self.assertIn("对象：mori", reply.text)
        self.assertEqual(gw.executions[-1], ("player.god_mode", {"playerId": "KU_SYNTH001", "enabled": True}))

        # #6: "跳过一天" auto-selects Master shard, asks R2 once confirmation, then runs
        reply = await send(admin, "跳过一天", woken=True)
        self.assertEqual(reply.outcome, "needs_selection", reply.text)
        self.assertIn("回复「确认」", reply.text)
        reply = await send(admin, "确认")
        self.assertEqual(reply.outcome, "matched", reply.text)
        self.assertEqual(gw.executions[-1], ("workbench.skip_days", {"days": 1, "worldId": "Master"}))

        # #7: "重置世界" asks R3 typed confirmation, supports opting out of backup
        reply = await send(admin, "重置世界", woken=True)
        self.assertEqual(reply.outcome, "needs_selection", reply.text)
        self.assertIn("确认重置", reply.text)
        reply = await send(admin, "确认重置 不备份")
        self.assertEqual(reply.outcome, "matched", reply.text)
        self.assertEqual(gw.executions[-1], ("room.reset", {"backup": False, "confirmation": "Camp"}))

        # #8: "查询最近一天服务器的运行状态 cpu 内存指标怎么样" works for ordinary members
        reply = await send(member, "查询最近一天服务器的运行状态 cpu 内存指标怎么样", woken=True)
        self.assertEqual(reply.outcome, "matched", reply.text)
        self.assertIn("📊 性能报表", reply.text)
        self.assertIn("模拟评分 平均 98.4", reply.text)
        self.assertIn("CPU 平均 28.0% / 峰值 42.5%", reply.text)
        self.assertIn("内存 当前 512 MiB / 峰值 640 MiB", reply.text)
        self.assertEqual(gw.executions[-1], ("room.performance", {"window": "24h"}))

        # #9: "查看地图" works for ordinary members and never gets treated as a player nickname
        reply = await send(member, "查看地图", woken=True)
        self.assertEqual(reply.outcome, "matched", reply.text)
        self.assertIn("🗺️ 世界地图", reply.text)
        self.assertIn("1024×1024", reply.text)
        self.assertIn("标记 128 处", reply.text)
        self.assertEqual(gw.executions[-1], ("room.map", {}))

        # #12: AI discovery (TaskTools.find) ranks canonical capabilities first and respects negatives
        tools = TaskTools(NS(directory=directory))
        for query, want_id in (
            ("跳过一天", "workbench.skip_days"),
            ("重置世界", "room.reset"),
            ("查看地图", "room.map"),
            ("最近一天CPU内存", "room.performance"),
            ("开启无敌模式", "player.god_mode"),
            ("制作模式", "player.creative_mode"),
            ("重置角色", "player.reset_character"),
        ):
            found = json.loads(await tools.find(admin, query))
            self.assertTrue(found["items"], query)
            self.assertEqual(found["items"][0]["id"], want_id, f"{query} -> {found['items']}")

    async def test_quote_reply_text_focus_preservation_and_notice_correlation(self):
        from astrbot_plugin_dst_companion.broadcast import BroadcastEngine
        from astrbot_plugin_dst_companion.event_notices import accept_page
        from astrbot_plugin_dst_companion.state import Target
        from unittest.mock import AsyncMock
        import datetime

        gw = SnapshotGateway(self.snapshot)
        state = State()
        directory = Directory(gw)
        records = {}

        async def load():
            return dict(records)

        async def save(value):
            records.clear()
            records.update(value)

        actions = Actions(gw, load, save, directory=directory)
        self.addAsyncCleanup(actions.close)
        engine = Engine(gw, state, default_room="r1", actions=actions, directory=directory)
        all_views = frozenset(
            {"status", "players", "profile", "inventory", "network", "mods", "performance", "backups", "world"}
        )
        admin = replace(
            actor(role="game-manage"),
            visibility=all_views,
            write_allowlist=frozenset({"*"}),
            workbench_mode="all",
        )

        # #1 + #3: Quoting a death notice selects mori, and subsequent "复活" / "再次复活" / "复活玩家" reuse focus
        state.associate(admin.scope, "death-msg-1", Target("r1", "KU_SYNTH001", "mori"))
        seq = 0

        async def send(who, text, **kw):
            nonlocal seq
            seq += 1
            who = replace(
                who,
                event=NS(
                    message_obj=NS(message_id=f"revive-msg-{seq}"),
                    unified_msg_origin="aiocqhttp:GroupMessage:10001",
                ),
            )
            return await engine.handle(who, text, **kw)

        r1 = await send(admin, "复活", woken=True, quote="death-msg-1")
        self.assertEqual(r1.outcome, "matched", r1.text)
        self.assertIn("对象：mori（引用的消息）", r1.text)

        for followup in ("再次复活", "复活", "复活玩家"):
            rn = await send(admin, followup, woken=True)
            self.assertEqual(rn.outcome, "matched", f"{followup}: {rn.text}")
            self.assertIn("对象：mori（沿用你刚才操作的玩家）", rn.text)
            self.assertEqual(gw.executions[-1], ("player.resurrect", {"player_id": "KU_SYNTH001"}))

        # An execution receipt does not prove resurrection: keep the observed notice.
        self.assertFalse(
            await actions.claim_notice(
                "aiocqhttp:GroupMessage:10001",
                admin.connection_id,
                {"roomId": "r1", "kind": "players", "eventKind": "player.respawned", "playerId": "KU_SYNTH001"},
            )
        )
        # Other groups still receive the notice
        self.assertFalse(
            await actions.claim_notice(
                "aiocqhttp:GroupMessage:99999",
                admin.connection_id,
                {"roomId": "r1", "kind": "players", "eventKind": "player.respawned", "playerId": "KU_SYNTH001"},
            )
        )

        # #11b: Delayed player death notice (>=15s old) is annotated with delay duration
        now = 1800000000
        be = BroadcastEngine(
            AsyncMock(),
            AsyncMock(return_value=True),
            AsyncMock(),
            {"r1": "Camp"},
            options={"players": True},
            clock=lambda: now,
        )
        occurred = datetime.datetime.fromtimestamp(now - 25, datetime.timezone.utc).isoformat()
        await accept_page(
            be,
            "r1",
            {
                "items": [
                    {
                        "id": "evt-delay-1",
                        "roomId": "r1",
                        "category": "players",
                        "kind": "player.death",
                        "status": "observed",
                        "title": "玩家阵亡",
                        "message": "mori 被发条主教击败了",
                        "occurredAt": occurred,
                        "facts": {"playerId": "KU_SYNTH001", "playerName": "mori"},
                    }
                ],
                "nextCursor": "c2",
                "hasMore": False,
            },
        )
        self.assertEqual(len(be.state["outbox"]), 1)
        self.assertIn("延迟 25 秒送达", be.state["outbox"][0]["text"])


if __name__ == "__main__":
    unittest.main()
