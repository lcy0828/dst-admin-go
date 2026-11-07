"""Unknown contracts exercise the unchanged adapter through real AstrBot tools."""

import copy
import json
import time
import unittest
import tempfile
import io
import uuid
from dataclasses import replace
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock

import test_astrbot_adapter as adapter
import test_task_tools as task_adapter
from test_companion import actor, mcp
from astrbot_plugin_dst_companion.generic import GenericOperations, actor_envelope
from astrbot_plugin_dst_companion.gateway import unpack, QueryError
from astrbot_plugin_dst_companion.group_permissions import simple_access
from astrbot_plugin_dst_companion.policy import Policy


def install_test_storage(bot):
    storage = {}
    async def get(key, default=None):
        return copy.deepcopy(storage.get(key, default))
    async def put(key, value):
        storage[key] = copy.deepcopy(value)
    bot.get_kv_data, bot.put_kv_data = get, put
    return storage


def contract(ident, access="read", selectors=None):
    return {"id": "api:" + ident, "operationId": ident, "title": "合成接口的原始说明",
            "policy": {"access": access, "confirmation": "none"}, "selectors": selectors or {},
            "parameters": {"type": "object", "properties": {"body": {"type": "object", "properties": {
                "enabled": {"type": "boolean", "default": True},
                "choice": {"oneOf": [{"type": "string"}, {"type": "integer"}]},
                "nullable": {"type": ["string", "null"]}}}}}}


class GenericBoundaryTests(unittest.IsolatedAsyncioTestCase):
    def test_missing_access_policy_retains_custom_limits(self):
        self.assertFalse(simple_access({}))
        self.assertFalse(simple_access({"access_policy": "invalid"}))
        self.assertFalse(simple_access({"access_policy": "custom"}))
        self.assertTrue(simple_access({"access_policy": "simple"}))

    def test_array_scalar_and_empty_results_are_not_zero(self):
        for value in [[], [1, {"enabled": False}], None, False, 0, "", {"nextCursor": "next"}]:
            with self.subTest(value=value):
                self.assertEqual(unpack(mcp(value)), value)

    def test_game_update_preview_hides_runtime_inventory_and_read_receipt(self):
        plan = {
            "ready": True, "updateRequired": True, "desiredVersion": "26474880",
            "affectedRoomIds": ["cm9vbTE"], "blockers": [],
            "policy": {"stopDelaySeconds": 60, "skipBackup": False},
            "installations": [{
                "targetId": "local", "targetName": "本机", "installationId": "default",
                "steamBuild": "26335402", "desiredVersion": "26474880",
                "capabilities": ["disk.inspect", "runtime.console.v2"], "blockers": [],
                "shards": [
                    {"roomId": "cm9vbTE", "roomName": "测试服", "worldName": "Master", "wasRunning": True},
                    {"roomId": "cm9vbTE", "roomName": "测试服", "worldName": "Caves", "wasRunning": True},
                ],
            }],
        }
        from astrbot_plugin_dst_companion.generic_render import game_release_preview

        compact, shown = game_release_preview(plan)
        self.assertEqual(compact["worldCount"], 2)
        self.assertEqual(compact["runningWorldCount"], 2)
        self.assertIn("测试服", shown)
        self.assertIn("26335402 → 26474880", shown)
        self.assertNotIn("disk.inspect", shown)
        self.assertNotIn("cm9vbTE", shown)
        reply = GenericOperations.human_result({"state": "succeeded", "taskRef": "generic:read-receipt",
                                                "result": {"data": plan}})
        self.assertIn("本次仅检查", reply)
        self.assertNotIn("任务编号", reply)

    def test_failed_preview_displays_original_error_once(self):
        reply = GenericOperations.human_result({"state": "uncertain", "taskRef": "generic:sample",
                                                "result": {"error": {"code": "GAME_RELEASE_PREVIEW_TIMEOUT",
                                                                     "message": "版本检查超时"}}})
        self.assertIn("版本检查超时", reply)
        self.assertEqual(reply.count("任务编号"), 1)

    def test_game_update_submission_distinguishes_receipt_from_completion(self):
        from astrbot_plugin_dst_companion.generic_render import game_release_confirmation, game_release_submission

        prompt = game_release_confirmation({"body": {"confirmed": True, "policy": {
            "stopDelaySeconds": 0, "skipBackup": True, "restartRunning": False}}}, "确认")
        self.assertIn("立即停止", prompt)
        self.assertIn("按你的选择跳过", prompt)
        self.assertIn("保持世界停止", prompt)
        self.assertEqual(prompt.count("提交时会再次检查"), 1)

        accepted = game_release_submission({"state": "accepted", "taskRef": "generic:receipt",
            "result": {"httpStatus": 202, "data": {"id": "example-job", "status": "queued",
                "capabilities": ["runtime.game-update.v1"]}}})
        self.assertIn("任务已提交", accepted)
        self.assertIn("任务编号：example-job", accepted)
        self.assertNotIn("generic:receipt", accepted)
        self.assertNotIn("capabilities", accepted)
        self.assertNotIn("更新已完成", accepted)
        uncertain = game_release_submission({"state": "uncertain", "taskRef": "generic:receipt"})
        self.assertIn("不要重复提交", uncertain)
        self.assertIn("generic:receipt", uncertain)
        failed = game_release_submission({"state": "failed", "result": {"error": {"message": "磁盘空间不足"}}})
        self.assertIn("磁盘空间不足", failed)
        self.assertNotIn("任务已提交", failed)

    async def test_actor_cannot_be_replaced_and_denies_are_forwarded(self):
        native = AsyncMock(return_value=True)
        policy = Policy(native)
        who = replace(actor(), disabled_capabilities=("api:newCapability",))
        envelope = {"actor": actor_envelope(who), "mode": "read"}
        await policy.authorize(who, "companion_gateway", envelope)
        forged = copy.deepcopy(envelope)
        forged["actor"]["profile"] = "panel-manage"
        with self.assertRaises(QueryError):
            await policy.authorize(who, "companion_gateway", forged)
        with self.assertRaises(QueryError):
            await policy.authorize(who, "companion_gateway", {**envelope, "mode": "execute"})


