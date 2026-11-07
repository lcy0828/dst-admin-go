import json
import unittest
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock

import test_astrbot_adapter as adapter


@unittest.skipUnless(adapter.AVAILABLE, "requires real AstrBot")
class TaskAdapterTests(unittest.IsolatedAsyncioTestCase):
    async def test_mistyped_result_reference_does_not_hide_original_uncertainty(self):
        from astrbot_plugin_dst_companion.gateway import QueryError
        bot, event = self.companion(), self.event("admin")
        who = await bot.tasks.actor(event)
        original = "结果尚未确认；请查询原任务 original-test-reference，不要重发。"
        event._dst_uncertain_write = (who.projection, str(event.message_obj.message_id), original)
        bot.tasks.result = AsyncMock(side_effect=QueryError("PERMISSION_DENIED"))
        result = await bot.tasks.perform(event, "dst_result", task_ref="mistyped-reference")
        self.assertEqual(result, original)
        bot.engine.execute.assert_not_awaited()
        await bot.terminate()

    async def test_world_read_discovery_and_explicit_user_shard_survive_model_omission(self):
        bot,event=self.companion(),self.event('admin')
        bot.config['public_queries'].append('world')
        await bot.prepare(event)
        bot.input=lambda event: ('读取洞穴的季节和昼夜',)
        item={'id':'world.world_info','title':'世界信息','category':'world','access':'read',
              'parameters':{'type':'object','properties':{'worldId':{'type':'string'}},'required':['worldId']},
              'parameterHints':{'worldId':'world'}}
        bot.directory.items.return_value=[item]
        found=json.loads(await bot.tasks.perform(event,'dst_find',query='读取洞穴世界的季节和昼夜信息'))
        self.assertEqual(found['items'][0]['id'],'world.world_info')
        bot.gateway.call=AsyncMock(return_value={'items':[{'id':'surface','name':'Master','role':'master'}, {'id':'underground','name':'Caves','role':'caves'}]})
        await bot.tasks.perform(event,'dst_world',room='r1',view='world_info')
        self.assertEqual(bot.engine.execute.call_args.args[1].parameters['worldId'],'underground')
        await bot.terminate()

    async def test_discovery_self_inventory_question_creates_actor_bound_continuation(self):
        bot,event=self.companion(),self.event('member')
        await bot.prepare(event)
        bot.input=lambda event: ('看看我的背包，QQ名不是我的游戏名',)
        reply=json.loads(await bot.tasks.perform(event,'dst_find',query='玩家背包'))
        self.assertEqual(reply['state'],'needs_input')
        actor=await bot.tasks.actor(event)
        form=bot.state.pending_form(actor.key)
        self.assertEqual(form['type'],'player')
        self.assertEqual(form['intent'].kind,'inventory')
        bot.engine.execute.assert_not_awaited()
        await bot.terminate()

    async def test_player_modes_require_group_grants_and_preserve_false(self):
        from astrbot_plugin_dst_companion.group_permissions import DEFAULT_RULES

        for action in ("god_mode", "creative_mode"):
            with self.subTest(action=action):
                bot, event = self.companion(), self.event("admin")
                bot.config["access_policy"] = "custom"
                item = {"id": "player." + action, "title": "玩家模式", "category": "management", "access": "write",
                        "parameters": {"type": "object", "properties": {"playerId": {"type": "string"}, "enabled": {"type": "boolean"}},
                                       "required": ["playerId", "enabled"], "additionalProperties": False},
                        "parameterHints": {"playerId": "player"}}
                bot.directory.items.return_value = [item]
                values = dict(room="r1", player="示例玩家", action=action, parameters={"enabled": False})
                denied = await bot.tasks.perform(event, "dst_player_action", **values)
                self.assertIn("PERMISSION_DENIED", denied)
                bot.engine.execute.assert_not_awaited()
                bot.config["group_permissions"] = {
                    "owner": {"write": DEFAULT_RULES["owner"]["write"] + [item["id"]]},
                    "admin": {"write": DEFAULT_RULES["admin"]["write"] + [item["id"]]}}
                result = await bot.tasks.perform(event, "dst_player_action", **values)
                self.assertIn("查询卡片", result)
                actor, intent, target = bot.engine.execute.call_args.args
                self.assertEqual(intent.parameters, {"playerId": "示例玩家", "enabled": False})
                self.assertEqual(intent.capability, item["id"])
                self.assertFalse(intent.confirm_required)  # R1: tier policy, same as direct commands
                self.assertEqual(target.room, "r1")
                bot.engine.execute.reset_mock()
                result = await bot.tasks.perform(event, "dst_player_action", **{**values, "parameters": {"enabled": "false"}})
                self.assertIn("PARAMETER_INVALID", result)
                bot.engine.execute.assert_not_awaited()
                bot.config["group_permissions"]["admin"]["deny"] = [item["id"]]
                result = await bot.tasks.perform(event, "dst_player_action", **values)
                self.assertIn("PERMISSION_DENIED", result)
                bot.engine.execute.assert_not_awaited()
                await bot.terminate()

    async def test_discovered_player_mode_works_through_generic_task_adapter(self):
        from astrbot_plugin_dst_companion.group_permissions import DEFAULT_RULES

        bot, event = self.companion(), self.event("owner")
        bot.config["group_permissions"] = {"owner": {"write": DEFAULT_RULES["owner"]["write"] + ["player.god_mode"]}}
        bot.directory.items.return_value = [{"id":"player.god_mode", "title":"无敌模式", "category":"management", "access":"write",
            "parameters":{"type":"object", "properties":{"playerId":{"type":"string"}, "enabled":{"type":"boolean"}},
                          "required":["playerId","enabled"], "additionalProperties":False}, "parameterHints":{"playerId":"player"}}]
        result = await bot.tasks.perform(event,"dst_run", capability_id="player.god_mode", room="r1", player="示例玩家", parameters={"enabled":True})
        self.assertIn("查询卡片",result)
        # Confirmation is decided by the capability tier in the engine, not by the entry point.
        self.assertFalse(bot.engine.execute.call_args.args[1].confirm_required)
        await bot.terminate()

    def companion(self):
        bot = adapter.AdapterTests.companion(self)
        bot.config["permission_mode"] = "group"
        bot.engine.queries.rooms = AsyncMock(return_value=[{"id": "r1", "name": "示例房间"}])
        bot.engine.execute = AsyncMock(return_value=NS(text="已返回查询卡片", target=None))
        bot.load_journal = AsyncMock(return_value={})
        bot.directory.items = AsyncMock(
            return_value=[
                {
                    "id": "player.resurrect",
                    "title": "复活玩家",
                    "category": "management",
                    "access": "write",
                    "parameters": {
                        "type": "object",
                        "properties": {"player_id": {"type": "string", "maxLength": 128}},
                        "additionalProperties": False,
                    },
                    "parameterHints": {"player_id": "player"},
                }
            ]
        )
        return bot

    def event(self, role="member"):
        event = adapter.AdapterTests.event(self, [])
        event.bot.call_action = AsyncMock(
            return_value={"user_id": 123, "group_id": 789, "role": role}
        )
        return event

    async def test_admin_nickname_and_typed_parameters_enter_shared_engine(self):
        bot, event = self.companion(), self.event("admin")
        text = await bot.tasks.perform(
            event,
            "dst_player_action",
            room="1服",
            player="示例玩家",
            action="resurrect",
            parameters={},
        )
        self.assertIn("查询卡片", text)
        actor, intent, target = bot.engine.execute.call_args.args
        self.assertEqual(actor.group_role, "admin")
        self.assertEqual(intent.parameters, {"player_id": "示例玩家"})
        self.assertEqual(target.room, "r1")
        self.assertFalse(intent.confirm_required)
        await bot.terminate()

    async def test_generic_model_resurrection_uses_same_no_confirmation_policy_on_older_panel(self):
        bot, event = self.companion(), self.event("admin")
        bot.directory.items.return_value[0]["risk"] = "high"
        await bot.tasks.perform(event, "dst_run", capability_id="player.resurrect", room="r1", player="示例玩家")
        intent = bot.engine.execute.call_args.args[1]
        self.assertEqual(intent.capability, "player.resurrect")
        self.assertEqual(intent.parameters, {"player_id": "示例玩家"})
        self.assertFalse(intent.confirm_required)
        await bot.terminate()

    async def test_model_actions_and_queries_use_same_verified_quote_selector(self):
        from astrbot_plugin_dst_companion.state import Target

        for tool, values in (("dst_player_action", {"action": "resurrect"}),
                             ("dst_run", {"capability_id": "player.resurrect"}),
                             ("dst_player", {"view": "inventory"})):
            with self.subTest(tool=tool):
                bot, event = self.companion(), self.event("admin")
                actor = await bot.tasks.actor(event)
                selected = Target("r1", "KU_TEST123", "示例玩家")
                bot.state.associate(actor.scope, "notice", selected, source=bot.connections.source())
                bot.input = lambda event: ("请对这个玩家操作", True, "notice", "")
                await bot.tasks.perform(event, tool, **values)
                self.assertEqual(bot.engine.execute.call_args.args[2], selected)
                if tool != "dst_player":
                    self.assertFalse(bot.engine.execute.call_args.args[1].confirm_required)
                await bot.terminate()

    async def test_unknown_model_quote_and_explicit_different_player_never_reuse_old_target(self):
        bot, event = self.companion(), self.event("admin")
        bot.input = lambda event: ("请复活这个玩家", True, "missing-notice", "")
        result = await bot.tasks.perform(event, "dst_player_action", action="resurrect")
        self.assertIn("未能确认引用中的玩家", result)
        bot.engine.execute.assert_not_awaited()
        # A clarification is latched to this model turn. A later user message
        # may provide an explicit target even when quoting an unrelated notice.
        del event._dst_task_question
        result = await bot.tasks.perform(event, "dst_player_action", action="resurrect", player="指定玩家")
        self.assertIn("查询卡片", result)
        self.assertEqual(bot.engine.execute.call_args.args[1].parameters, {"player_id": "指定玩家"})
        self.assertEqual(bot.engine.execute.call_args.args[2].player, "")
        await bot.terminate()

    async def test_no_confirmation_does_not_grant_resurrection_to_members_or_cross_room(self):
        bot, event = self.companion(), self.event("member")
        for values in ({"room": "r1"}, {"room": "other-room"}):
            with self.subTest(values=values):
                result = await bot.tasks.perform(event, "dst_player_action", action="resurrect", player="示例玩家", **values)
                self.assertNotIn("查询卡片", result)
                bot.engine.execute.assert_not_awaited()
        await bot.terminate()

    async def test_model_write_uses_the_same_tier_policy_as_direct_commands(self):
        bot, event = self.companion(), self.event("owner")
        bot.directory.items.return_value = [{"id": "room.save", "title": "保存世界", "category": "management",
            "access": "write", "risk": "low", "parameters": {"type": "object", "properties": {}, "additionalProperties": False},
            "parameterHints": {}}]
        await bot.tasks.perform(event, "dst_server_action", action="save", room="r1")
        self.assertFalse(bot.engine.execute.call_args.args[1].confirm_required)
        await bot.terminate()

    async def test_wrong_read_id_suggests_only_authorized_read_without_execution(self):
        bot, event = self.companion(), self.event("admin")
        bot.config["public_queries"].append("world")
        await bot.prepare(event)
        item = {"id": "world.world_info", "title": "世界信息", "category": "world", "access": "read",
                "parameters": {"type": "object", "properties": {"worldId": {"type": "string"}}, "required": ["worldId"]},
                "parameterHints": {"worldId": "world"}}
        bot.directory.items.return_value = [item]
        shown = json.loads(await bot.tasks.perform(event, "dst_read", capability_id="world_info", room="r1"))
        self.assertEqual(shown["code"], "CAPABILITY_ID_MISMATCH")
        self.assertEqual(shown["items"][0]["tool"], "dst_world")
        self.assertEqual(shown["items"][0]["input"], {"view": "world_info"})
        bot.engine.execute.assert_not_awaited()
        item["category"] = "management"
        item["access"] = "write"
        shown = json.loads(await bot.tasks.perform(event, "dst_read", capability_id="world_info", room="r1"))
        self.assertNotIn("items", shown)
        bot.engine.execute.assert_not_awaited()
        await bot.terminate()

    async def test_cross_world_evidence_keeps_points_when_world_names_fail(self):
        from astrbot_plugin_dst_companion.gateway import QueryError
        bot, event = self.companion(), self.event("admin")
        await bot.prepare(event)
        actor = bot.actor(event)
        evidence = {"capabilityId": "player.trajectory", "data": {"items": [
            {"worldId": "w1", "position": {"x": 10, "z": 20}},
            {"worldId": "w2", "position": {"x": 90, "z": 80}}]}}
        bot.gateway.call = AsyncMock(return_value={"items": [
            {"id": "w1", "name": "Master", "role": "master"},
            {"id": "w2", "name": "Caves", "role": "caves"}]})
        shown = json.loads(await bot.tasks.read_evidence(actor, "已读取", evidence, "r1"))
        self.assertEqual(shown["output"]["context"]["worlds"][1]["name"], "Caves")
        self.assertEqual(shown["result"], evidence)
        self.assertEqual(bot.gateway.call.call_args.args[2], {"path": {"roomId": "r1"}})
        bot.gateway.call.side_effect = QueryError("QUERY_TIMEOUT")
        shown = json.loads(await bot.tasks.read_evidence(actor, "已读取", evidence, "r1"))
        self.assertTrue(shown["output"]["context"]["worldNamesUnavailable"])
        self.assertEqual(shown["result"], evidence)
        await bot.terminate()

    async def test_model_cannot_choose_ambiguous_world_by_guessing_an_id(self):
        bot, event = self.companion(), self.event("admin")
        bot.config["public_queries"].append("world")
        event.get_messages = lambda: [adapter.C.At(qq="456"), adapter.C.Plain("读取洞穴蜘蛛巢数量")]
        await bot.prepare(event)
        bot.directory.items.return_value = [{"id": "world.entity_count", "title": "实体数量", "category": "world", "access": "read",
            "parameters": {"type": "object", "properties": {"worldId": {"type": "string"}, "prefab": {"type": "string"}}, "required": ["worldId", "prefab"], "additionalProperties": False},
            "parameterHints": {"worldId": "world"}}]
        bot.gateway.call = AsyncMock(return_value={"items": [
            {"id": "c1", "name": "Caves", "role": "caves"},
            {"id": "c2", "name": "Caves", "role": "caves"}]})
        for values in ({"world": "c1"}, {"world": "c2"}, {"parameters": {"worldId": "c1", "prefab": "spiderden"}}):
            result = json.loads(await bot.tasks.perform(event, "dst_world", room="r1", view="entity_count",
                **({"parameters": {"prefab": "spiderden"}} | values)))
            self.assertEqual(result["state"], "needs_input")
            self.assertEqual(len(result["choices"]), 2)
        bot.engine.execute.assert_not_awaited()
        await bot.terminate()

    async def test_invalid_read_parameter_returns_schema_without_execution(self):
        bot, event = self.companion(), self.event("admin")
        bot.config["public_queries"].append("world")
        await bot.prepare(event)
        bot.directory.items.return_value = [{"id": "world.entity_count", "title": "实体数量", "category": "world", "access": "read",
            "parameters": {"type": "object", "properties": {"worldId": {"type": "string"}, "prefab": {"type": "string"}}, "required": ["worldId", "prefab"], "additionalProperties": False},
            "parameterHints": {"worldId": "world"}}]
        bot.gateway.call = AsyncMock(return_value={"items": [{"id": "m", "name": "Master", "role": "master"}]})
        values = {"room": "r1", "world": "Master", "view": "entity_count"}
        result = json.loads(await bot.tasks.perform(event, "dst_world", **values, parameters={"prefab": "spiderden", "count": True}))
        self.assertEqual(result["code"], "PARAMETER_INVALID")
        self.assertNotIn("count", result["items"][0]["parameters"]["properties"])
        bot.engine.execute.assert_not_awaited()
        await bot.tasks.perform(event, "dst_world", **values, parameters={"prefab": "spiderden"})
        bot.engine.execute.assert_awaited_once()
        await bot.terminate()

    async def test_my_inventory_cannot_be_assigned_to_the_only_online_player(self):
        bot, event = self.companion(), self.event("admin")
        event.get_messages = lambda: [adapter.C.At(qq="456"), adapter.C.Plain("看看我的背包，QQ名不是我的游戏名")]
        for tool, values in (("dst_player", {"view": "inventory", "player": "示例玩家"}),
                             ("dst_read", {"capability_id": "player.inventory", "parameters": {"playerId": "KU_EXAMPLE"}})):
            shown = json.loads(await bot.tasks.perform(event, tool, **values))
            self.assertEqual(shown["state"], "needs_input")
            self.assertIn("游戏昵称", shown["summary"])
        bot.engine.execute.assert_not_awaited()
        await bot.terminate()

    async def test_self_identity_does_not_treat_qq_label_as_a_supplied_game_name(self):
        bot, event = self.companion(), self.event("admin")
        event.get_messages = lambda: [adapter.C.At(qq="456"), adapter.C.Plain("看看我的背包，QQ名不是我的游戏名")]
        shown = json.loads(await bot.tasks.perform(event, "dst_player", view="profile", player="QQ名"))
        self.assertEqual(shown["state"], "needs_input")
        form = bot.state.pending_form(bot.actor(event).key)
        self.assertEqual(form["intent"].kind, "inventory")
        bot.engine.execute.assert_not_awaited()
        await bot.terminate()

    async def test_self_query_accepts_an_explicitly_named_game_identity(self):
        bot, event = self.companion(), self.event("admin")
        event.get_messages = lambda: [adapter.C.At(qq="456"), adapter.C.Plain("看看我的背包，我的游戏名是示例玩家")]
        await bot.tasks.perform(event, "dst_player", view="inventory", player="示例玩家")
        bot.engine.execute.assert_awaited_once()
        self.assertEqual(bot.engine.execute.call_args.args[1].player, "示例玩家")
        await bot.terminate()

    async def test_member_or_cross_room_or_read_wrapper_cannot_execute_write(self):
        for role, room, tool in (
            ("member", "r1", "dst_run"),
            ("admin", "other", "dst_run"),
            ("admin", "r1", "dst_read"),
        ):
            bot, event = self.companion(), self.event(role)
            result = await bot.tasks.perform(
                event, tool, room=room, capability_id="player.resurrect", parameters={}
            )
            self.assertIn(json.loads(result)["state"], {"unavailable", "needs_input"})
            bot.engine.execute.assert_not_awaited()
            await bot.terminate()

    async def test_ambiguous_room_is_not_chosen_and_strings_are_not_parameter_objects(self):
        bot, event = self.companion(), self.event("admin")
        bot.engine.queries.rooms.return_value = [
            {"id": "r1", "name": "相同名字"},
            {"id": "r2", "name": "相同名字"},
        ]
        for values in (
            {"room": "相同名字", "parameters": {}},
            {"room": "r1", "parameters": '{"count":3}'},
        ):
            result = await bot.tasks.perform(
                event, "dst_player_action", action="resurrect", player="测试", **values
            )
            self.assertIn(json.loads(result)["state"], {"unavailable", "needs_input"})
        bot.engine.execute.assert_not_awaited()
        await bot.terminate()

    async def test_ambiguous_user_room_is_not_resolved_by_model_omission_or_guess(self):
        bot, event = self.companion(), self.event("admin")
        event.get_messages = lambda: [adapter.C.At(qq="456"), adapter.C.Plain("双子房间谁在线，请让我选")]
        bot.engine.queries.rooms.return_value = [{"id": "r1", "name": "双子"}, {"id": "r2", "name": "双子"}]
        for guessed in ("", "r1", "r2"):
            result = json.loads(await bot.tasks.perform(event, "dst_servers", view="players", room=guessed))
            self.assertEqual(result["state"], "needs_input")
            self.assertEqual(len(result["choices"]), 2)
        bot.engine.execute.assert_not_awaited()
        await bot.terminate()

    async def test_search_filters_permissions_and_is_bounded(self):
        bot, event = self.companion(), self.event()
        result = json.loads(await bot.tasks.perform(event, "dst_find", query="复活"))
        self.assertEqual(result["items"], [])
        bot.group_roles.rows.clear()
        event.bot.call_action.return_value["role"] = "admin"
        result = json.loads(await bot.tasks.perform(event, "dst_find", query="复活"))
        self.assertEqual(result["items"][0]["id"], "player.resurrect")
        self.assertEqual(result["items"][0]["tool"], "dst_player_action")
        self.assertEqual(result["items"][0]["input"], {"action": "resurrect"})
        await bot.terminate()

    async def test_dynamic_read_keeps_structured_records_and_coverage(self):
        bot, event = self.companion(), self.event()
        bot.directory.items.return_value = [
            {
                "id": "player.activity",
                "title": "玩家活动",
                "category": "profile",
                "access": "read",
                "parameters": {"type": "object", "properties": {"playerId": {"type": "string", "maxLength": 128}}, "additionalProperties": False},
                "parameterHints": {"playerId": "player"},
            }
        ]
        receipt = {
            "status": "succeeded",
            "complete": False,
            "capabilityId": "player.activity",
            "data": {
                "items": [{"kind": "harvest", "prefab": "sapling"}],
                "coverage": {"complete": False, "gaps": [{"reason": "SEGMENT_PENDING"}]},
            },
        }
        bot.engine.execute.return_value = NS(text="活动摘要", target=None, data=receipt)
        result = json.loads(
            await bot.tasks.perform(event, "dst_player", view="activity", player="示例玩家")
        )
        self.assertEqual(result["result"], receipt)
        self.assertFalse(result["output"]["truncated"])
        self.assertEqual(bot.engine.execute.await_count, 1)
        await bot.terminate()

    async def test_actual_astrbot_schema_uses_object_not_json_string(self):
        from astrbot.core.provider.register import llm_tools

        manager = llm_tools
        tools = [t for t in manager.func_list if t.name == "dst_read"]
        if not tools:
            self.fail("task tool not registered with AstrBot")
        schema = tools[0].parameters
        self.assertEqual(schema["properties"]["parameters"]["type"], "object")
        self.assertNotIn("role", schema["properties"])

    async def test_read_request_cannot_be_turned_into_player_write_by_model(self):
        bot, event = self.companion(), self.event("admin")
        event.get_messages = lambda: [adapter.C.At(qq="456"), adapter.C.Plain("AI: 现在谁在线呀")]
        result = json.loads(await bot.tasks.perform(event, "dst_player_action", action="resurrect", player="测试玩家"))
        self.assertEqual(result["code"], "READ_REQUEST_ONLY")
        bot.engine.execute.assert_not_awaited()
        await bot.terminate()

    async def test_old_model_tool_call_is_stopped_after_new_request(self):
        bot, event = self.companion(), self.event("admin")
        await bot.prepare(event)
        bot.state.claim_request(bot.actor(event).key, "newer-message")
        result = json.loads(await bot.tasks.perform(event, "dst_player", view="profile"))
        self.assertEqual(result["state"], "superseded")
        bot.engine.execute.assert_not_awaited()
        await bot.terminate()

    async def test_request_tool_schema_has_available_views_and_keeps_permission_guard(self):
        from astrbot_plugin_dst_companion.task_tools import tool_view
        bot, event = self.companion(), self.event("admin")
        bot.config["access_policy"] = "simple"
        await bot.prepare(event)
        actor = bot.actor(event)
        original = adapter.FunctionTool(name="dst_servers", description="fixture", parameters={"type":"object", "properties":{"view":{"type":"string"}}})
        guarded = adapter._PermissionGuardedTool(original, bot.manager)
        shown = tool_view(guarded, actor)
        self.assertIsInstance(shown, adapter._PermissionGuardedTool)
        self.assertIs(shown._wrapped, original)
        # Simple policy: every read view is visible to everyone.
        self.assertEqual(shown.parameters["properties"]["view"]["enum"], ["overview", "players", "mods", "performance"])
        self.assertNotIn("enum", original.parameters["properties"]["view"])
        hidden = adapter.FunctionTool(name="dst_world", description="fixture", parameters={"type":"object"})
        self.assertIsNotNone(tool_view(hidden, actor))
        bot.config["access_policy"] = "custom"
        await bot.prepare(event)
        custom = bot.actor(event)
        self.assertEqual(tool_view(guarded, custom).parameters["properties"]["view"]["enum"], ["overview", "players", "mods"])
        self.assertIsNone(tool_view(hidden, custom))
        await bot.terminate()


