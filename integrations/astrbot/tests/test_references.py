"""Trusted broadcast references; no real QQ messages, players or panel writes."""

import copy
import importlib.util
import sys
import time
import unittest
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from astrbot_plugin_dst_companion.broadcast import BroadcastEngine, presence_text
from astrbot_plugin_dst_companion.references import References, presence_identity
from astrbot_plugin_dst_companion.routing import Intent
from astrbot_plugin_dst_companion.state import State, Target

AVAILABLE = importlib.util.find_spec("astrbot") is not None


@unittest.skipUnless(AVAILABLE, "requires the installed AstrBot framework")
class ReferenceTests(unittest.IsolatedAsyncioTestCase):
    def fixture(self):
        from astrbot.api import message_components as C

        text = presence_text("示例房间", "player.left", {"name": "示例玩家"}, "地面", {})
        self.reply = C.Reply(id="42", sender_id="456", message_str=text)
        self.original = {
            "message_id": 42,
            "message_type": "group",
            "group_id": 789,
            "sender": {"user_id": 456},
            "time": time.time(),
            "message": [{"type": "text", "data": {"text": text}}],
        }
        event = NS(
            get_messages=lambda: [self.reply],
            get_platform_name=lambda: "aiocqhttp",
            get_group_id=lambda: "789",
            is_private_chat=lambda: False,
            bot=NS(call_action=AsyncMock(return_value=self.original)),
        )
        self.actor = NS(
            scope=("adapter", "456", "adapter:GroupMessage:123_789"), rooms=("r1",), event=event
        )
        self.plugin = NS(
            state=State(),
            notifications=NS(
                saved={
                    "adapter:GroupMessage:789": {
                        "source": "same-panel",
                        "state": {
                            "deliveries": [
                                {
                                    "key": "r1/world/cursor",
                                    "at": self.original["time"] - 1,
                                    "status": "sent",
                                }
                            ]
                        },
                    }
                }
            ),
            connections=NS(source=lambda: "same-panel"),
            engine=NS(queries=NS(rooms=AsyncMock(return_value=[{"id": "r1", "name": "示例房间"}]))),
        )
        return References(self.plugin)

    async def test_group_receipt_is_shared_only_within_the_same_group_and_bot(self):
        refs = self.fixture()
        scope = ("adapter", "456", "adapter:GroupMessage:789")
        target = Target("r1", "KU_TEST123", "示例玩家")
        self.plugin.state.associate(scope, "42", target, source="same-panel")
        self.assertEqual(await refs.resolve(self.actor, "42", Intent("inventory")), target)
        self.actor.scope = ("adapter", "456", "adapter:GroupMessage:123_999")
        self.actor.event.get_messages = lambda: []
        self.assertIsNone(await refs.resolve(self.actor, "42", Intent("inventory")))
        self.actor.event.bot.call_action.assert_not_awaited()

    async def test_receipt_from_previous_source_cannot_identify_a_player(self):
        refs = self.fixture()
        scope = ("adapter", "456", "adapter:GroupMessage:789")
        self.plugin.state.associate(scope, "42", Target("r1", "KU_TEST123"), source="other-panel")
        self.plugin.notifications.saved.clear()
        self.assertIsNone(await refs.resolve(self.actor, "42", Intent("inventory")))
        self.actor.event.bot.call_action.assert_not_awaited()

    async def test_original_legacy_notice_must_be_authenticated_then_resolve_name(self):
        refs = self.fixture()
        self.original["time"] = int(self.original["time"])
        self.plugin.notifications.saved["adapter:GroupMessage:789"]["state"]["deliveries"][0][
            "at"
        ] = self.original["time"] + 0.8
        target = await refs.resolve(self.actor, "42", Intent("inventory"))
        self.assertEqual(target, Target("r1", "示例玩家", "示例玩家"))
        self.actor.event.bot.call_action.assert_awaited_once_with(
            "get_msg", message_id=42, self_id="456"
        )
        self.plugin.engine.queries.rooms.assert_awaited_once()

    async def test_spoofed_cross_group_old_or_non_text_original_is_rejected(self):
        variants = (
            {"group_id": 999},
            {"sender": {"user_id": 999}},
            {"time": time.time() - 86401},
            {"message_id": 43},
            {"message_type": "private"},
            {"message": [{"type": "forward", "data": {}}]},
        )
        for change in variants:
            with self.subTest(change=change):
                refs = self.fixture()
                self.original.update(change)
                self.assertIsNone(await refs.resolve(self.actor, "42", Intent("inventory")))
                self.plugin.engine.queries.rooms.assert_not_awaited()
        refs = self.fixture()
        self.reply.sender_id = "999"
        self.assertIsNone(await refs.resolve(self.actor, "42", Intent("inventory")))
        self.actor.event.bot.call_action.assert_not_awaited()

    async def test_duplicate_room_labels_and_truncated_nickname_are_not_guessed(self):
        refs = self.fixture()
        self.plugin.engine.queries.rooms.return_value *= 2
        self.assertIsNone(await refs.resolve(self.actor, "42", Intent("inventory")))
        self.assertIsNone(
            presence_identity(
                presence_text("示例房间", "player.left", {"name": "甲" * 80}, "地面", {})
            )
        )

    async def test_sent_reference_survives_reload_but_unknown_delivery_cannot_create_one(self):
        refs = self.fixture()
        saved = []

        async def save(state):
            saved[:] = [copy.deepcopy(state)]

        engine = BroadcastEngine(
            AsyncMock(),
            None,
            save,
            {"r1": "示例房间"},
            emit_notice=AsyncMock(return_value={"messageId": "42", "selfId": "456"}),
        )
        engine.queue(
            "presence-1",
            "示例通知",
            {"roomId": "r1", "playerId": "KU_TEST123", "name": "示例玩家", "kind": "player"},
        )
        await engine.deliver_one()
        self.plugin.notifications.saved = {
            "adapter:GroupMessage:789": {"source": "same-panel", "state": saved[0]}
        }
        self.assertEqual(
            (await refs.resolve(self.actor, "42", Intent("inventory"))).player, "KU_TEST123"
        )
        self.actor.event.bot.call_action.assert_not_awaited()
        # A different credential source cannot inherit an old target.
        self.plugin.connections.source = lambda: "other-panel"
        self.actor.event.get_messages = lambda: []
        self.assertIsNone(await refs.resolve(self.actor, "42", Intent("inventory")))
        engine.emit_notice.side_effect = TimeoutError()
        engine.queue(
            "presence-2", "示例通知", {"roomId": "r1", "playerId": "KU_OTHER123", "kind": "player"}
        )
        await engine.deliver_one()
        self.assertEqual(len(engine.state["references"]), 1)
        self.assertEqual(engine.state["deliveries"][-1]["status"], "unknown")

    async def test_read_references_never_infer_write_targets_or_override_explicit_player(self):
        refs = self.fixture()
        for intent in (
            Intent("kick"),
            Intent("capability", capability="player.give_item"),
            Intent("inventory", player="指定玩家"),
        ):
            self.assertIsNone(await refs.resolve(self.actor, "42", intent))
        self.actor.event.bot.call_action.assert_not_awaited()
