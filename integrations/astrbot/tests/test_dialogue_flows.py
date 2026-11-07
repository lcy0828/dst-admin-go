"""Full AstrBot conversation flows; replace external I/O, retain the engine.

These are simulated group conversations, not real QQ end-to-end acceptance.
"""

import copy
import importlib.util
import unittest

from test_companion import mcp
from test_v41 import capability
from test_workbench import recipe

from astrbot_plugin_dst_companion.routing import parse

AVAILABLE = importlib.util.find_spec("astrbot") is not None
if AVAILABLE:
    import test_qq_pipeline as pipeline
    from astrbot.api import message_components as C


@unittest.skipUnless(AVAILABLE, "requires installed AstrBot framework")
class DialogueFlowTests(unittest.IsolatedAsyncioTestCase):
    if AVAILABLE:
        asyncSetUp = pipeline.QQPipelineTests.asyncSetUp
        asyncTearDown = pipeline.QQPipelineTests.asyncTearDown
        event = pipeline.QQPipelineTests.event
        dispatch = pipeline.QQPipelineTests.dispatch

    async def setup_panel(self):
        self.ctx.astrbot_config["admins_id"] = ["123"]
        self.bot.config.update(configuration_version=2, defaults={"rooms": ["r1"],
            "writeAllowlist": ["player.resurrect", "player.give_item", "player.god_mode"],
            "workbenchMode": "standard"})
        grant = capability("player.give_item", "write", {"type": "object", "properties": {
            "playerId": {"type": "string", "maxLength": 128}, "item": {"type": "string", "maxLength": 128},
            "count": {"type": "integer", "minimum": 1, "maximum": 40}},
            "required": ["playerId", "item", "count"], "additionalProperties": False})
        grant.update(parameterHints={"playerId": "player"}, resultKind="item-grant", risk="high", title="发物品")
        god = capability("player.god_mode", "write", {"type": "object", "properties": {
            "playerId": {"type": "string", "maxLength": 128}, "enabled": {"type": "boolean"}},
            "required": ["playerId", "enabled"], "additionalProperties": False})
        god.update(parameterHints={"playerId": "player"}, resultKind="player-action", risk="high", title="无敌模式")
        self.items = [recipe(risk="low"), grant, god]
        self.sent, self.reads, self.journal = [], [], {}
        self.player_status = "resolved"

        async def load():
            return copy.deepcopy(self.journal)

        async def save(value):
            self.journal = copy.deepcopy(value)

        self.bot.engine.actions.load, self.bot.engine.actions.save = load, save

        async def transport(name, args, timeout, write):
            self.reads.append((name, copy.deepcopy(args)))
            if name == "api_listBotCapabilities":
                return mcp({"protocolVersion": 1, "revision": "dialogue-v1", "items": self.items, "nextCursor": ""})
            if name == "api_resolveRoomPlayer":
                query = args["query"]["query"]
                rows = [{"id": "KU_DEMO123", "name": "示例玩家", "online": True, "presenceStatus": "live"}]
                if query in {"示例玩家", "KU_DEMO123"}:
                    if self.player_status == "ambiguous":
                        return mcp({"status": "ambiguous", "items": rows + [{"id": "KU_OTHER123", "name": "示例玩家二"}]})
                    return mcp({"status": self.player_status, "match": "exact_name", "items": rows if self.player_status == "resolved" else []})
                return mcp({"status": "not_found", "items": []})
            if name == "api_runBotCapability":
                self.sent.append(copy.deepcopy(args))
                return mcp({"status": "succeeded", "complete": True, "requestId": args["requestId"],
                    "capabilityId": args["path"]["capabilityId"], "roomId": "r1", "resultKind": "workbench-run",
                    "data": {"output": {"text": "[DST-ADMIN-RESURRECT] REQUESTED", "truncated": False}}})
            return mcp(await self.panel.call(None, name, args, 0))

        self.bot.gateway.transport = transport

    def request(self, text, *, user="123", mention=True, quote=""):
        parts = ([C.Reply(id=quote, sender_id="456", message_str="通知模板可自定义")] if quote else [])
        if mention:
            parts.append(C.At(qq="456"))
        return self.event([*parts, C.Plain(text)], user=user)

    async def test_prompt_accepts_same_users_plain_nickname_and_executes_only_once(self):
        await self.setup_panel()
        output = await self.dispatch(self.request("复活"))
        self.assertTrue(output)
        self.assertFalse(self.sent)
        await self.dispatch(self.request("示例玩家", user="999", mention=False))
        self.assertFalse(self.sent)
        output = await self.dispatch(self.request("示例玩家", mention=False))
        self.assertTrue(output, "same user's nickname reply must not silently disappear")
        self.assertEqual(len(self.sent), 1)
        self.assertEqual(self.sent[0]["body"]["parameters"], {"player_id": "KU_DEMO123"})
        self.assertEqual(self.models, [])

    async def test_revival_politeness_is_not_part_of_the_nickname(self):
        await self.setup_panel()
        await self.dispatch(self.request("复活一下示例玩家"))
        self.assertEqual(len(self.sent), 1)
        self.assertEqual(self.sent[0]["body"]["parameters"]["player_id"], "KU_DEMO123")
        self.assertEqual(self.models, [])

    async def test_quoted_targetless_grant_uses_real_engine_and_unique_identity(self):
        from astrbot_plugin_dst_companion.state import Target

        await self.setup_panel()
        event = self.request("发五个草", quote="notice")
        await self.bot.prepare(event)
        self.bot.state.associate(self.bot.actor(event).scope, "notice", Target("r1", "KU_DEMO123", "示例玩家"),
                                 source=self.bot.connections.source())
        await self.dispatch(event)
        self.assertEqual(len(self.sent), 1)
        self.assertEqual(self.sent[0]["body"]["parameters"], {"playerId": "KU_DEMO123", "item": "草", "count": 5})
        self.assertEqual(self.models, [])

    async def test_native_boolean_answer_is_understood_without_model_or_extra_confirmation(self):
        await self.setup_panel()
        await self.dispatch(self.request("功能 player.god_mode {\"playerId\":\"示例玩家\"}"))
        self.assertFalse(self.sent)
        output = await self.dispatch(self.request("开启"))
        self.assertEqual(len(self.sent), 1, output)
        self.assertIs(self.sent[0]["body"]["parameters"]["enabled"], True)
        self.assertEqual(self.models, [])

    async def test_unavailable_directory_retains_player_prompt_for_corrected_name(self):
        await self.setup_panel()
        await self.dispatch(self.request("复活"))
        output = await self.dispatch(self.request("昵称写错了", mention=False))
        self.assertTrue(output)
        self.assertFalse(self.sent)
        output = await self.dispatch(self.request("示例玩家", mention=False))
        self.assertTrue(output)
        self.assertEqual(len(self.sent), 1)

    async def test_wrong_name_does_not_lose_item_or_quantity_when_corrected(self):
        await self.setup_panel()
        await self.dispatch(self.request("发五个草"))
        await self.dispatch(self.request("错别字", mention=False))
        self.assertFalse(self.sent)
        await self.dispatch(self.request("示例玩家", mention=False))
        self.assertEqual(len(self.sent), 1)
        self.assertEqual(self.sent[0]["body"]["parameters"], {"playerId": "KU_DEMO123", "item": "草", "count": 5})
        self.assertEqual(self.models, [])

    async def test_reported_player_descriptor_forms_resolve_nickname_before_submission(self):
        await self.setup_panel()
        for message in ("给玩家示例玩家 发五个草", "给玩家示例玩家发五个草"):
            with self.subTest(message=message):
                before = len(self.sent)
                await self.dispatch(self.request(message))
                self.assertEqual(len(self.sent), before + 1)
                self.assertEqual(self.sent[-1]["body"]["parameters"],
                                 {"playerId": "KU_DEMO123", "item": "草", "count": 5})
        self.assertEqual(self.models, [])

    async def test_unverified_quote_cannot_reuse_previous_player_for_management(self):
        await self.setup_panel()
        await self.dispatch(self.request("复活示例玩家"))
        self.sent.clear()
        output = await self.dispatch(self.request("复活", mention=False, quote="unverified-notice"))
        self.assertIn("未能确认引用中的玩家", output[-1])
        self.assertFalse(self.sent)
        self.assertEqual(self.models, [])

    async def test_explicit_nickname_wins_over_a_different_verified_player_quote(self):
        from astrbot_plugin_dst_companion.state import Target

        await self.setup_panel()
        event = self.request("复活示例玩家", quote="notice")
        await self.bot.prepare(event)
        self.bot.state.associate(self.bot.actor(event).scope, "notice", Target("r1", "KU_OTHER123", "另一位玩家"),
                                 source=self.bot.connections.source())
        await self.dispatch(event)
        self.assertEqual(len(self.sent), 1)
        self.assertEqual(self.sent[0]["body"]["parameters"]["player_id"], "KU_DEMO123")
        self.assertEqual(self.models, [])

    async def test_dangerous_action_still_requires_current_users_confirmation(self):
        from astrbot_plugin_dst_companion.state import Target

        await self.setup_panel()
        item = recipe("workbench.clear_player_inventory", "critical")
        item["title"] = "清空背包"
        self.items.append(item)
        self.bot.config["defaults"]["writeAllowlist"].append(item["id"])
        output = await self.dispatch(self.request('功能 workbench.clear_player_inventory {"player_id":"示例玩家"}'))
        self.assertIn("确认", output[-1])
        self.assertFalse(self.sent)
        event = self.request("确认", mention=False, quote="other-notice")
        await self.bot.prepare(event)
        self.bot.state.associate(self.bot.actor(event).scope, "other-notice", Target("r1", "KU_OTHER123", "另一位玩家"),
                                 source=self.bot.connections.source())
        await self.dispatch(event)
        self.assertFalse(self.sent, "a different player's quote must not confirm or change a prepared operation")
        await self.dispatch(self.request("确认清空", mention=False))
        self.assertEqual(len(self.sent), 1)
        self.assertEqual(self.sent[0]["body"]["parameters"], {"player_id": "KU_DEMO123", "confirmed": True})
        self.assertEqual(self.models, [])

    async def test_reply_to_our_notice_without_at_can_select_a_player(self):
        from astrbot_plugin_dst_companion.state import Target

        await self.setup_panel()
        event = self.request("复活", mention=False, quote="notice")
        await self.bot.prepare(event)
        self.bot.state.associate(self.bot.actor(event).scope, "notice", Target("r1", "KU_DEMO123", "示例玩家"),
                                 source=self.bot.connections.source())
        await self.dispatch(event)
        self.assertEqual(len(self.sent), 1)
        self.assertEqual(self.models, [])

    async def test_pending_player_prompt_accepts_another_quoted_notice_without_at(self):
        from astrbot_plugin_dst_companion.state import Target

        await self.setup_panel()
        await self.dispatch(self.request("发五个草"))
        await self.dispatch(self.request("错别字", mention=False))
        event = self.request("就是他", mention=False, quote="notice")
        await self.bot.prepare(event)
        self.bot.state.associate(self.bot.actor(event).scope, "notice", Target("r1", "KU_DEMO123", "示例玩家"),
                                 source=self.bot.connections.source())
        await self.dispatch(event)
        self.assertEqual(len(self.sent), 1)
        self.assertEqual(self.sent[0]["body"]["parameters"]["playerId"], "KU_DEMO123")
        self.assertEqual(self.models, [])

    async def test_expiry_and_permission_revocation_block_plain_player_answers(self):
        for variant in ("expired", "revoked"):
            with self.subTest(variant=variant):
                await self.setup_panel()
                event = self.request("复活")
                await self.dispatch(event)
                if variant == "expired":
                    self.bot.state.forms[self.bot.actor(event).key]["until"] = 0
                else:
                    self.ctx.astrbot_config["admins_id"] = []
                await self.dispatch(self.request("示例玩家", mention=False))
                self.assertFalse(self.sent)

    async def test_new_read_request_and_cancel_retire_the_pending_write(self):
        await self.setup_panel()
        for text in ("谁在线", "取消"):
            await self.dispatch(self.request("复活"))
            await self.dispatch(self.request(text))
            await self.dispatch(self.request("示例玩家", mention=False))
            self.assertFalse(self.sent)
        self.assertEqual(self.models, [])

    async def test_ambiguous_player_requires_choice_then_uses_selected_identity_once(self):
        await self.setup_panel()
        self.player_status = "ambiguous"
        output = await self.dispatch(self.request("复活示例玩家"))
        self.assertIn("请确认玩家", output[-1])
        self.assertFalse(self.sent)
        await self.dispatch(self.request("2", mention=False))
        self.assertEqual(len(self.sent), 1)
        self.assertEqual(self.sent[0]["body"]["parameters"], {"player_id": "KU_OTHER123"})
        await self.dispatch(self.request("2", mention=False))
        self.assertEqual(len(self.sent), 1)
        self.assertEqual(self.models, [])

    async def test_out_of_range_natural_quantity_can_be_corrected_in_same_form(self):
        await self.setup_panel()
        output = await self.dispatch(self.request('功能 player.give_item {"playerId":"示例玩家","item":"草"}'))
        self.assertIn("数量", output[-1])
        await self.dispatch(self.request("五十个"))
        self.assertFalse(self.sent)
        await self.dispatch(self.request("五个"))
        self.assertEqual(len(self.sent), 1)
        self.assertEqual(self.sent[0]["body"]["parameters"]["count"], 5)
        self.assertEqual(self.models, [])

    async def test_unavailable_directory_does_not_claim_user_misspelled_the_name(self):
        await self.setup_panel()
        self.player_status = "unavailable"
        output = await self.dispatch(self.request("复活示例玩家"))
        self.assertIn("暂时无法读取", output[-1])
        self.assertFalse(self.sent)
        self.player_status = "resolved"
        await self.dispatch(self.request("示例玩家", mention=False))
        self.assertEqual(len(self.sent), 1)


