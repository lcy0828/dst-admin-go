"""Readable status cards preserve identity, sampling evidence and unknown data."""

import subprocess
import sys
import tempfile
import unittest
from dataclasses import replace
from pathlib import Path
from zipfile import ZipFile

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from test_companion import FakeGateway, P, actor

from astrbot_plugin_dst_companion import formatters as fmt
from astrbot_plugin_dst_companion.display import room_heading, room_label, time_text
from astrbot_plugin_dst_companion.engine import Engine
from astrbot_plugin_dst_companion.gateway import QueryError
from astrbot_plugin_dst_companion.state import State
from package import build

WORLDS = {"items": [{"id": "m", "name": "Master"}, {"id": "c", "name": "Caves"}]}
STATES = {
    "items": [
        {
            "worldId": wid,
            "runtimeState": "running",
            "freshness": "live",
            "stale": False,
            "cycles": 385,
            "season": "spring",
            "remainingDaysInSeason": 20,
            "observedAt": "2026-10-30T19:37:" + sec + "Z",
        }
        for wid, sec in (("m", "20"), ("c", "18"))
    ]
}


class StatusGateway(FakeGateway):
    async def call(self, a, name, args, deadline, **kwargs):
        result = await super().call(a, name, args, deadline, **kwargs)
        if name == "api_listRooms":
            return {
                "items": [
                    {"id": "r1", "name": "\U000f000d示例营地「测试服」「半纯净」", "maxPlayers": 12}
                ]
            }
        if name == "api_listRoomWorlds":
            return WORLDS
        if name == "api_listWorldStates":
            return STATES
        if name == "api_listRoomPlayers":
            return {"online": 1, "total": 1, "items": [{**P, "prefab": "wendy"}]}
        if name == "api_listRoomMods":
            return {"items": [], "total": 0}
        return result


