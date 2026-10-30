"""Protocol, configuration, model-free UX and failure-window acceptance."""

import asyncio
import copy
import hashlib
import json
import sys
import time
import unittest
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from test_companion import FakeGateway, actor, mcp

from astrbot_plugin_dst_companion.capabilities import Directory, compatible
from astrbot_plugin_dst_companion.configuration import (
    DEFAULTS,
    ConfigEditor,
    Configuration,
    fingerprint,
    safe_reload,
)
from astrbot_plugin_dst_companion.engine import Engine
from astrbot_plugin_dst_companion.gateway import Gateway, QueryError
from astrbot_plugin_dst_companion.jobs import Actions
from astrbot_plugin_dst_companion.native_permissions import check_native
from astrbot_plugin_dst_companion.policy import Policy
from astrbot_plugin_dst_companion.presentation import business, render
from astrbot_plugin_dst_companion.state import State, Target


def capability(id="room.new-read", access="read", schema=None):
    return {
        "id": id,
        "title": "新增查询",
        "category": "status" if access == "read" else "management",
        "access": access,
        "contractVersion": 1,
        "schemaRevision": "schema-1",
        "parameters": schema
        or {"type": "object", "properties": {}, "required": [], "additionalProperties": False},
        "nativeTools": ["api_listRoomWorlds" if access == "read" else "api_createRoomBackup"],
        "parameterHints": {},
        "resultKind": "facts" if access == "read" else "job",
    }


class Panel:
    def __init__(self):
        self.items = [capability()]
        self.revision = "revision-1"
        self.calls = []
        self.executions = 0
        self.lose_reply = False
        self.requests = {}
        self.result_failure = False
        self.pages = None

    async def transport(self, name, args, timeout, write):
        self.calls.append((name, copy.deepcopy(args)))
        if name == "api_listRooms":
            return mcp({"items": [{"id": "r1", "name": "房间一"}]})
        if name == "api_listBotCapabilities":
            if self.pages:
                return mcp(self.pages.pop(0))
            if args["query"].get("knownRevision") == self.revision:
                return mcp({"protocolVersion": 1, "revision": self.revision, "notModified": True})
            return mcp(
                {
                    "protocolVersion": 1,
                    "revision": self.revision,
                    "items": self.items,
                    "nextCursor": "",
                }
            )
        if name in {"api_queryBotCapability", "api_runBotCapability"}:
            self.executions += 1
            request = args.get("requestId") or args["query"]["requestId"]
            result = {
                "requestId": request,
                "status": "running" if write else "succeeded",
                "complete": not write,
                "source": "platform",
                "resultKind": "job" if write else "facts",
                "data": {"answer": 42},
                "capabilityId": args["path"]["capabilityId"],
                "roomId": "r1",
            }
            if write:
                result["jobId"] = "job-example"
            self.requests[request] = result
            if self.lose_reply:
                raise QueryError("RESULT_UNCERTAIN")
            return mcp(result)
        if name == "api_getBotCapabilityResult":
            if self.result_failure:
                raise QueryError("API_FAILED")
            result = self.requests[args["path"]["requestId"]]
            return mcp({**result, "status": "succeeded", "complete": True})
        raise QueryError("CAPABILITY_MISSING")