class EverydayGrammarTests(unittest.TestCase):
    def test_confirmation_and_switch_prompts_show_human_labels(self):
        from astrbot_plugin_dst_companion.parameter_ui import describe, prompt
        from astrbot_plugin_dst_companion.state import Target

        item = {"parameterHints": {"playerId": "player"}, "parameters": {"properties": {
            "enabled": {"type": "boolean"}}}}
        details = describe(item, {"playerId": "KU_DEMO123", "enabled": False, "feedback": "panel_detail", "confirmed": True},
                           Target("r1", "KU_DEMO123", "示例玩家"))
        self.assertEqual(details, "玩家：示例玩家\n开关：关闭")
        self.assertIn("开启", prompt(item, "enabled", {"type": "boolean"}))
        self.assertNotIn("boolean", prompt(item, "enabled", {"type": "boolean"}))

    def test_explicit_grant_orders_and_pronouns(self):
        cases = [("发五个草给示例玩家", "示例玩家"), ("给他五个草", "他"),
                 ("送5个草给这个玩家", "这个玩家"), ("发五个草", ""), ("再给她5个草", "她")]
        for text, player in cases:
            with self.subTest(text=text):
                intent = parse(text, {})
                self.assertIsNotNone(intent)
                self.assertEqual(intent.capability, "player.give_item")
                self.assertEqual(intent.parameters.get("playerId", ""), player)
                self.assertEqual(intent.parameters["count"], 5)

    def test_native_mode_orders_have_explicit_true_or_false(self):
        cases = [("给示例玩家开无敌", True), ("关闭示例玩家的无敌模式", False),
                 ("给他开启制作模式", True), ("给这个玩家关创造模式", False)]
        for text, enabled in cases:
            with self.subTest(text=text):
                intent = parse(text, {})
                self.assertIsNotNone(intent)
                self.assertIs(intent.parameters["enabled"], enabled)
