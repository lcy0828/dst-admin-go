"""Read-only recovery from a real closed AnyIO MCP session, with mock HTTP only."""

import asyncio
import json
import unittest
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock

import anyio
import httpx

from astrbot_plugin_dst_companion.broadcast import READ_TOOLS
from astrbot_plugin_dst_companion.notifications import Notifications
from astrbot_plugin_dst_companion.streams import (
    MCPReadError,
    closed_transport,
    endpoint_valid,
    stateless_read,
)


class NotificationRecoveryTests(unittest.IsolatedAsyncioTestCase):
    async def test_closed_sdk_session_recovers_without_replacing_it_or_writing(self):
        calls = []

        async def handler(request):
            body = json.loads(request.content)
            calls.append(body)
            self.assertNotIn("mcp-session-id", request.headers)
            self.assertEqual(str(request.url.query), "b'view=bot&pagination=off'")
            return httpx.Response(
                200,
                json={
                    "jsonrpc": "2.0",
                    "id": body["id"],
                    "result": {
                        "content": [
                            {
                                "type": "text",
                                "text": json.dumps(
                                    {
                                        "status": "succeeded",
                                        "data": {"httpStatus": 200, "data": {"items": []}},
                                    }
                                ),
                            }
                        ]
                    },
                },
            )

        session = NS(call_tool=AsyncMock(side_effect=anyio.ClosedResourceError))
        client = NS(
            session=session,
            tools=[NS(name="api_listRoomChatLogs", annotations=NS(readOnlyHint=True))],
        )
        config = {
            "url": "https://panel.example/mcp?view=bot&pagination=off",
            "headers": {"Authorization": "Bearer example-only", "Mcp-Session-Id": "old"},
        }
        p = NS(
            manager=NS(mcp_server_runtime_view={"panel": NS(client=client)}),
            connections=NS(broadcast=lambda _: "panel", configuration=lambda _: config),
            background=asyncio.Semaphore(2),
            gateway=NS(limit=asyncio.Semaphore(2)),
            connection_identity=lambda: "identity",
        )
        hub = Notifications(p)
        hub.source = "identity"
        hub.engines = {"session": NS(rooms={"r1": "Example"})}
        async with httpx.AsyncClient(transport=httpx.MockTransport(handler)) as http:
            hub.http = http
            for _ in range(2):
                result = await hub._read("api_listRoomChatLogs", {"path": {"roomId": "r1"}})
                self.assertEqual(result, {"items": []})
        self.assertIs(client.session, session)
        self.assertEqual(session.call_tool.await_count, 1)
        self.assertEqual(len(calls), 2)
        self.assertTrue(all(c["params"]["name"] == "api_listRoomChatLogs" for c in calls))

    def test_recovery_preserves_only_known_listing_options(self):
        for query in ("", "?view=bot", "?view=all&pagination=off", "?pagination=on"):
            self.assertTrue(endpoint_valid({"url": "https://panel.example/mcp" + query}))
        for query in (
            "?token=x",
            "?view=bot&view=all",
            "?pagination=bad",
            "?view=",
            "?view=bot&redirect=x",
            "?view=bot&pagination=off&extra=x",
        ):
            self.assertFalse(endpoint_valid({"url": "https://panel.example/mcp" + query}))

    async def test_recovery_rejects_writes_wrong_ids_redirects_and_oversized_responses(self):
        async def handler(request):
            return httpx.Response(200, json={"id": "other", "result": {}})

        config = {"url": "https://panel.example/mcp"}
        async with httpx.AsyncClient(transport=httpx.MockTransport(handler)) as http:
            for name, url in (
                ("api_runBotCapability", config["url"]),
                ("api_listRoomChatLogs", "https://panel.example/mcp?token=x"),
            ):
                with self.assertRaises(MCPReadError):
                    await stateless_read(http, {"url": url}, name, {}, READ_TOOLS)
            with self.assertRaises(MCPReadError):
                await stateless_read(http, config, "api_listRoomChatLogs", {}, READ_TOOLS)
        for response in (
            httpx.Response(307, headers={"location": "https://other.example/mcp"}),
            httpx.Response(
                200, headers={"content-type": "application/json"}, content=b"x" * ((2 << 20) + 1)
            ),
        ):
            async with httpx.AsyncClient(
                transport=httpx.MockTransport(lambda _, response=response: response)
            ) as http:
                with self.assertRaises(MCPReadError):
                    await stateless_read(http, config, "api_listRoomChatLogs", {}, READ_TOOLS)
        self.assertTrue(closed_transport(ExceptionGroup("closed", [anyio.ClosedResourceError()])))
        self.assertFalse(closed_transport(RuntimeError("business error")))
