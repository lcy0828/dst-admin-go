"""Synthetic chat stream tests: no model, QQ, game or live credentials."""

import copy
import json
import sys
import unittest
from datetime import datetime, timezone
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock

import httpx

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from astrbot_plugin_dst_companion.broadcast import BroadcastEngine, READ_TOOLS
from astrbot_plugin_dst_companion.chat_stream import (
    CHAT_STREAM_TOOL, accept_chat, chat_stream_session, supports_chat_stream,
)
from astrbot_plugin_dst_companion.notifications import Notifications
from astrbot_plugin_dst_companion.streams import MCPReadError


def record(key="message-1", **changes):
    entry = {"id": key, "kind": "say", "content": "示例聊天", "playerName": "示例玩家",
             "occurredAt": datetime.fromtimestamp(1000, timezone.utc).isoformat()}
    return {"id": key, "kind": "say", "operation": "created", "historical": False,
            "entry": entry, **changes}


def frame(kind, cursor="", payload=None):
    return f"event: {kind}\nid: {cursor}\ndata: {json.dumps(payload or {})}\n\n"


class ChatStreamTests(unittest.IsolatedAsyncioTestCase):
    def engine(self):
        return BroadcastEngine(AsyncMock(return_value={"items": []}), None, AsyncMock(),
                               {"room": "示例房间"}, options={"chat": True}, clock=lambda: 1000)

    def hub(self, engines, content):
        plugin = NS(config={}, alive=True, connection_identity=lambda: "source",
                    catalog=NS(valid=lambda: True), logger=NS(warning=lambda _: None),
                    connections=NS(broadcast=lambda required: "panel",
                                   configuration=lambda _: {"url": "https://example.invalid/mcp?view=bot&pagination=off"}),
                    manager=NS(mcp_server_runtime_view={"panel": NS(client=NS(tools=[
                        NS(name=CHAT_STREAM_TOOL, annotations=NS(readOnlyHint=True))]))}))
        hub = Notifications(plugin)
        hub.source = "source"
        hub.engines = dict(enumerate(engines))
        hub.read = AsyncMock(return_value={"resource": {
            "url": "/api/v2/system/ai/resources/synthetic-stream", "kind": "stream"}})
        requests = []

        async def handle(request):
            requests.append(request)
            return httpx.Response(200, headers={"content-type": "text/event-stream"}, content=content)

        hub.http = httpx.AsyncClient(transport=httpx.MockTransport(handle))
        self.addAsyncCleanup(hub.http.aclose)
        return hub, requests

    async def test_revision_updates_replays_and_private_messages_do_not_duplicate(self):
        engine = self.engine()
        await accept_chat(engine, "room", "chat.record", "one", record())
        await accept_chat(engine, "room", "chat.record", "two", record(operation="updated"))
        await accept_chat(engine, "room", "chat.record", "one", record())
        for index, kind in enumerate(("whisper", "announcement")):
            item = record(str(index), kind=kind)
            item["entry"]["kind"] = kind
            await accept_chat(engine, "room", "chat.record", "other" + str(index), item)
        self.assertEqual(len(engine.state["outbox"]), 1)
        self.assertIn("示例聊天", engine.state["outbox"][0]["text"])
        self.assertEqual(engine.save.call_args.args[0], engine.state)

    async def test_stream_burst_batches_only_unsent_messages(self):
        engine = self.engine()
        for index in range(7):
            await accept_chat(engine, "room", "chat.record", str(index), record(str(index)))
        self.assertEqual([item["related"]["chatCount"] for item in engine.state["outbox"]], [5, 2])
        self.assertEqual(engine.room("room")["chat_cursor"], "6")

    async def test_save_failure_rolls_back_cursor_seen_and_outbox(self):
        engine = self.engine()
        before = copy.deepcopy(engine.state)
        engine.save.side_effect = OSError("synthetic storage failure")
        with self.assertRaises(OSError):
            await accept_chat(engine, "room", "chat.record", "one", record())
        self.assertEqual(engine.state, before)
        engine.save.side_effect = None
        await accept_chat(engine, "room", "chat.record", "one", record())
        self.assertEqual(engine.room("room")["chat_cursor"], "one")
        self.assertEqual(len(engine.state["outbox"]), 1)

    async def test_gaps_and_truncated_archive_records_are_not_complete_chat(self):
        engine = self.engine()
        for kind in ("chat.gap", "chat.reset"):
            await accept_chat(engine, "room", kind, kind, {})
            self.assertEqual(engine.room("room")["chat_stream_warning"], kind)
        for i, change in enumerate(({"historical": True}, {"truncated": True, "entry": None}, {"operation": "deleted", "entry": None})):
            await accept_chat(engine, "room", "chat.record", str(i), record(str(i), **change))
        self.assertEqual(engine.state["outbox"], [])

    async def test_malformed_records_do_not_advance_and_incomplete_eof_reconnects(self):
        engine = self.engine()
        for payload in ({}, record(operation="execute"), record(entry={"id": "forged"})):
            with self.assertRaises(MCPReadError):
                await accept_chat(engine, "room", "chat.record", "bad", payload)
        self.assertNotIn("chat_cursor", engine.room("room"))
        hub, _ = self.hub([engine], frame("chat.cursor", "tail") + 'event: chat.record\nid: lost\ndata: {')
        with self.assertRaises(MCPReadError):
            await chat_stream_session(hub, "room", [engine])
        self.assertEqual(engine.room("room")["chat_cursor"], "tail")

    async def test_shared_stream_and_resource_reuse_resume_per_group(self):
        a, b = self.engine(), self.engine()
        for engine in (a, b):
            engine.room("room")["chat_cursor"] = "previous"
        hub, requests = self.hub([a, b], frame("chat.record", "next", record()) + frame("chat.reconnect"))
        await chat_stream_session(hub, "room", [a, b])
        await chat_stream_session(hub, "room", [a, b])
        self.assertEqual(hub.read.await_count, 1)
        self.assertEqual(requests[0].headers["last-event-id"], "previous")
        self.assertEqual(requests[1].headers["last-event-id"], "next")
        for engine in (a, b):
            self.assertEqual(len(engine.state["outbox"]), 1)
        self.assertNotIn(CHAT_STREAM_TOOL, READ_TOOLS)
        self.assertTrue(supports_chat_stream(hub))
        hub.plugin.manager.mcp_server_runtime_view["panel"].client.tools = []
        self.assertFalse(supports_chat_stream(hub))

    async def test_divergent_group_cursors_catch_up_before_tail_commit(self):
        a, b = self.engine(), self.engine()
        for engine, cursor in ((a, "old"), (b, "newer")):
            engine.room("room").update(chat_seeded=True, chat_cursor=cursor)
            engine.call.return_value = {"items": [record()["entry"]]}
        hub, requests = self.hub([a, b], frame("chat.cursor", "tail") + frame("chat.record", "next", record()) + frame("chat.reconnect"))
        await chat_stream_session(hub, "room", [a, b])
        self.assertNotIn("last-event-id", requests[0].headers)
        for engine in (a, b):
            self.assertEqual(engine.call.await_count, 1)
            self.assertEqual(engine.room("room")["chat_cursor"], "next")
            self.assertEqual(len(engine.state["outbox"]), 1)

    async def test_first_subscription_seeds_history_without_broadcasting_it(self):
        engine = self.engine()
        engine.call.return_value = {"items": [record()["entry"]]}
        hub, _ = self.hub([engine], frame("chat.cursor", "tail") + frame("chat.record", "next", record("live")) + frame("chat.reconnect"))
        await chat_stream_session(hub, "room", [engine])
        self.assertEqual(len(engine.state["outbox"]), 1)
        self.assertIn("live", engine.state["outbox"][0]["key"])

    async def test_scope_change_aborts_without_delivering_or_advancing(self):
        engine = self.engine()
        engine.room("room")["chat_cursor"] = "old"
        hub, _ = self.hub([engine], frame("chat.record", "next", record()))
        hub.plugin.connection_identity = lambda: "changed"
        with self.assertRaises(MCPReadError):
            await chat_stream_session(hub, "room", [engine])
        self.assertEqual(engine.room("room")["chat_cursor"], "old")
        self.assertFalse(engine.state["outbox"])

    async def test_storage_false_is_not_acknowledged(self):
        hub, _ = self.hub([], "")
        hub.plugin.put_kv_data = AsyncMock(return_value=False)
        with self.assertRaises(Exception):
            await hub.save("synthetic-session", "source", {})
        self.assertEqual(hub.saved, {})


if __name__ == "__main__":
    unittest.main()
