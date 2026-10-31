"""Question corpus and contextual acceptance through the installed QQ pipeline."""

import importlib.util
import sys
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from query_scenarios import AI_CASES, SHORTCUT_SCENARIOS, WRITE_CASES

from astrbot_plugin_dst_companion.commands import COMMANDS
from astrbot_plugin_dst_companion.gateway import QueryError
from astrbot_plugin_dst_companion.routing import parse
from astrbot_plugin_dst_companion.state import State

AVAILABLE = importlib.util.find_spec("astrbot") is not None
if AVAILABLE:
    import test_qq_pipeline as pipeline
    from astrbot.api import message_components as C


class CorpusTests(unittest.TestCase):
    def test_all_rendered_menu_labels_are_parsable_from_the_same_registry(self):
        for command in COMMANDS:
            with self.subTest(label=command.label):
                result = parse(command.label, {"1服": "r1"})
                self.assertIsNotNone(result)
                self.assertEqual(result.kind, command.kind)

    def test_read_corpus_politeness_and_suffixes_keep_targets(self):
        for message, kind, player in SHORTCUT_SCENARIOS:
            with self.subTest(message=message):
                result = parse(message, {"1服": "r1", "2服": "r2"})
                self.assertIsNotNone(result)
                self.assertEqual(result.kind, kind)
                self.assertEqual(result.player, player)

    def test_analysis_discussion_and_read_verbs_do_not_execute_writes(self):
        for message in AI_CASES:
            with self.subTest(message=message):
                self.assertIsNone(parse(message, {"1服": "r1"}))
        for verb in ("查询", "查看", "查一下", "看一下", "查下"):
            for action in ("重启1服", "停止1服", "启动1服", "给 示例玩家 三个肉丸"):
                with self.subTest(message=verb + action):
                    self.assertIsNone(parse(verb + action, {"1服": "r1"}))

    def test_chinese_quantities_are_still_explicit_bounded_write_intents(self):
        for message, capability, count in WRITE_CASES:
            with self.subTest(message=message):
                result = parse(message, {"1服": "r1"})
                self.assertEqual(result.capability, capability)
                self.assertEqual(result.parameters["count"], count)
                self.assertEqual(result.parameters["playerId"], "示例玩家")

    def test_room_scope_is_never_lost_by_read_prefixes_or_all_room_phrases(self):
        for prefix in ("", "查询", "查看", "查一下", "看一下"):
            for room, expected in (("1服", "r1"), ("一服", "r1"), ("2服", "r2")):
                for phrase in ("房间状态", "玩家信息", "在线玩家", "模组列表", "网络档位"):
                    with self.subTest(text=prefix + room + phrase):
                        result = parse(prefix + room + phrase, {"1服": "r1", "2服": "r2"})
                        self.assertIsNotNone(result)
                        self.assertEqual(result.room, expected)
                        self.assertFalse(result.all_rooms)
        result = parse("1服所有服状态", {"1服": "r1"})
        self.assertEqual(result.room, "r1")
        self.assertFalse(result.all_rooms)