class MessageDisplayTests(unittest.IsolatedAsyncioTestCase):
    async def test_room_overview_uses_local_time_capacity_and_real_zero_without_extra_reads(self):
        gateway = StatusGateway()
        engine = Engine(gateway, State())
        result = await engine.handle(actor(), "测试服信息", woken=True)
        for value in (
            "🏕️ 测试服 · 房间概况",
            "🌍 世界状态",
            "🟢 地面 · 运行中",
            "🟢 洞穴 · 运行中",
            "第 386 天 · 🌱 春 · 剩余 20 天",
            "👥 在线 1 / 12 人",
            "温蒂 · 地面",
            "📦 模组 · 已配置 0 个",
            "🕒 最近世界采样：10/31 03:37:20（北京时间）",
        ):
            self.assertIn(value, result.text)
        self.assertNotIn("Master", result.text)
        self.assertNotIn("半纯净", result.text)
        self.assertNotIn("2026-10-30T", result.text)
        self.assertEqual(len(gateway.calls), 5)
        self.assertEqual(
            {name for name, _ in gateway.calls},
            {
                "api_listRooms",
                "api_listRoomWorlds",
                "api_listWorldStates",
                "api_listRoomPlayers",
                "api_listRoomMods",
            },
        )

    async def test_failed_mod_read_is_not_zero_and_does_not_hide_worlds_or_players(self):
        class BrokenMods(StatusGateway):
            async def call(self, a, name, *args, **kwargs):
                if name == "api_listRoomMods":
                    raise QueryError("API_FAILED")
                return await super().call(a, name, *args, **kwargs)

        result = await Engine(BrokenMods(), State()).handle(actor(), "信息", woken=True)
        self.assertIn("模组：暂未读取成功，数量未确认", result.text)
        self.assertNotIn("配置 0 个", result.text)
        self.assertIn("地面 · 运行中", result.text)
        self.assertIn("在线 1 / 12 人", result.text)

    async def test_hidden_sections_are_not_queried_or_inferred_from_the_room_card(self):
        gateway = StatusGateway()
        a = replace(actor(), visibility=frozenset({"status", "help"}))
        result = await Engine(gateway, State()).handle(a, "信息", woken=True)
        self.assertIn("世界状态", result.text)
        self.assertNotIn("模组", result.text)
        self.assertNotIn("在线", result.text)
        self.assertEqual(len(gateway.calls), 3)

    async def test_compact_display_names_resolve_only_unique_authorized_rooms(self):
        class Duplicates(StatusGateway):
            async def call(self, a, name, *args, **kwargs):
                if name == "api_listRooms":
                    return {
                        "items": [
                            {"id": "r1", "name": "甲营地「测试服」"},
                            {"id": "r2", "name": "乙营地「测试服」"},
                        ]
                    }
                return await super().call(a, name, *args, **kwargs)

        gateway = Duplicates()
        a = actor(rooms=("r1", "r2"))
        result = await Engine(gateway, State()).handle(a, "测试服信息", woken=True)
        self.assertIn("没有唯一匹配", result.text)
        self.assertEqual(gateway.calls, [])
        result = await Engine(StatusGateway(), State()).handle(
            actor(rooms=("r2",)), "测试服信息", woken=True
        )
        self.assertIn("没有唯一匹配", result.text)

    def test_stale_and_paused_worlds_cannot_appear_fresh_and_running(self):
        import copy

        states = copy.deepcopy(STATES)
        states["items"][1].update(freshness="delayed", stale=True, cycles=999)
        result = fmt.status(WORLDS, states)
        self.assertIn("🟡 洞穴 · 运行中 · 采集待同步", result)
        self.assertNotIn("第 1000 天", result)
        states["items"][1].update(freshness="paused", paused=True, stale=False)
        result = fmt.status(WORLDS, states)
        self.assertIn("⏸️ 洞穴 · 已暂停", result)
        self.assertNotIn("🟢 洞穴", result)
        self.assertNotIn("采集待同步", result)
        states["items"][1].update(runtimeState="stopped", freshness="stopped")
        result = fmt.status(WORLDS, states)
        self.assertIn("⚫ 洞穴 · 已停止", result)
        self.assertNotIn("第 1000 天", result)

    def test_mod_total_unknown_partial_and_enabled_counts_remain_distinct(self):
        for data in ({}, {"items": None}, {"items": []}, {"items": [], "total": True}):
            self.assertNotIn("配置 0 个", fmt.mods(data))
        self.assertIn("配置 0 个", fmt.mods({"items": [], "total": 0}))
        full = {"total": 2, "items": [{"id": "a", "enabled": True}, {"id": "b", "enabled": False}]}
        self.assertIn("启用 1 个 · 停用 1 个", fmt.mods(full))
        partial = {"total": 8, "items": full["items"][:1]}
        self.assertIn("配置 8 个", fmt.mods(partial))
        self.assertNotIn("停用 7 个", fmt.mods(partial))
        self.assertIn("部分模组", fmt.mods(partial))

    def test_labels_keep_identity_and_zero_values_are_not_unnamed(self):
        rooms = [
            {"id": "same-prefix-a", "name": "甲「测试服」"},
            {"id": "same-prefix-b", "name": "乙「测试服」"},
        ]
        a, _ = room_heading(rooms[0], rooms)
        b, _ = room_heading(rooms[1], rooms)
        self.assertNotEqual(a, b)
        self.assertIn("甲", a)
        self.assertIn("乙", b)
        self.assertEqual(room_label("\U000f000d"), "未命名房间")
        self.assertEqual(fmt.safe(0), "0")
        self.assertIn("［CQ:", fmt.safe("[CQ:at,qq=all]"))

    def test_times_require_timezone_and_handle_day_rollover_and_offsets(self):
        self.assertEqual(time_text("2026-10-30T19:37:20Z", year=True), "2026-10-31 03:37:20")
        self.assertEqual(time_text("2026-10-31T03:37:20+08:00"), "10/31 03:37:20")
        for value in (None, "bad", "2026-10-31T03:37:20", "0001-01-01T00:00:00Z"):
            self.assertEqual(time_text(value), "时间未确认")

    def test_sample_time_ignores_unrelated_worlds(self):
        value = {
            "items": [*STATES["items"], {"worldId": "other", "observedAt": "2030-01-01T00:00:00Z"}]
        }
        self.assertNotIn("2030", fmt.sample_time(WORLDS, value))
        self.assertIn("03:37:20", fmt.sample_time(WORLDS, value))
        self.assertIn("未确认", fmt.sample_time(WORLDS, {"items": []}))

    def test_legacy_broadcast_zip_can_import_shared_display_without_companion_installed(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            archive = build(root, "broadcast")
            target = root / "isolated_broadcast"
            with ZipFile(archive) as package:
                self.assertIn("display.py", package.namelist())
                package.extractall(target)
            script = "import sys; sys.path.insert(0, sys.argv[1]); import isolated_broadcast.broadcast as b; assert b.chat_label('营地「测试服」')=='测试服'"
            subprocess.run(
                [sys.executable, "-I", "-c", script, str(root)], check=True, capture_output=True
            )