class ConfigTests(unittest.IsolatedAsyncioTestCase):
    def source(self):
        return {
            "configuration_version": 2,
            "defaults": {
                **copy.deepcopy(DEFAULTS),
                "rooms": ["r1"],
                "pageSize": 8,
                "taskNotifications": True,
                "writeAllowlist": ["room.backup.create"],
            },
            "session_overrides": [
                {
                    "session": "group",
                    "overrideFields": ["taskNotifications", "writeAllowlist"],
                    "values": {
                        **copy.deepcopy(DEFAULTS),
                        "pageSize": 5,
                        "taskNotifications": False,
                        "writeAllowlist": [],
                    },
                }
            ],
        }

    def test_C07_explicit_overrides_survive_framework_filling(self):
        values = Configuration(self.source()).effective("group")
        self.assertEqual(values["pageSize"], 8)
        self.assertFalse(values["taskNotifications"])
        self.assertEqual(values["writeAllowlist"], ())
        self.assertEqual(values.origins["pageSize"], "global")
        with self.assertRaises(TypeError):
            values.values["rooms"] = ("other",)

    def test_C11_invalid_authorization_never_uses_old_allow(self):
        source = self.source()
        source["session_overrides"][0]["values"]["writeAllowlist"] = ["room.delete"]
        with self.assertRaises(ValueError):
            Configuration(source).effective("group")
        source = self.source()
        source["session_overrides"].append(copy.deepcopy(source["session_overrides"][0]))
        with self.assertRaises(ValueError):
            Configuration(source).effective("group")

    async def test_C08_concurrent_save_does_not_publish_old_candidate(self):
        source = self.source()
        original = fingerprint(source)
        candidate = copy.deepcopy(source)
        candidate["defaults"]["pageSize"] = 9

        async def save():
            source["defaults"]["pageSize"] = 10
            return False

        editor = ConfigEditor(source, save)
        with self.assertRaises(QueryError):
            await editor.apply(candidate, original, ["group"])
        self.assertEqual(Configuration(source).effective("group")["pageSize"], 10)
        with self.assertRaises(QueryError):
            await editor.apply(candidate, original, ["group"])

    def test_C09_unknown_reload_never_calls_framework(self):
        manager = NS(reload=AsyncMock())
        with self.assertRaises(QueryError):
            safe_reload(manager, "missing", {"real": object()})
        manager.reload.assert_not_called()

    def test_migration_and_global_disables_preserve_authority(self):
        from astrbot_plugin_dst_companion.configuration import migrate_v2

        source = {"room_ids": ["r1"], "room_aliases": '{"1服":"r1"}', "allowed_sessions": ["group"]}
        migrated = migrate_v2(source)
        self.assertEqual(migrated["configuration_version"], 2)
        self.assertEqual(Configuration(migrated).effective("group")["mode"], "mixed")
        migrated["defaults"]["disabledCapabilities"] = ["player.inventory"]
        migrated["session_overrides"] = [
            {
                "session": "group",
                "overrideFields": ["disabledCapabilities"],
                "values": {"disabledCapabilities": []},
            }
        ]
        with self.assertRaises(ValueError):
            Configuration(migrated).effective("group")

    def test_legacy_keeps_mixed_chat(self):
        self.assertEqual(Configuration({"room_ids": ["r1"]}).effective("group")["mode"], "mixed")


class NativeTests(unittest.IsolatedAsyncioTestCase):
    async def test_C06_storage_failure_unknown_rule_and_old_admin_deny(self):
        a = actor()
        a.event.is_admin = lambda: False
        helper = AsyncMock(return_value=None)  # the framework's fail-open behavior
        for load in (
            AsyncMock(side_effect=OSError()),
            AsyncMock(return_value={"_default": {"api_old": "everyone"}}),
            AsyncMock(return_value={"_default": {"api_old": "admin"}}),
            AsyncMock(return_value=[]),
        ):
            self.assertFalse(await check_native(load, helper, None, a, "api_old", NS()))
        helper.assert_not_awaited()
        self.assertTrue(
            await check_native(AsyncMock(return_value={}), helper, None, a, "api_old", NS())
        )


