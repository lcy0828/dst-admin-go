import sys
import unittest
from pathlib import Path
from unittest.mock import AsyncMock

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from astrbot_plugin_dst_broadcast.broadcast import BroadcastEngine


class BroadcastTests(unittest.IsolatedAsyncioTestCase):
    async def test_additive_related_object_keeps_one_delivery_owner(self):
        save = AsyncMock()
        original = AsyncMock(return_value=True)
        related = AsyncMock(return_value=True)
        engine = BroadcastEngine(
            AsyncMock(), original, save, {"r1": "示例房间"}, emit_notice=related
        )
        target = {"roomId": "r1", "playerId": "KU_example", "kind": "player", "name": "示例玩家"}
        engine.queue("example-event", "示例通知", target)
        await engine.deliver_one()
        original.assert_not_awaited()
        related.assert_awaited_once_with("示例通知", target)
        self.assertEqual(engine.state["deliveries"][-1]["status"], "sent")
        self.assertEqual(engine.state["outbox"], [])

    async def test_old_outbox_and_uncertain_delivery_are_not_replayed(self):
        original = AsyncMock(return_value=True)
        save = AsyncMock()
        engine = BroadcastEngine(
            AsyncMock(),
            original,
            save,
            {"r1": "房间"},
            clock=lambda: 100,
            state={
                "version": 1,
                "outbox": [{"key": "old", "text": "旧通知", "created": 99}],
                "deliveries": [{"key": "uncertain", "status": "sending"}],
            },
        )
        await engine.deliver_one()
        original.assert_awaited_once_with("旧通知")
        self.assertEqual(engine.state["deliveries"][0]["status"], "unknown")

    async def test_delivery_exception_does_not_use_second_sender(self):
        original = AsyncMock(return_value=True)
        bridge = AsyncMock(side_effect=TimeoutError())
        engine = BroadcastEngine(
            AsyncMock(), original, AsyncMock(), {"r1": "房间"}, emit_notice=bridge
        )
        engine.queue("event", "通知", {"roomId": "r1"})
        await engine.deliver_one()
        await engine.deliver_one()
        original.assert_not_awaited()
        self.assertEqual(bridge.await_count, 1)
        self.assertEqual(engine.state["deliveries"][-1]["status"], "unknown")


if __name__ == "__main__":
    unittest.main()