@unittest.skipUnless(AVAILABLE, "requires AstrBot 4.29")
class QuestionPipelineTests(unittest.IsolatedAsyncioTestCase):
    if AVAILABLE:
        asyncSetUp = pipeline.QQPipelineTests.asyncSetUp
        asyncTearDown = pipeline.QQPipelineTests.asyncTearDown
        event = pipeline.QQPipelineTests.event
        dispatch = pipeline.QQPipelineTests.dispatch
        automatic_setup = pipeline.QQPipelineTests.automatic_setup

    def reset_dialog(self):
        self.bot.state = State()
        self.bot.engine.state = self.bot.state
        self.models.clear()
        self.other_handlers.clear()
        self.panel.calls.clear()

    async def test_every_read_question_reaches_the_expected_recipe_without_ai(self):
        self.bot.config.update(
            room_ids=["r1", "r2"], default_room_id="r1", room_aliases={"1服": "r1", "2服": "r2"}
        )
        for message, kind, player in SHORTCUT_SCENARIOS:
            self.reset_dialog()
            with self.subTest(message=message):
                event = self.event([C.At(qq="456"), C.Plain(message)])
                output = await self.dispatch(event)
                self.assertTrue(event.is_stopped())
                self.assertTrue(output)
                self.assertEqual(self.models, [])
                self.assertEqual(self.other_handlers, [])
                names = {name for name, _ in self.panel.calls}
                expected = {
                    "status": "api_listWorldStates",
                    "players": "api_listRoomPlayers",
                    "network": "api_listRoomPlayers",
                    "mods": "api_listRoomMods",
                    "rooms": "api_listRooms",
                }.get(kind)
                if expected and not (kind == "network" and player):
                    self.assertIn(expected, names)
                if kind == "network" and player:
                    self.assertIn("api_resolveRoomPlayer", names)
                    self.assertNotIn("api_listRoomPlayers", names)
                if kind in {"profile", "inventory"}:
                    if player:
                        resolved = [
                            args
                            for name, args in self.panel.calls
                            if name == "api_resolveRoomPlayer"
                        ]
                        self.assertEqual(resolved[0]["query"]["query"], player)
                        if kind == "inventory":
                            self.assertIn("api_getRoomPlayerInventory", names)
                    else:
                        self.assertIn("玩家昵称", output[-1])
                        self.assertNotIn("api_listRoomPlayers", names)
                self.assertFalse(
                    any(
                        name
                        in {
                            "api_runRoomAction",
                            "api_runBotCapability",
                            "api_createRoomBackup",
                            "api_kickRoomPlayer",
                        }
                        for name in names
                    )
                )

    async def test_reported_double_at_and_plain_at_menu_shapes_are_deterministic(self):
        shapes = (
            [C.Plain("@"), C.At(qq="456"), C.Plain(" 菜单")],
            [C.Plain("@@ "), C.At(qq="456"), C.Plain("菜单")],
            [C.At(qq="456"), C.Plain("@"), C.Plain(" 菜单")],
            [C.Plain("菜单 "), C.At(qq="456")],
            [C.At(qq="456"), C.At(qq="456"), C.Plain("菜单")],
            [C.At(qq="456"), C.Plain("菜单")],
        )
        for parts in shapes:
            self.reset_dialog()
            with self.subTest(parts=[type(p).__name__ for p in parts]):
                output = await self.dispatch(self.event(parts))
                self.assertEqual(sum("可用查询与管理" in text for text in output), 1)
                self.assertEqual(self.models, [])
                self.assertEqual(self.other_handlers, [])

    async def test_plain_at_text_and_other_bot_do_not_gain_our_scope(self):
        for parts in ([C.Plain("@456 菜单")], [C.Plain("@"), C.At(qq="999"), C.Plain("菜单")]):
            self.reset_dialog()
            await self.dispatch(self.event(parts))
            self.assertEqual(self.panel.calls, [])

    async def test_model_analysis_questions_pass_without_panel_side_effects(self):
        for message in AI_CASES:
            self.reset_dialog()
            with self.subTest(message=message):
                event = self.event([C.At(qq="456"), C.Plain(message)])
                await self.dispatch(event)
                self.assertEqual(self.models, [event])
                self.assertEqual(self.panel.calls, [])

    async def test_readonly_user_cannot_run_any_visible_management_label(self):
        for command in COMMANDS:
            if not command.management:
                continue
            self.reset_dialog()
            with self.subTest(command=command.label):
                event = self.event([C.At(qq="456"), C.Plain(command.label)], user="999")
                output = await self.dispatch(event)
                self.assertTrue(event.is_stopped())
                self.assertTrue(any("未开放" in text for text in output))
                self.assertEqual(self.models, [])
                self.assertEqual(self.panel.calls, [])

    async def test_historical_player_query_does_not_depend_on_current_online_count(self):
        self.panel.candidates = [
            {**self.panel.candidates[0], "online": False, "presenceStatus": "live"}
        ]
        output = await self.dispatch(self.event([C.At(qq="456"), C.Plain("玩家信息 示例玩家")]))
        self.assertTrue(any("离线" in text for text in output))
        self.assertTrue(any(name == "api_resolveRoomPlayer" for name, _ in self.panel.calls))
        self.assertFalse(any(name == "api_listRoomPlayers" for name, _ in self.panel.calls))
        self.assertEqual(self.models, [])

    async def test_reported_menu_profile_status_sequence_never_reaches_model(self):
        for parts in (
            [C.Plain("@"), C.At(qq="456"), C.Plain(" 菜单")],
            [C.At(qq="456"), C.Plain("菜单")],
            [C.At(qq="456"), C.Plain("玩家信息")],
            [C.At(qq="456"), C.Plain("房间状态")],
        ):
            output = await self.dispatch(self.event(parts))
            self.assertTrue(output)
        self.assertEqual(self.models, [])
        self.assertEqual(self.other_handlers, [])

    async def test_at_sign_in_player_name_is_never_removed(self):
        for parts in (
            [C.At(qq="456"), C.Plain("查背包 @示例玩家")],
            [C.Plain("@"), C.At(qq="456"), C.Plain("查背包 @示例玩家")],
        ):
            self.reset_dialog()
            await self.dispatch(self.event(parts))
            resolved = [args for name, args in self.panel.calls if name == "api_resolveRoomPlayer"]
            self.assertEqual(resolved[0]["query"]["query"], "@示例玩家")

    async def test_menu_inventory_then_name_completes_the_same_read_without_ai(self):
        await self.dispatch(self.event([C.At(qq="456"), C.Plain("菜单")]))
        prompt = await self.dispatch(self.event([C.Plain("5")]))
        self.assertIn("玩家昵称", prompt[-1])
        result = await self.dispatch(self.event([C.At(qq="456"), C.Plain("示例玩家")]))
        self.assertIn("背包", result[-1])
        self.assertIn("存档快照", result[-1])
        self.assertIn("树枝", result[-1])
        self.assertEqual(self.models, [])

    async def test_room_choice_and_explicit_new_scope_never_reuse_the_wrong_room(self):
        self.bot.config.update(room_ids=["r1", "r2"], room_aliases={"1服": "r1", "2服": "r2"})
        prompt = await self.dispatch(self.event([C.At(qq="456"), C.Plain("在线玩家")]))
        self.assertIn("请选择房间", prompt[-1])
        await self.dispatch(self.event([C.Plain("2")]))
        rooms = [
            args["path"]["roomId"]
            for name, args in self.panel.calls
            if name == "api_listRoomPlayers"
        ]
        self.assertEqual(rooms, ["r2"])
        await self.dispatch(self.event([C.At(qq="456"), C.Plain("1服查询玩家信息 示例玩家")]))
        resolved = [args for name, args in self.panel.calls if name == "api_resolveRoomPlayer"]
        self.assertEqual(resolved[-1]["path"]["roomId"], "r1")
        self.assertEqual(resolved[-1]["query"]["query"], "示例玩家")
        self.assertEqual(self.models, [])

    async def test_profile_prompt_binds_only_the_requesting_user(self):
        await self.dispatch(self.event([C.At(qq="456"), C.Plain("玩家信息")]))
        await self.dispatch(self.event([C.Plain("示例玩家")], user="999"))
        self.assertFalse(any(name == "api_resolveRoomPlayer" for name, _ in self.panel.calls))
        await self.dispatch(self.event([C.At(qq="456"), C.Plain("示例玩家")]))
        resolved = [args for name, args in self.panel.calls if name == "api_resolveRoomPlayer"]
        self.assertEqual(len(resolved), 1)
        self.assertEqual(resolved[0]["query"]["query"], "示例玩家")
        self.assertEqual(self.models, [])

    async def test_core_read_timeout_retains_its_reason_and_never_falls_through(self):
        original = self.panel.call

        async def broken(a, name, *args, **kwargs):
            if name == "api_listWorldStates":
                raise QueryError("QUERY_TIMEOUT")
            return await original(a, name, *args, **kwargs)

        with patch.object(self.panel, "call", side_effect=broken):
            output = await self.dispatch(self.event([C.At(qq="456"), C.Plain("房间状态")]))
        self.assertIn("读取超时", output[-1])
        self.assertNotIn("在线 0", output[-1])
        self.assertEqual(self.models, [])

    async def test_optional_reads_can_fail_without_turning_into_zero(self):
        original = self.panel.call

        async def broken(a, name, *args, **kwargs):
            if name in {"api_listRoomPlayers", "api_listRoomMods"}:
                raise QueryError("PANEL_DISCONNECTED")
            return await original(a, name, *args, **kwargs)

        with patch.object(self.panel, "call", side_effect=broken):
            output = await self.dispatch(self.event([C.At(qq="456"), C.Plain("房间状态")]))
        self.assertIn("运行中", output[-1])
        self.assertIn("在线玩家：暂未读取成功，数量未确认", output[-1])
        self.assertIn("模组：暂未读取成功，数量未确认", output[-1])
        self.assertNotIn("在线 0", output[-1])
        self.assertNotIn("模组 · 已配置 0", output[-1])
        self.assertEqual(self.models, [])
