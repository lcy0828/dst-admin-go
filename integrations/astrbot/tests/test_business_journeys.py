"""Distinct user journeys through AstrBot; external services are controlled fixtures.

Each test asserts identities, effects or recovery across multiple messages.
These are not live QQ sends or model evaluations.
"""

import asyncio
import importlib.util
import json
import unittest
from types import SimpleNamespace as NS
from unittest.mock import patch

from test_companion import FakeGateway, P

AVAILABLE = importlib.util.find_spec("astrbot") is not None
if AVAILABLE:
    import test_astrbot_adapter as adapter
    import test_qq_pipeline as pipeline
    import test_task_tools as task_adapter
    from astrbot.api import message_components as C


class DirectoryFixture(FakeGateway):
    def __init__(self):
        super().__init__()
        self.people = {
            "r1": [
                {**P, "id": "KU_alpha", "name": "同名示例", "worldName": "Master"},
                {**P, "id": "KU_beta", "name": "同名示例", "worldName": "Caves"},
            ],
            "r2": [{**P, "id": "KU_elsewhere", "name": "异服示例"}],
        }

    async def call(self, a, name, args, deadline, **kwargs):
        result = await super().call(a, name, args, deadline, **kwargs)
        path = args.get("path", {})
        people = self.people.get(path.get("roomId"), [])
        if name == "api_resolveRoomPlayer":
            query = args["query"]["query"]
            matches = [p for p in people if query in {p["id"], p["name"]}]
            return {
                "status": "resolved"
                if len(matches) == 1
                else "ambiguous"
                if matches
                else "not_found",
                "items": matches,
                "match": "exact_id" if query.startswith("KU_") else "exact_name",
            }
        if name == "api_getRoomPlayerInventory":
            assert any(p["id"] == path["playerId"] for p in people), "Wrong room/player"
            return {
                "source": "save",
                "status": "complete",
                "savedAt": "2026-01-01T00:00:00Z",
                "inventory": [{"prefab": "twigs", "stackSize": 3}],
            }
        return result


