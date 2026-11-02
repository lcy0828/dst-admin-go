import json
import sys
import unittest
from pathlib import Path
from types import SimpleNamespace as NS

import httpx

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from astrbot_plugin_dst_companion.gateway import QueryError
from astrbot_plugin_dst_companion.private_connection import (
    PRIVATE_NAME,
    PrivateManager,
    PrivatePanel,
)


def config():
    return {"panel_url": "https://panel.invalid", "panel_token": "dstai_" + "fixture" * 5}


class PrivateConnectionTests(unittest.IsolatedAsyncioTestCase):
    async def test_old_panel_without_handshake_remains_usable_as_protocol_one(self):
        async def handler(request):
            value = json.loads(request.content)
            self.assertEqual(value["method"], "tools/list")
            return httpx.Response(200, json={"id": value["id"], "result": {"tools": [{"name": "api_listBotCapabilities", "inputSchema": {"type": "object"}}]}})

        panel = PrivatePanel(config())
        panel.protocol, panel.full_access = 2, True
        panel.http = httpx.AsyncClient(transport=httpx.MockTransport(handler))
        await panel.refresh()
        self.assertEqual(panel.protocol, 1)
        self.assertFalse(panel.full_access)
        self.assertEqual(panel.tools[0].name, "api_listBotCapabilities")
        await panel.close()

    async def test_private_directory_and_handshake_never_register_host_tools(self):
        calls = []

        async def handler(request):
            value = json.loads(request.content)
            calls.append(value["method"])
            if value["method"] == "tools/list":
                result = {
                    "tools": [{"name": "api_getBotProtocol", "inputSchema": {"type": "object"}}]
                }
            else:
                result = {
                    "content": [],
                    "structuredContent": {
                        "status": "succeeded",
                        "data": {
                            "httpStatus": 200,
                            "data": {
                                "protocolVersions": [1, 2],
                                "capabilityContractVersions": [1],
                                "profile": "panel-manage",
                                "features": ["task-tools"],
                            },
                        },
                    },
                }
            return httpx.Response(200, json={"jsonrpc": "2.0", "id": value["id"], "result": result})

        panel = PrivatePanel(config())
        panel.http = httpx.AsyncClient(transport=httpx.MockTransport(handler))
        host = NS(mcp_server_runtime_view={})
        manager = PrivateManager(host, panel)
        await panel.refresh()
        await panel.refresh()
        self.assertEqual(calls, ["tools/list", "tools/call"])
        self.assertEqual(panel.protocol, 2)
        self.assertTrue(panel.full_access)
        self.assertEqual(host.mcp_server_runtime_view, {})
        self.assertIn(PRIVATE_NAME, manager.mcp_server_runtime_view)
        await panel.close()

    async def test_no_redirect_no_write_replay_and_bounded_response(self):
        for outcome in ("redirect", "lost", "oversized", "wrong_id"):
            calls = []

            async def handler(request, calls=calls, outcome=outcome):
                calls.append(1)
                if outcome == "redirect":
                    return httpx.Response(302, headers={"location": "https://other.invalid"})
                if outcome == "lost":
                    raise httpx.ReadError("connection lost")
                if outcome == "oversized":
                    return httpx.Response(
                        200,
                        content=b" " * (2**21 + 1),
                        headers={"content-type": "application/json"},
                    )
                return httpx.Response(200, json={"id": "wrong", "result": {}})

            panel = PrivatePanel(config())
            panel.http = httpx.AsyncClient(transport=httpx.MockTransport(handler))
            with self.assertRaises((QueryError, httpx.ReadError)):
                await panel.call_tool("api_runBotCapability", {})
            self.assertEqual(len(calls), 1)
            await panel.close()

    async def test_key_change_during_response_cannot_reuse_old_authority(self):
        settings = config()

        async def handler(request):
            value = json.loads(request.content)
            settings["panel_token"] = "dstai_differentfixture"
            return httpx.Response(200, json={"id": value["id"], "result": {"tools": []}})

        panel = PrivatePanel(settings)
        panel.http = httpx.AsyncClient(transport=httpx.MockTransport(handler))
        with self.assertRaises(QueryError):
            await panel.refresh()
        self.assertEqual(panel.tools, [])
        await panel.close()