@unittest.skipUnless(adapter.AVAILABLE, "requires real AstrBot")
class FrozenGenericAdapterTests(unittest.IsolatedAsyncioTestCase):
    def companion(self):
        bot = task_adapter.TaskAdapterTests.companion(self)
        bot.config["access_policy"] = "simple"
        bot.generic.available = AsyncMock(return_value=True)
        install_test_storage(bot)
        return bot

    def event(self, role="admin"):
        return task_adapter.TaskAdapterTests.event(self, role)

    async def test_frozen_plugin_accepts_new_global_machine_and_room_contracts(self):
        # Identifiers are created after the plugin object exists; no keyword,
        # category or operation map is modified for any of these capabilities.
        bot, event = self.companion(), self.event()
        for ident, selectors, parameters, room in [
            ("inspectAurora", {}, {}, ""),
            ("inspectHelios", {}, {"path": {"targetId": "machine-test"}}, ""),
            ("inspectCanopy", {"path.roomId": "room"}, {}, "r1"),
        ]:
            with self.subTest(ident=ident):
                card = contract(ident, selectors=selectors)
                async def request(who, mode, **kwargs):
                    if mode == "find":
                        return {"items": [copy.deepcopy(card)]}
                    self.assertEqual(kwargs["operationId"], ident)
                    if not room:
                        self.assertNotIn("roomId", kwargs["arguments"].get("path", {}))
                    return {"state": "succeeded", "result": {"httpStatus": 200, "data": [False, 0, None]}}
                bot.generic.request = AsyncMock(side_effect=request)
                found = json.loads(await bot.tasks.perform(event, "dst_find", query=ident))
                self.assertEqual(found["items"][0]["id"], "api:" + ident)
                result = json.loads(await bot.tasks.perform(event, "dst_read", capability_id="api:" + ident,
                                                           room=room, parameters=parameters))
                self.assertEqual(result["result"]["data"], [False, 0, None])
        bot.directory.items.assert_not_awaited()
        await bot.terminate()

    async def test_patch_passes_only_supplied_fields_and_no_model_request_id(self):
        bot, event = self.companion(), self.event()
        bot.input = lambda _: ("调整示例设置", True, "", None)
        card = contract("adjustAurora", "write")
        async def request(who, mode, **kwargs):
            if mode == "find":
                return {"items": [copy.deepcopy(card)]}
            self.assertEqual(kwargs["arguments"], {"body": {"enabled": False, "nullable": None, "choice": 0}})
            self.assertEqual(len(kwargs["requestId"]), 64)
            return {"state": "succeeded", "taskRef": "test-task", "result": {"httpStatus": 204}}
        bot.generic.request = AsyncMock(side_effect=request)
        result = json.loads(await bot.tasks.perform(event, "dst_run", capability_id=card["id"],
            parameters={"body": {"enabled": False, "nullable": None, "choice": 0}}))
        self.assertEqual(result["state"], "succeeded")
        rejected = await bot.tasks.perform(event, "dst_run", capability_id=card["id"], parameters={"requestId": "forged"})
        self.assertIn("PARAMETER_INVALID", rejected)
        await bot.terminate()

    async def test_human_confirmation_is_not_exposed_as_tool_argument(self):
        bot, event = self.companion(), self.event()
        bot.input = lambda _: ("运行合成维护任务", True, "", None)
        card = contract("maintainAurora", "write")
        async def request(who, mode, **kwargs):
            if mode == "find":
                return {"items": [copy.deepcopy(card)]}
            if mode == "run":
                return {"state": "needs_confirmation", "taskRef": "test-prepared", "confirmation": "destructive"}
            self.assertEqual(mode, "execute")
            return {"state": "accepted", "taskRef": "test-prepared"}
        bot.generic.request = AsyncMock(side_effect=request)
        result = json.loads(await bot.tasks.perform(event, "dst_run", capability_id=card["id"]))
        self.assertEqual(result["state"], "needs_confirmation")
        who = await bot.tasks.actor(event)
        self.assertIsNone(await bot.generic.continuation(who, "确认"))
        reply = await bot.generic.continuation(who, "确认执行")
        self.assertIn("仍在处理", reply)
        self.assertEqual(reply.count("任务编号"), 1)
        self.assertIsNone(await bot.generic.continuation(who, "确认执行"))
        await bot.terminate()

    async def test_game_update_preview_uses_read_tool_without_confirmation(self):
        bot, event = self.companion(), self.event()
        bot.input = lambda _: ("更新游戏", True, "", None)
        card = contract("previewGameRelease", "read")
        plan = {
            "ready": True, "updateRequired": True, "desiredVersion": "26474880",
            "affectedRoomIds": ["cm9vbTE"], "blockers": [],
            "installations": [{"targetId": "local", "targetName": "本机", "installationId": "default",
                               "steamBuild": "26335402", "desiredVersion": "26474880",
                               "capabilities": ["disk.inspect"], "shards": [], "blockers": []}],
        }

        async def request(_actor, mode, **_kwargs):
            if mode == "find":
                return {"items": [copy.deepcopy(card)]}
            self.assertEqual(mode, "read")
            return {"state": "succeeded", "taskRef": "read-receipt",
                    "result": {"httpStatus": 200, "data": plan}}

        bot.generic.request = AsyncMock(side_effect=request)
        result = json.loads(await bot.tasks.perform(event, "dst_read", capability_id=card["id"]))
        self.assertEqual(result["result"]["data"]["targetBuild"], "26474880")
        self.assertIn("本次仅检查", result["summary"])
        self.assertNotIn("disk.inspect", json.dumps(result))
        self.assertNotIn("taskRef", result)
        self.assertNotIn("callId", result)
        self.assertIsNone(bot.state.pending_form(bot.actor(event).key))
        await bot.terminate()

    async def test_game_update_confirmation_is_plain_and_submission_has_one_job_id(self):
        bot, event = self.companion(), self.event()
        bot.input = lambda _: ("更新游戏", True, "", None)
        preview_card = contract("previewGameRelease", "read")
        publish_card = contract("publishGameRelease", "write")
        plan = {"ready": True, "updateRequired": True, "desiredVersion": "26474880",
            "affectedRoomIds": ["example-room"], "blockers": [],
            "installations": [{"targetId": "local", "targetName": "本机", "installationId": "default",
                "steamBuild": "26335402", "desiredVersion": "26474880",
                "capabilities": ["runtime.game-update.v1"], "blockers": [],
                "shards": [{"roomId": "example-room", "roomName": "测试服", "worldName": "Master", "wasRunning": True},
                           {"roomId": "example-room", "roomName": "测试服", "worldName": "Caves", "wasRunning": True}]}]}
        modes = []

        async def request(_actor, mode, **kwargs):
            modes.append(mode)
            if mode == "find":
                return {"items": [copy.deepcopy(preview_card if "preview" in kwargs["query"] else publish_card)]}
            if mode == "read":
                return {"state": "succeeded", "result": {"httpStatus": 200, "data": plan}}
            if mode == "run":
                self.assertEqual(kwargs["arguments"], {"body": {"confirmed": True}})
                return {"state": "needs_confirmation", "taskRef": "prepared-release",
                    "confirmation": "confirm", "arguments": {"body": {"confirmed": True, "private": "do-not-show"}}}
            self.assertEqual(mode, "execute")
            return {"state": "accepted", "taskRef": "prepared-release",
                "result": {"httpStatus": 202, "data": {"id": "example-job", "status": "queued"}}}

        bot.generic.request = AsyncMock(side_effect=request)
        await bot.tasks.perform(event, "dst_read", capability_id=preview_card["id"])
        prepared = json.loads(await bot.tasks.perform(event, "dst_run", capability_id=publish_card["id"],
                                                       parameters={"body": {"confirmed": True}}))
        self.assertEqual(prepared["state"], "needs_confirmation")
        self.assertIn("1 个房间 · 2 个世界", prepared["summary"])
        self.assertIn("测试服", prepared["summary"])
        self.assertNotIn("do-not-show", json.dumps(prepared))
        self.assertNotIn("本次参数", prepared["summary"])
        self.assertNotIn("execute", modes)
        who = await bot.tasks.actor(event)
        answer = await bot.generic.continuation(who, "确认")
        self.assertEqual(modes.count("execute"), 1)
        self.assertIn("任务编号：example-job", answer)
        self.assertEqual(answer.count("任务编号"), 1)
        self.assertNotIn("prepared-release", answer)
        self.assertNotIn("capabilities", answer)
        self.assertNotIn("接口已执行", answer)
        self.assertIsNone(await bot.generic.continuation(who, "确认"))
        await bot.terminate()

    async def test_update_gateway_budget_exceeds_release_preflight_only(self):
        bot, event = self.companion(), self.event()
        who = bot.actor(event)
        bot.gateway.call = AsyncMock(return_value={"state": "succeeded"})
        before = time.monotonic()
        await bot.generic.request(who, "read", operationId="previewGameRelease", arguments={"body": {}})
        deadline = bot.gateway.call.call_args.args[3]
        self.assertGreater(deadline - before, 65)
        self.assertLess(deadline - before, 72)
        before = time.monotonic()
        await bot.generic.request(who, "read", operationId="getSoftwareStatus")
        deadline = bot.gateway.call.call_args.args[3]
        self.assertLess(deadline - before, 37)
        await bot.terminate()

    async def test_old_panel_keeps_legacy_directory_without_generic_call(self):
        bot, event = self.companion(), self.event()
        bot.generic.available.return_value = False
        bot.generic.request = AsyncMock()
        await bot.tasks.perform(event, "dst_find", query="复活")
        bot.generic.request.assert_not_awaited()
        bot.directory.items.assert_awaited()
        await bot.terminate()

    async def test_selected_workbench_recipe_never_loads_full_legacy_directory(self):
        from test_workbench import recipe

        bot, event = self.companion(), self.event()
        item = recipe("workbench.inspect_canopy", risk="low")
        bot.input = lambda _: ("运行合成工作台操作", True, "", None)
        bot.generic.request = AsyncMock(return_value={"items": [{
            "id": item["id"], "title": item["title"], "recipe": copy.deepcopy(item),
            "policy": {"access": "write", "confirmation": "none"},
        }]})
        # A full catalog is deliberately unavailable: its size must not gate
        # discovery or execution of one server-selected immutable recipe.
        bot.directory.items.side_effect = AssertionError("full legacy catalog must not be loaded")
        who = await bot.tasks.actor(event)
        selected = await bot.directory.get(who, item["id"], 0)
        self.assertEqual(selected, item)
        self.assertEqual(bot.generic.request.await_count, 1)
        await bot.directory.get(who, item["id"], 0)
        self.assertEqual(bot.generic.request.await_count, 1)
        self.assertIsNone(bot.directory.lookup(replace(who, disabled_capabilities=(item["id"],)), item["id"]))
        result = await bot.tasks.perform(event, "dst_run", capability_id=item["id"], room="r1",
                                         parameters={"player_id": "KU_SYNTHETIC"})
        self.assertIn("查询卡片", result)
        self.assertEqual(bot.engine.execute.call_args.args[1].capability, item["id"])
        bot.directory.items.assert_not_awaited()
        await bot.terminate()

    async def test_lost_response_recovers_after_adapter_reload_without_resending(self):
        bot, event = self.companion(), self.event()
        bot.input = lambda _: ("调整合成设置", True, "", None)
        card = contract("adjustForest", "write")
        modes = []
        async def request(who, mode, **kw):
            modes.append(mode)
            if mode == "find":
                return {"items": [copy.deepcopy(card)]}
            if mode == "run":
                raise QueryError("RESULT_UNCERTAIN")
            self.assertEqual(mode, "recover")
            return {"state": "succeeded", "taskRef": "original-task"}
        bot.generic.request = AsyncMock(side_effect=request)
        first = json.loads(await bot.tasks.perform(event, "dst_run", capability_id=card["id"]))
        self.assertEqual(first["state"], "uncertain")
        self.assertTrue(first["taskRef"].startswith("generic-request:"))
        blocked = json.loads(await bot.tasks.perform(event, "dst_run", capability_id="api:anotherMutation"))
        self.assertEqual(blocked["taskRef"], first["taskRef"])
        # Same durable KV, a new adapter object (simulated plugin reload).
        event = self.event()
        bot.generic = GenericOperations(bot)
        bot.generic.available = AsyncMock(return_value=True)
        bot.generic.request = AsyncMock(side_effect=request)
        second = json.loads(await bot.tasks.perform(event, "dst_run", capability_id=card["id"]))
        self.assertEqual(second["taskRef"], "generic:original-task")
        self.assertEqual(modes.count("run"), 1)
        recovered = json.loads(await bot.tasks.perform(event, "dst_result", task_ref=first["taskRef"]))
        self.assertEqual(recovered["state"], "succeeded")
        await bot.terminate()

    async def test_generic_player_prompt_continues_without_mention(self):
        bot, event = self.companion(), self.event()
        bot.input = lambda _: ("读取玩家合成数据", True, "", None)
        card = contract("inspectPlayerCanopy", selectors={"path.roomId": "room", "path.playerId": "player"})
        bot.engine.queries.resolve = AsyncMock(return_value={"status": "resolved", "items": [{"id": "KU_SYNTHETIC", "name": "示例玩家"}]})
        async def request(who, mode, **kw):
            if mode == "find":
                return {"items": [copy.deepcopy(card)]}
            self.assertEqual(kw["arguments"]["path"], {"roomId": "r1", "playerId": "KU_SYNTHETIC"})
            return {"state": "succeeded", "result": {"data": {"sample": 7}}}
        bot.generic.request = AsyncMock(side_effect=request)
        result = json.loads(await bot.tasks.perform(event, "dst_read", capability_id=card["id"]))
        self.assertEqual(result["state"], "needs_input")
        who = await bot.tasks.actor(event)
        form = bot.state.pending_form(who.key)
        self.assertTrue(bot.generic.accepts(form, "示例玩家", False, ""))
        self.assertFalse(bot.generic.accepts(form, "查看地图", True, ""))
        reply = await bot.generic.continuation(who, "示例玩家")
        self.assertIn("7", reply)
        self.assertIsNone(bot.state.pending_form(who.key))
        await bot.terminate()

    async def test_required_boolean_false_continuation_leaves_optional_defaults_absent(self):
        bot, event = self.companion(), self.event()
        bot.input = lambda _: ("设置合成开关", True, "", None)
        card = contract("switchCanopy", "write")
        card["parameters"]["required"] = ["body"]
        card["parameters"]["properties"]["body"]["required"] = ["enabled"]
        async def request(who, mode, **kw):
            if mode == "find":
                return {"items": [copy.deepcopy(card)]}
            self.assertEqual(kw["arguments"], {"body": {"enabled": False}})
            return {"state": "succeeded", "result": {"httpStatus": 204}}
        bot.generic.request = AsyncMock(side_effect=request)
        result = json.loads(await bot.tasks.perform(event, "dst_run", capability_id=card["id"]))
        self.assertEqual(result["state"], "needs_input")
        who = await bot.tasks.actor(event)
        await bot.generic.continuation(who, "关闭")
        self.assertEqual(bot.generic.request.call_args.args[1], "run")
        await bot.terminate()

    async def test_optional_player_filter_resolves_nickname_and_does_not_invent_a_target(self):
        bot, event = self.companion(), self.event()
        bot.input = lambda _: ("查看活动记录", True, "", None)
        card = contract("inspectOptionalPlayer", selectors={"path.roomId": "room", "query.userId": "player"})
        card["requiredSelectors"] = ["path.roomId"]
        bot.engine.queries.resolve = AsyncMock(return_value={"status": "resolved", "items": [{"id": "KU_SYNTHETIC", "name": "示例玩家"}]})
        async def request(who, mode, **kwargs):
            if mode == "find": return {"items": [copy.deepcopy(card)]}
            return {"state": "succeeded", "result": {"data": []}}
        bot.generic.request = AsyncMock(side_effect=request)
        first = json.loads(await bot.tasks.perform(event, "dst_read", capability_id=card["id"]))
        self.assertEqual(first["state"], "succeeded")
        bot.engine.queries.resolve.assert_not_awaited()
        self.assertFalse(bot.generic.request.call_args.kwargs["arguments"].get("query"))
        await bot.tasks.perform(event, "dst_read", capability_id=card["id"], player="示例玩家")
        self.assertEqual(bot.generic.request.call_args.kwargs["arguments"]["query"]["userId"], "KU_SYNTHETIC")
        # A fresh unspecified-player request must not reuse the focus just saved
        # above, or silently turn into an all-player activity query.
        bot.input = lambda _: ("帮我看看某位玩家最近在做什么", True, "", None)
        who = await bot.tasks.actor(event)
        args, question = await bot.generic.parameters(who, card, {})
        self.assertEqual(question["state"], "needs_input")
        self.assertEqual(question["field"], "query.userId")
        self.assertNotIn("userId", args.get("query", {}))
        # Even a valid player id chosen from a roster by the model is not the
        # user's choice when they deliberately left the target unspecified.
        _, refused = await bot.generic.parameters(who, card, {"player": "示例玩家", "parameters": {"query": {"userId": "KU_SYNTHETIC"}}})
        self.assertEqual(refused["state"], "needs_input")
        bot.state.form(who.key, {"type": "generic_input", "generic": True,
            "tool": "dst_read", "values": {"capability_id": card["id"], "parameters": args},
            "question": question, "projection": who.projection, "connectionId": who.connection_id})
        bot.input = lambda _: ("我说的是示例玩家", True, "", None)
        await bot.generic.continuation(who, "我说的是示例玩家")
        self.assertEqual(bot.engine.queries.resolve.call_args.args[2], "示例玩家")
        await bot.terminate()

    async def test_explicit_world_survives_model_placeholder_without_guessing_ambiguity(self):
        bot, event = self.companion(), self.event()
        await bot.prepare(event)
        who = bot.actor(event)
        card = contract("inspectWorld", selectors={"path.roomId": "room", "path.worldId": "world"})
        bot.input = lambda _: ("统计地面的实体", True, "", None)
        bot.gateway.call = AsyncMock(return_value={"items": [{"id": "m", "name": "Master", "role": "master"},
                                                          {"id": "c", "name": "Caves", "role": "caves"}]})
        args, question = await bot.generic.parameters(who, card, {"parameters": {"path": {"worldId": "?"}}})
        self.assertIsNone(question)
        self.assertEqual(args["path"]["worldId"], "m")
        bot.input = lambda _: ("比较地面和洞穴", True, "", None)
        _, question = await bot.generic.parameters(who, card, {"world": "?"})
        self.assertEqual(question["state"], "needs_input")
        await bot.terminate()

    async def test_new_protocol_projects_only_four_tools_and_preserves_foreign_tools(self):
        bot, event = self.companion(), self.event()
        bot.input = lambda _: ("帮我调整合成设置", True, "", None)
        from astrbot.core.agent.tool import FunctionTool, ToolSet
        names = ["dst_servers", "dst_player", "dst_world", "dst_find", "dst_read", "dst_run", "dst_result", "dst_player_action", "dst_server_action", "foreign_tool", "send_message_to_user"]
        request = NS(system_prompt="persona", func_tool=ToolSet([FunctionTool(name=n, description="fixture", parameters={"type":"object"}) for n in names]))
        await bot.on_model_request(event, request)
        self.assertEqual({t.name for t in request.func_tool}, {"dst_find", "dst_read", "dst_run", "dst_result", "foreign_tool"})
        self.assertTrue(request.system_prompt.startswith("persona"))
        self.assertNotIn("使用 dst_servers", request.system_prompt)
        await bot.terminate()

    async def test_generic_media_request_keeps_sender_for_image_delivery(self):
        bot, event = self.companion(), self.event()
        bot.input = lambda _: ("发一张地图图片", True, "", None)
        from astrbot.core.agent.tool import FunctionTool, ToolSet
        sender = FunctionTool(name="send_message_to_user", description="media", parameters={"type": "object"})
        request = NS(system_prompt="persona", func_tool=ToolSet([sender]))
        await bot.on_model_request(event, request)
        self.assertEqual(request.func_tool.names(), ["send_message_to_user"])
        await bot.terminate()

    async def test_dst_tool_step_does_not_publish_intermediate_text(self):
        bot, event = self.companion(), self.event()
        step = NS(role="assistant", tools_call_name=["dst_find"],
                  completion_text="我先看看更新接口", result_chain=None)
        await bot.on_model_response(event, step)
        self.assertEqual(step.completion_text, "")
        unrelated = NS(role="assistant", tools_call_name=["foreign_tool"],
                       completion_text="正在处理其他问题", result_chain=None)
        await bot.on_model_response(event, unrelated)
        self.assertEqual(unrelated.completion_text, "正在处理其他问题")
        await bot.terminate()

    async def test_image_validation_and_current_actor_are_checked_before_delivery(self):
        from PIL import Image
        bot, event = self.companion(), self.event()
        event.send = AsyncMock()
        await bot.prepare(event)
        who = bot.actor(event)
        bot.policy.authorize = AsyncMock()
        stream = io.BytesIO()
        Image.new("RGB", (8, 8)).save(stream, format="PNG")
        bot.private_panel.generic_resource = AsyncMock(return_value={"contentType":"image/png", "data": stream.getvalue()})
        data = json.loads(await bot.generic.resource(event, who, str(uuid.uuid4())))
        self.assertEqual(data["state"], "submitted")
        event.send.assert_awaited_once()
        self.assertEqual(bot.policy.authorize.await_count, 2)
        bot.private_panel.generic_resource.return_value["data"] = b"invalid-image"
        with self.assertRaises(QueryError) as failure:
            await bot.generic.resource(event, who, str(uuid.uuid4()))
        self.assertEqual(failure.exception.code, "RESOURCE_INVALID")
        self.assertEqual(event.send.await_count, 1)
        await bot.terminate()

    def test_file_cache_is_private_bounded_and_ignores_remote_paths(self):
        from astrbot_plugin_dst_companion.generic_files import cache_file
        from pathlib import Path
        with tempfile.TemporaryDirectory() as root:
            path, name = cache_file({"data": b"synthetic report", "name": "../../example.txt"}, root)
            self.assertEqual(path.parent, Path(root))
            self.assertEqual(name, "example.txt")
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            self.assertEqual(path.read_bytes(), b"synthetic report")
            with self.assertRaises(QueryError):
                cache_file({"data": b"x" * ((4 << 20)+1)}, root)