@unittest.skipUnless(AVAILABLE, "requires the installed AstrBot framework")
class ConversationJourneys(unittest.IsolatedAsyncioTestCase):
    if AVAILABLE:
        asyncTearDown = pipeline.QQPipelineTests.asyncTearDown
        event = pipeline.QQPipelineTests.event
        dispatch = pipeline.QQPipelineTests.dispatch

    async def asyncSetUp(self):
        await pipeline.QQPipelineTests.asyncSetUp(self)
        self.panel = DirectoryFixture()
        self.bot.config.update(
            room_ids=["r1", "r2"], default_room_id="r1", room_aliases={"1服": "r1", "2服": "r2"}
        )

    async def say(self, text, *, user="123", wake=True):
        parts = ([C.At(qq="456")] if wake else []) + [C.Plain(text)]
        event = self.event(parts, user=user)
        return event, await self.dispatch(event)

    def inventory_paths(self):
        return [
            args["path"] for name, args in self.panel.calls if name == "api_getRoomPlayerInventory"
        ]

    async def test_duplicate_names_require_choice_then_other_user_cannot_take_the_selection(self):
        _, output = await self.say("1服查背包 同名示例")
        self.assertIn("KU_alpha", output[-1])
        self.assertIn("KU_beta", output[-1])
        await self.say("2", user="999", wake=False)
        self.assertEqual(self.inventory_paths(), [])
        await self.say("2", wake=False)
        self.assertEqual(self.inventory_paths(), [{"roomId": "r1", "playerId": "KU_beta"}])
        await self.say("2服查背包 异服示例")
        self.assertEqual(self.inventory_paths()[-1], {"roomId": "r2", "playerId": "KU_elsewhere"})
        self.assertEqual(self.models, [])

    async def test_two_users_pending_prompts_keep_separate_players_and_followups(self):
        await self.say("玩家信息")
        await self.say("查背包", user="999")
        await self.say("KU_alpha", wake=False)
        await self.say("KU_beta", user="999", wake=False)
        await self.say("查他背包")
        self.assertEqual(
            self.inventory_paths(),
            [
                {"roomId": "r1", "playerId": "KU_beta"},
                {"roomId": "r1", "playerId": "KU_alpha"},
            ],
        )
        self.assertEqual(self.models, [])

    async def test_natural_cancel_ends_read_prompt_and_later_plain_name_is_not_consumed(self):
        await self.say("查背包")
        _, output = await self.say("算了，不查了", wake=False)
        self.assertTrue(any("已取消" in s for s in output), output)
        await self.say("KU_alpha", wake=False)
        self.assertEqual(self.inventory_paths(), [])
        self.assertFalse(any(n == "api_resolveRoomPlayer" for n, _ in self.panel.calls))
        self.assertEqual(self.models, [])

    async def test_natural_cancel_ends_choice_without_becoming_stop_task_permission_error(self):
        await self.say("查背包 同名示例")
        _, output = await self.say("算了", wake=False)
        self.assertTrue(any("已取消" in s for s in output), output)
        await self.say("1", wake=False)
        self.assertEqual(self.inventory_paths(), [])
        self.assertEqual(self.models, [])

    async def test_cancel_during_slow_read_prevents_late_progress_and_result(self):
        started, release = asyncio.Event(), asyncio.Event()
        original = self.panel.call

        async def slow(a, name, *args, **kwargs):
            if name == "api_listWorldStates":
                started.set()
                await release.wait()
            return await original(a, name, *args, **kwargs)

        with patch.object(self.panel, "call", side_effect=slow):
            pending = self.event([C.At(qq="456"), C.Plain("房间状态")])
            work = asyncio.create_task(self.dispatch(pending))
            try:
                await asyncio.wait_for(started.wait(), 2)
                await self.say("取消")
                await asyncio.sleep(1.6)  # Cross the adapter's progress notification deadline.
            finally:
                release.set()
                await work
        self.assertEqual(pending.bot.call_action.await_count, 0)
        self.assertEqual(self.models, [])

    async def test_old_quote_cannot_cross_a_panel_connection_change(self):
        first, _ = await self.say("查玩家 KU_alpha")
        receipt_id = str(1000 + int(first.message_obj.message_id))
        with patch.object(self.bot.connections, "source", return_value="another-panel"):
            event = self.event(
                [C.Reply(id=receipt_id, sender_id="456"), C.At(qq="456"), C.Plain("查背包")]
            )
            output = await self.dispatch(event)
        self.assertIn("未能确认引用", output[-1])
        self.assertEqual(self.inventory_paths(), [])
        self.assertEqual(self.models, [])

    async def test_cancel_also_suppresses_progress_already_waiting_for_send_capacity(self):
        started, release = asyncio.Event(), asyncio.Event()
        original = self.panel.call

        async def slow(a, name, *args, **kwargs):
            if name == "api_listWorldStates":
                started.set()
                await release.wait()
            return await original(a, name, *args, **kwargs)

        await self.bot.send_lock.acquire()
        await self.bot.send_lock.acquire()
        with patch.object(self.panel, "call", side_effect=slow):
            pending = self.event([C.At(qq="456"), C.Plain("房间状态")])
            cancel = self.event([C.At(qq="456"), C.Plain("取消")])
            work = asyncio.create_task(self.dispatch(pending))
            cancel_task = None
            try:
                await asyncio.wait_for(started.wait(), 2)
                await asyncio.sleep(1.6)
                cancel_task = asyncio.create_task(self.dispatch(cancel))
                async with asyncio.timeout(2):
                    while not self.bot.state.current_request(
                        self.bot.actor(cancel).key, cancel.message_obj.message_id
                    ):
                        await asyncio.sleep(0)
                await asyncio.sleep(0)
            finally:
                self.bot.send_lock.release()
                self.bot.send_lock.release()
                release.set()
                await work
                if cancel_task:
                    await cancel_task
        self.assertEqual(pending.bot.call_action.await_count, 0)
        self.assertEqual(cancel.bot.call_action.await_count, 1)

    async def test_offline_renamed_player_quote_still_uses_confirmed_id(self):
        first, _ = await self.say("查玩家 KU_beta")
        person = self.panel.people["r1"][1]
        person.update(name="改名后的示例", online=False, presenceStatus="offline")
        receipt_id = str(1000 + int(first.message_obj.message_id))
        event = self.event(
            [C.Reply(id=receipt_id, sender_id="456"), C.At(qq="456"), C.Plain("查背包")]
        )
        output = await self.dispatch(event)
        self.assertIn("改名后的示例", output[-1])
        self.assertIn("存档快照", output[-1])
        self.assertEqual(self.inventory_paths(), [{"roomId": "r1", "playerId": "KU_beta"}])

    async def test_read_failure_then_recovery_keeps_request_explicit_and_never_claims_zero(self):
        from astrbot_plugin_dst_companion.gateway import QueryError

        self.panel.failure = QueryError("PANEL_DISCONNECTED")
        _, first = await self.say("在线玩家")
        self.assertNotIn("在线 0", first[-1])
        self.panel.failure = None
        _, second = await self.say("在线玩家")
        self.assertIn("在线 1", second[-1])
        self.assertEqual(self.models, [])


