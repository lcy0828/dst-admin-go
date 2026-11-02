import copy
import json
import time
import unittest
from dataclasses import replace
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock

from test_companion import actor
from test_v41 import Panel

from astrbot_plugin_dst_companion.capabilities import Directory
from astrbot_plugin_dst_companion.engine import Engine
from astrbot_plugin_dst_companion.gateway import Gateway, QueryError
from astrbot_plugin_dst_companion.jobs import Actions
from astrbot_plugin_dst_companion.policy import Policy
from astrbot_plugin_dst_companion.read_receipts import ReadReceipts
from astrbot_plugin_dst_companion.routing import Intent
from astrbot_plugin_dst_companion.state import State, Target
from astrbot_plugin_dst_companion.task_tools import TaskTools


class ReadReceiptTests(unittest.IsolatedAsyncioTestCase):
    async def test_lost_read_is_recovered_after_reload_without_resubmission(self):
        storage = {}

        async def load(key, default):
            return copy.deepcopy(storage.get(key, default))

        async def save(key, value):
            storage[key] = copy.deepcopy(value)

        panel, a = Panel(), actor()
        panel.lose_reply = True
        policy = Policy(AsyncMock(return_value=True))
        gateway = Gateway(panel.transport, policy.authorize)
        receipts = ReadReceipts(load, save)
        directory = Directory(gateway, receipts=receipts)
        policy.directory = directory
        engine = Engine(gateway, State(), directory=directory)
        reply = await engine.execute(
            a, Intent("capability", room="r1", capability="room.new-read"), Target("r1")
        )
        self.assertEqual(reply.data["status"], "uncertain")
        request_id = reply.data["requestId"]
        self.assertIn(request_id, storage["task_reads_v1"])
        self.assertNotIn("actions_v1", storage)

        # A fresh adapter reads the persisted handle and asks for the original
        # receipt. It neither re-runs the query nor needs the old Python objects.
        reloaded = ReadReceipts(load, save)
        actions = Actions(gateway, AsyncMock(), AsyncMock(), directory=directory)
        tasks = TaskTools(
            NS(
                read_receipts=reloaded,
                load_journal=AsyncMock(return_value={}),
                engine=NS(actions=actions),
            )
        )
        result = json.loads(await tasks.result(a, request_id))
        self.assertEqual(result["result"]["data"], {"answer": 42})
        self.assertEqual(result["taskRef"], request_id)
        self.assertEqual(panel.executions, 1)
        for changed in (
            replace(a, key=("other",)),
            replace(a, rooms=("other",)),
            replace(a, visibility=frozenset()),
        ):
            with self.assertRaises(QueryError):
                await tasks.result(changed, request_id)
        self.assertEqual(panel.executions, 1)
        await gateway.close()

    async def test_expired_read_handle_cannot_be_reused(self):
        a = actor()
        receipts = ReadReceipts(AsyncMock(return_value={}), AsyncMock())
        receipts.load.return_value = {
            "fixture": {
                "owner": receipts.owner(a),
                "roomId": "r1",
                "connectionId": a.connection_id,
                "createdAt": time.time() - 86401,
            }
        }
        with self.assertRaises(QueryError):
            await receipts.find(a, "fixture")