class DynamicTests(unittest.IsolatedAsyncioTestCase):
    def setup_panel(self, panel=None):
        panel = panel or Panel()
        policy = Policy(AsyncMock(return_value=True))
        gateway = Gateway(panel.transport, policy.authorize)
        directory = Directory(gateway)
        policy.directory = directory
        return panel, gateway, directory

    async def test_C01_same_plugin_gets_new_capability_through_menu_parameters(self):
        import importlib.util
        import tempfile

        package_file = Path(__file__).resolve().parents[1] / "package.py"
        spec = importlib.util.spec_from_file_location("dst_package_acceptance", package_file)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)

        def package_hash():
            with tempfile.TemporaryDirectory() as output:
                return hashlib.sha256(
                    module.build(Path(output), "companion").read_bytes()
                ).hexdigest()

        before = package_hash()
        panel, gateway, directory = self.setup_panel()
        clock = [0]
        directory.clock = lambda: clock[0]
        a = actor()
        engine = Engine(gateway, State(), directory=directory)
        await directory.items(a, time.monotonic() + 8)
        panel.items.append(
            capability(
                "room.added-later",
                schema={
                    "type": "object",
                    "properties": {"period": {"type": "integer", "minimum": 1, "maximum": 30}},
                    "required": ["period"],
                    "additionalProperties": False,
                },
            )
        )
        panel.revision = "revision-2"
        clock[0] = 61
        reply = await engine.handle(a, "菜单", woken=True)
        while "room.added-later" not in [
            i["id"]
            for i in engine.state.choices[a.key]["candidates"][
                engine.state.choices[a.key]["page"] * 5 : (engine.state.choices[a.key]["page"] + 1)
                * 5
            ]
        ]:
            reply = await engine.handle(a, "下一页", woken=True)
        pending = engine.state.choices[a.key]
        visible = pending["candidates"][pending["page"] * 5 : (pending["page"] + 1) * 5]
        number = next(i + 1 for i, item in enumerate(visible) if item["id"] == "room.added-later")
        reply = await engine.handle(a, str(number), woken=True)
        self.assertIn("period", reply.text)
        bad = await engine.handle(a, "0", woken=True)
        self.assertIn("参数", bad.text)
        reply = await engine.handle(a, "7", woken=True)
        self.assertIn("42", reply.text)
        call = next(args for name, args in panel.calls if name == "api_queryBotCapability")
        self.assertEqual(json.loads(call["query"]["parameters"]), {"period": 7})
        self.assertEqual(before, package_hash())
        self.assertFalse(any("model" in name or "Raw" in name for name, _ in panel.calls))
        await gateway.close()

    async def test_C10_changed_page_never_publishes_half_directory(self):
        panel, gateway, directory = self.setup_panel()
        a = actor()
        original = await directory.items(a, time.monotonic() + 8)
        panel.pages = [
            {
                "protocolVersion": 1,
                "revision": "new",
                "items": [capability("room.extra")],
                "nextCursor": "page-2",
            },
            {"protocolVersion": 1, "revision": "changed-mid-page", "items": [], "nextCursor": ""},
        ]
        with self.assertRaises(QueryError):
            await directory.items(a, time.monotonic() + 8, force=True)
        self.assertEqual(directory.cache[a.read_projection]["items"], original)
        self.assertFalse(compatible({"type": "object", "$ref": "https://invalid.example/schema"}))
        await gateway.close()

    async def test_C02_C03_lost_write_response_reload_recovers_same_request(self):
        from dataclasses import replace

        panel, gateway, directory = self.setup_panel()
        panel.items = [capability("room.backup.create", "write")]
        a = actor(role="game-manage")
        a = replace(a, write_allowlist=())
        await directory.items(a, time.monotonic() + 8)
        item = panel.items[0]
        with self.assertRaises(QueryError):
            await directory.execute(a, item, "r1", {}, time.monotonic() + 8)
        a = replace(a, write_allowlist=("room.backup.create",))
        await directory.items(a, time.monotonic() + 8)
        records = {}

        async def load():
            return copy.deepcopy(records)

        async def save(value):
            records.clear()
            records.update(copy.deepcopy(value))

        actions = Actions(gateway, load, save, directory=directory)
        panel.lose_reply = True
        with self.assertRaises(QueryError):
            await actions.submit_capability(a, item, Target("r1"), {}, time.monotonic() + 8)
        self.assertEqual(panel.executions, 1)
        request_id = next(iter(records.values()))["requestId"]
        await actions.close()
        await gateway.close()
        _, gateway, directory = self.setup_panel(panel)
        restored = Actions(gateway, load, save, directory=directory)
        text = await restored.latest(a, "r1", time.monotonic() + 8)
        self.assertIn("已完成", text)
        self.assertIn(request_id, text)
        self.assertEqual(panel.executions, 1)
        # C05: a missing result must not trigger another submission.
        panel.result_failure = True
        with self.assertRaises(QueryError):
            await restored.latest(a, "r1", time.monotonic() + 8)
        self.assertEqual(panel.executions, 1)
        await restored.close()
        await gateway.close()

    async def test_duplicate_choice_and_missing_player_workflow(self):
        state, gateway = State(), FakeGateway()
        engine = Engine(gateway, state)
        a = actor()
        reply = await engine.handle(a, "背包", woken=True, message_id="initial")
        self.assertIn("昵称", reply.text)
        reply = await engine.handle(a, "玩家 A 🦌", woken=True, message_id="answer")
        self.assertIn("背包", reply.text)
        a = actor(rooms=("r1", "r2"))
        await engine.handle(a, "有哪些服", woken=True, message_id="rooms")
        await engine.handle(a, "1", woken=True, message_id="choose")
        await engine.handle(a, "有哪些服", woken=True, message_id="rooms-again")
        await engine.handle(a, "1", woken=True, message_id="choose")
        self.assertIn(a.key, state.choices)

    async def test_C12_delivery_timeout_is_not_retried(self):
        records = {"key": {"requestId": "request-example", "jobId": "job-a", "state": "received"}}

        async def load():
            return copy.deepcopy(records)

        async def save(value):
            records.clear()
            records.update(copy.deepcopy(value))

        notify = AsyncMock(side_effect=TimeoutError())
        actions = Actions(FakeGateway(), load, save, notify=notify)
        watch = {"actor": actor(), "record": records["key"]}
        actions.watches["key"] = watch
        with self.assertRaises(TimeoutError):
            await actions._finish_watch("key", watch, {"status": "succeeded", "outcome": "full"})
        self.assertNotIn("key", actions.watches)
        self.assertEqual(records["key"]["delivery"], "delivery_unknown")
        notify.assert_awaited_once()
        await actions.close()

    async def test_watch_finishes_partial_and_rejected_without_more_polling(self):
        for status in ("partial", "rejected"):
            with self.subTest(status=status):
                record = {
                    "requestId": "request-example",
                    "roomId": "r1",
                    "protocol": 1,
                    "state": "received",
                }
                records = {"key": record}

                async def load(journal=records):
                    return copy.deepcopy(journal)

                async def save(value, journal=records):
                    journal.clear()
                    journal.update(copy.deepcopy(value))

                notify = AsyncMock(return_value="message-example")
                actions = Actions(FakeGateway(), load, save, notify=notify)
                actions.get_job = AsyncMock(
                    return_value={
                        "status": status,
                        "complete": False,
                        "nextAction": "none",
                        "requestId": record["requestId"],
                    }
                )
                actions.watches["key"] = {
                    "actor": actor(),
                    "record": record,
                    "next": 0,
                    "until": time.monotonic() + 300,
                }
                await asyncio.wait_for(actions._watch_loop(), 1)
                actions.get_job.assert_awaited_once()
                notify.assert_awaited_once()
                self.assertEqual(records["key"]["state"], "terminal")
                self.assertEqual(records["key"]["delivery"], "sent")
                if status == "partial":
                    self.assertIn("仅有部分结果", notify.call_args.args[1])
                    self.assertNotIn("已完成", notify.call_args.args[1])
                await actions.close()


class PresentationTests(unittest.TestCase):
    def test_templates_cannot_access_objects_or_hide_required_status(self):
        self.assertEqual(render("{body.__class__}", title="t", body="b"), "t\nb")
        body, footer = business(
            {"status": "succeeded", "complete": False, "data": None, "requestId": "example-request"}
        )
        self.assertIn("没有确认", body)
        self.assertIn("部分", footer)
        self.assertIn("example-request", footer)
