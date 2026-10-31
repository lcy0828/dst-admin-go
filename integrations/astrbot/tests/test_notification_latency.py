"""Bounded chat latency without real QQ, panel, model or game I/O."""

import asyncio
import sys
import unittest
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock, patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from astrbot_plugin_dst_companion.broadcast import BroadcastEngine, chat_label
from astrbot_plugin_dst_companion.configuration import Configuration
from astrbot_plugin_dst_companion.notifications import Notifications


class NotificationLatencyTests(unittest.IsolatedAsyncioTestCase):
    def hub(self):
        plugin = NS(config={"advanced": {}}, alive=True, logger=NS(warning=lambda _: None))
        return Notifications(plugin)

    async def test_chat_poll_defaults_to_three_seconds_and_backs_off_errors(self):
        hub = self.hub()
        engine = NS(
            rooms={"r1": "示例房间"},
            options={"chat": True},
            poll_chat=AsyncMock(side_effect=[RuntimeError(), RuntimeError(), None]),
        )
        hub.engines = {"example-session": engine}
        pauses = []

        async def pause(seconds):
            pauses.append(seconds)
            if len(pauses) == 3:
                raise asyncio.CancelledError

        with patch("astrbot_plugin_dst_companion.notifications.asyncio.sleep", side_effect=pause):
            with self.assertRaises(asyncio.CancelledError):
                await hub.chat_feed("r1")
        for actual, expected in zip(pauses, (6, 12, 3), strict=True):
            self.assertAlmostEqual(actual, expected, delta=0.2)
        self.assertTrue(hub.wakeup.is_set())

    async def test_chat_disabled_performs_no_poll(self):
        hub = self.hub()
        engine = NS(rooms={"r1": "示例房间"}, options={"chat": False}, poll_chat=AsyncMock())
        hub.engines = {"example-session": engine}
        await hub.chat_feed("r1")
        engine.poll_chat.assert_not_awaited()

    async def test_chat_does_not_wait_for_slow_world_seed(self):
        hub = self.hub()
        entered, polled = asyncio.Event(), asyncio.Event()

        async def seed(room):
            entered.set()
            await asyncio.Event().wait()

        async def chat(room):
            polled.set()

        engine = NS(rooms={"r1": "示例房间"}, options={"chat": True}, seed=seed, poll_chat=chat)
        hub.engines = {"example-session": engine}
        task = asyncio.create_task(hub.room_feed("r1"))
        try:
            await asyncio.wait_for(entered.wait(), 1)
            await asyncio.wait_for(polled.wait(), 1)
        finally:
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)

    async def test_pending_presence_detail_cannot_block_persisted_chat_delivery(self):
        hub = self.hub()
        hub.subscriptions = lambda: {"example-session": {}}
        entered, delivered = asyncio.Event(), asyncio.Event()
        sent = []

        async def emit(text, related):
            sent.append(text)
            if len(sent) == 2:
                delivered.set()
            return "receipt-" + str(len(sent))

        async def presence():
            entered.set()
            await asyncio.Event().wait()

        engine = BroadcastEngine(
            AsyncMock(), None, AsyncMock(), {"r1": "示例房间"}, clock=lambda: 1000, emit_notice=emit
        )
        engine.resolve_presence = presence
        engine.room("r1")["pending"]["KU_TEST123"] = {"due": 0}
        engine.queue("chat-1", "消息一", {"roomId": "r1", "kind": "chat"})
        hub.engines = {"example-session": engine}
        task = asyncio.create_task(hub.run())
        try:
            await asyncio.wait_for(entered.wait(), 1)
            async with engine.lock:
                engine.queue("chat-2", "消息二", {"roomId": "r1", "kind": "chat"})
                await engine.persist()
            hub.wakeup.set()
            await asyncio.wait_for(delivered.wait(), 2)
            self.assertEqual(sent, ["消息一", "消息二"])
        finally:
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)
            await hub.close()
        self.assertEqual(hub.presence_tasks, {})

    async def test_burst_chat_batches_without_replay_or_idle_state_writes(self):
        rows = [{"id": "old", "kind": "say", "occurredAt": 990, "content": "历史消息"}]

        async def call(name, arguments):
            self.assertEqual(name, "api_listRoomChatLogs")
            return {"items": rows}

        save = AsyncMock()
        engine = BroadcastEngine(call, None, save, {"r1": "示例房间"}, clock=lambda: 1000)
        await engine.poll_chat("r1")
        self.assertEqual(engine.state["outbox"], [])
        await engine.poll_chat("r1")
        self.assertEqual(save.await_count, 1)
        rows[:0] = [
            {
                "id": "new-" + str(i),
                "kind": "say",
                "occurredAt": 999,
                "playerName": "示例玩家",
                "content": f"消息-{i:02d}",
            }
            for i in range(11, 0, -1)
        ]
        await engine.poll_chat("r1")
        outbox = engine.state["outbox"]
        self.assertEqual([n["related"]["chatCount"] for n in outbox], [5, 5, 1])
        combined = "\n".join(n["text"] for n in outbox)
        self.assertNotIn("历史消息", combined)
        for i in range(1, 12):
            self.assertEqual(combined.count(f"消息-{i:02d}"), 1)
        self.assertLess(combined.index("消息-01"), combined.index("消息-11"))
        self.assertTrue(all(len(n["text"]) < 2800 for n in outbox))
        await engine.poll_chat("r1")
        self.assertEqual(save.await_count, 2)
        self.assertEqual(len(engine.state["outbox"]), 3)

    def test_chat_poll_configuration_is_bounded(self):
        for value in (0, 1, 31, True, "3"):
            with self.assertRaises(ValueError):
                Configuration(
                    {"configuration_version": 3, "advanced": {"chatPollInterval": value}}
                ).effective("example")

    def test_chat_title_is_compact_and_never_displays_game_font_icons(self):
        self.assertEqual(chat_label("\U000f000d示例营地\U000f000d「测试服」「更多说明」"), "测试服")
        self.assertLessEqual(len(chat_label("这是一个特别特别特别特别特别特别长的服务器名称")), 18)
        Configuration({"configuration_version": 3, "advanced": {"chatPollInterval": 3}}).effective(
            "example"
        )