class ResultBudgetTests(unittest.TestCase):
    def test_combat_summary_distinguishes_zero_kills_and_unknown_damage(self):
        from astrbot_plugin_dst_companion.task_results import read_result
        original = {"capabilityId": "player.combat", "data": {"items": [
            {"direction": "outgoing", "kills": 0, "unknownDamageHits": 3}]}}
        shown = json.loads(read_result("战斗记录", original))["output"]["combatEvidence"]
        self.assertEqual(shown["kills"], 0)
        self.assertIn("没有击杀", shown["killConclusion"])
        self.assertIsNone(shown["outgoingDamage"])
        self.assertEqual(shown["unknownDamageHits"], 3)
        original["data"]["items"][0].pop("kills")
        shown = json.loads(read_result("战斗记录", original))["output"]["combatEvidence"]
        self.assertIsNone(shown["kills"])
        self.assertIn("未确认", shown["killConclusion"])

    def test_event_time_is_converted_without_using_query_range_or_text(self):
        from astrbot_plugin_dst_companion.task_results import read_result
        original = {"data": {"from": "2026-01-01T02:00:00Z", "items": [
            {"occurredAt": "2026-01-01T03:37:26Z", "text": "2026-01-01T02:00:00Z"},
            {"occurredAt": "2026-01-01T03:37:26"}]}}
        shown = json.loads(read_result("已读取", original))
        self.assertEqual(shown["result"], original)
        self.assertEqual(shown["output"]["timeEvidence"], [{
            "path": "result.data.items[0].occurredAt", "meaning": "事件发生",
            "beijing": "2026-01-01 11:37:26"}])

    def test_discovery_lifts_player_and_world_out_of_native_parameters(self):
        from astrbot_plugin_dst_companion.task_tools import discover_item
        item = {"id": "world.entity_count", "title": "实体数量", "access": "read",
                "parameters": {"type": "object", "properties": {"worldId": {"type": "string"}, "prefab": {"type": "string"}}, "required": ["worldId", "prefab"]},
                "parameterHints": {"worldId": "world"}}
        shown = discover_item(item)
        self.assertEqual(shown["parameters"]["required"], ["prefab"])
        self.assertNotIn("worldId", shown["parameters"]["properties"])
        self.assertTrue(shown["selectors"]["world"]["required"])
        self.assertEqual(item["parameters"]["required"], ["worldId", "prefab"])

    def test_large_records_keep_evidence_and_make_truncation_explicit(self):
        from astrbot_plugin_dst_companion.task_results import MAX_RESULT_BYTES, read_result

        original = {
            "status": "succeeded",
            "complete": False,
            "observedAt": "2026-11-01T00:00:00Z",
            "data": {
                "items": [{"text": "测试" * 2000} for _ in range(300)],
                "coverage": {"complete": False, "gaps": [{"reason": "SEGMENT_PENDING"}]},
                "nextCursor": "fixture-cursor" * 64,
            },
        }
        raw = read_result("活动摘要", original)
        result = json.loads(raw)
        self.assertLessEqual(len(raw.encode()), MAX_RESULT_BYTES)
        self.assertTrue(result["output"]["truncated"])
        self.assertEqual(result["result"]["data"]["coverage"], original["data"]["coverage"])
        self.assertEqual(result["result"]["data"]["nextCursor"], "fixture-cursor" * 64)
        self.assertEqual(len(original["data"]["items"]), 300)