@unittest.skipUnless(AVAILABLE, "requires the installed AstrBot framework")
class ModelReadJourneys(unittest.IsolatedAsyncioTestCase):
    if AVAILABLE:
        companion = task_adapter.TaskAdapterTests.companion
        event = task_adapter.TaskAdapterTests.event

    async def test_room_discovery_overview_and_help_cannot_be_reinterpreted_as_writes(self):
        for phrase in ("AI: 有哪些服", "AI: 所有服状态", "AI: 菜单", "AI: 备份设置", "AI: 家园"):
            with self.subTest(request=phrase):
                bot, event = self.companion(), self.event("admin")
                bot.config["room_aliases"] = {"家园": "r1"}
                event.get_messages = lambda phrase=phrase: [C.At(qq="456"), C.Plain(phrase)]
                try:
                    for name, args in (
                        ("dst_player_action", {"action": "resurrect", "player": "示例"}),
                        ("dst_server_action", {"action": "restart"}),
                        ("dst_run", {"capability_id": "player.resurrect"}),
                    ):
                        result = json.loads(await bot.tasks.perform(event, name, **args))
                        self.assertEqual(result.get("code"), "READ_REQUEST_ONLY", result)
                    bot.engine.execute.assert_not_awaited()
                    bot.directory.items.assert_not_awaited()
                finally:
                    await bot.terminate()

    async def test_read_request_hides_write_tools_while_explicit_write_still_has_them(self):
        for phrase, expected in (("AI: 所有服状态", False), ("AI: 给 示例 三个肉丸", True)):
            bot, event = self.companion(), self.event("admin")
            event.get_messages = lambda phrase=phrase: [C.At(qq="456"), C.Plain(phrase)]
            request = NS(
                system_prompt="",
                func_tool=adapter.ToolSet(
                    [
                        adapter.FunctionTool(
                            name=name, description="fixture", parameters={"type": "object"}
                        )
                        for name in (
                            "dst_servers",
                            "dst_player_action",
                            "dst_server_action",
                            "dst_run",
                        )
                    ]
                ),
            )
            try:
                await bot.on_model_request(event, request)
                names = {t.name for t in request.func_tool}
                self.assertEqual("dst_player_action" in names, expected)
                self.assertEqual("dst_server_action" in names, expected)
                self.assertEqual("dst_run" in names, expected)
                self.assertIn("dst_servers", names)
            finally:
                await bot.terminate()
