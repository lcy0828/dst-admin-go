"""QQ management does not require native AstrBot admin; no external game/QQ I/O."""

import asyncio
import unittest
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock

import test_astrbot_adapter as adapter_tests
from test_astrbot_adapter import AVAILABLE

from astrbot_plugin_dst_companion.gateway import QueryError
from astrbot_plugin_dst_companion.group_permissions import GroupRoles, grants


class GroupRoleTests(unittest.IsolatedAsyncioTestCase):
    def event(self, role="admin"):
        return NS(
            get_platform_id=lambda: "qq",
            message_obj=NS(self_id="456"),
            unified_msg_origin="qq:GroupMessage:789",
            get_sender_id=lambda: "123",
            get_group_id=lambda: "789",
            is_admin=lambda: False,
            is_private_chat=lambda: False,
            get_platform_name=lambda: "aiocqhttp",
            bot=NS(
                call_action=AsyncMock(return_value={"user_id": 123, "group_id": 789, "role": role})
            ),
        )

    async def test_admin_identity_comes_from_adapter_and_demotion_invalidates(self):
        roles, event = GroupRoles(), self.event()
        self.assertEqual(roles.current(event), "member")
        self.assertEqual(await roles.refresh(event), "admin")
        self.assertEqual(roles.current(event), "admin")
        event.bot.call_action.return_value["role"] = "member"
        self.assertEqual(await roles.refresh(event, force=True), "member")
        self.assertEqual(roles.current(event), "member")
        await roles.close()

    async def test_failed_or_other_group_identity_never_preserves_admin(self):
        roles, event = GroupRoles(), self.event()
        await roles.refresh(event)
        event.bot.call_action.return_value["group_id"] = 111
        with self.assertRaises(QueryError):
            await roles.refresh(event, force=True)
        self.assertEqual(roles.current(event), "member")
        await roles.close()

    async def test_parallel_role_reads_are_merged(self):
        roles, event = GroupRoles(), self.event()
        self.assertEqual(
            await asyncio.gather(*(roles.refresh(event) for _ in range(5))), ["admin"] * 5
        )
        event.bot.call_action.assert_awaited_once()
        await roles.close()

    def test_member_override_is_specific_and_cannot_exceed_group_ceiling(self):
        record = {"rolePermissions": {"member": {"write": ["player.resurrect"]}}}
        rule, _ = grants({}, record, "member")
        self.assertEqual(rule["write"], ["player.resurrect"])
        record["rolePermissions"]["member"]["write"] = ["system.update"]
        with self.assertRaises(ValueError):
            grants({}, record, "member")

    def test_denial_survives_group_override(self):
        config = {"group_permissions": {"owner": {"deny": ["player.give_item"]}}}
        rule, _ = grants(config, {"rolePermissions": {"admin": {"deny": []}}}, "admin")
        self.assertNotIn("player.give_item", rule["write"])
        self.assertIn("player.give_item", rule["deny"])


@unittest.skipUnless(AVAILABLE, "requires real AstrBot")
class GroupAdapterTests(unittest.IsolatedAsyncioTestCase):
    def companion(self):
        bot = adapter_tests.AdapterTests.companion(self)
        bot.config["permission_mode"] = "group"
        bot.config["management_mcp_server"] = ""
        bot.config["game_admins"] = []
        bot.config["panel_admins"] = []
        return bot

    def event(self, role="admin"):
        event = adapter_tests.AdapterTests.event(self, [])
        event.bot.call_action = AsyncMock(
            return_value={"user_id": 123, "group_id": 789, "role": role}
        )
        return event

    async def test_unlisted_qq_admin_can_use_single_connection_and_native_raw_denial_is_not_reused(
        self,
    ):
        bot, event = self.companion(), self.event()
        bot.config["access_policy"] = "simple"
        bot.manager.host._check_tool_permission.return_value = "requires AstrBot admin"
        await bot.prepare(event)
        actor = bot.actor(event)
        self.assertEqual(actor.group_role, "admin")
        self.assertEqual(actor.role, "game-manage")
        self.assertEqual(bot.server(True), "dst-read")
        self.assertTrue(actor.may_write("player.resurrect"))
        self.assertTrue(actor.may_write("room.reset"), "simple policy: administrators manage everything")
        self.assertTrue(await bot.native(actor, "api_runBotCapability"))
        bot.manager.host._check_tool_permission.assert_not_awaited()
        await bot.terminate()

    async def test_member_cannot_use_write_even_when_service_token_has_all_tools(self):
        bot, event = self.companion(), self.event("member")
        await bot.prepare(event)
        actor = bot.actor(event)
        self.assertEqual(actor.role, "game-read")
        with self.assertRaises(QueryError):
            await bot.policy.authorize(actor, "api_runRoomAction", {"path": {"roomId": "r1"}})
        await bot.terminate()

    async def test_role_revoked_before_dispatch_denies_original_actor(self):
        bot, event = self.companion(), self.event()
        await bot.prepare(event)
        actor = bot.actor(event)
        event.bot.call_action.return_value["role"] = "member"
        self.assertFalse(await bot.native(actor, "api_runBotCapability"))
        await bot.terminate()

    async def test_explicit_member_grant_does_not_open_other_actions(self):
        from astrbot_plugin_dst_companion.capabilities import permitted

        bot, event = self.companion(), self.event("member")
        bot.config["access_policy"] = "custom"
        bot.config["group_permissions"] = {"member": {"write": ["player.resurrect"]}}
        await bot.prepare(event)
        actor = bot.actor(event)
        item = {"id": "player.resurrect", "category": "management", "access": "write"}
        self.assertTrue(permitted(actor, item))
        self.assertFalse(permitted(actor, {**item, "id": "player.give_item"}))
        await bot.terminate()

    async def test_groups_and_disable_still_limit_authority(self):
        bot, event = self.companion(), self.event("owner")
        await bot.prepare(event)
        actor = bot.actor(event)
        with self.assertRaises(QueryError):
            await bot.policy.authorize(actor, "api_runRoomAction", {"path": {"roomId": "other"}})
        bot.config["enabled"] = False
        self.assertFalse(await bot.native(actor, "api_runBotCapability"))
        await bot.terminate()

    async def test_failed_identity_also_blocks_explicit_member_writes(self):
        bot, event = self.companion(), self.event("member")
        bot.config["access_policy"] = "custom"
        bot.config["group_permissions"] = {"member": {"write": ["player.resurrect"]}}
        event.bot.call_action.side_effect = TimeoutError()
        await bot.prepare(event)
        actor = bot.actor(event)
        self.assertEqual(actor.role, "game-read")
        self.assertEqual(actor.write_allowlist, ())
        await bot.terminate()


class MigrationTests(unittest.IsolatedAsyncioTestCase):
    async def test_explicit_migration_preserves_rooms_and_only_rebinds_same_panel(self):
        from astrbot_plugin_dst_companion.group_setup import handle

        source = "__dst_private_panel__|https://panel.invalid:443/mcp"
        old = {"source": "legacy|https://panel.invalid:443/mcp", "rooms": ["r1"], "enabled": True,
               "query": True, "notices": {"chat": True}, "messageTemplates": [{"example": "preserved"}]}
        foreign = {**old, "source": "other|https://other.invalid:443/mcp"}
        p = NS(config={}, config_editor=NS(save=True, apply=AsyncMock()),
               private_panel=NS(configured=True, full_access=True, refresh=AsyncMock()),
               ensure_enrollment=AsyncMock(), connections=NS(source=lambda: source), catalog=NS(until=1),
               enrollment=NS(lock=asyncio.Lock(), records={"one": old, "two": foreign}, save=AsyncMock()))
        p.migration_lock = asyncio.Lock()
        state = {"cursor": "example-cursor", "outbox": [{"delivery": "unknown"}], "delivered": ["example-event"]}
        p.put_kv_data = AsyncMock()
        p.notifications = NS(lock=asyncio.Lock(), storage_lock=asyncio.Lock(),
                             stop_feeds=AsyncMock(), engines={}, ready=set(),
                             saved={"one": {"source": old["source"], "state": state},
                                    "two": {"source": foreign["source"], "state": {}}})
        await handle(p, NS(is_admin=lambda: True), "权限模式", "群角色", AsyncMock())
        self.assertEqual(p.enrollment.records["one"]["source"], source)
        self.assertEqual(p.enrollment.records["one"]["assignedRooms"], ["r1"])
        self.assertEqual(p.enrollment.records["one"]["messageTemplates"], old["messageTemplates"])
        self.assertEqual(p.enrollment.records["two"], foreign)
        self.assertEqual(p.notifications.saved["one"], {"source": source, "state": state})
        self.assertEqual(p.notifications.saved["two"]["source"], foreign["source"])
        p.notifications.stop_feeds.assert_awaited_once()


class BulkPermissionsTests(unittest.IsolatedAsyncioTestCase):
    def plugin(self, *, config=None, role="owner", save=None):
        from astrbot_plugin_dst_companion.onboarding import Enrollment

        session = "qq:GroupMessage:789"
        enrollment = Enrollment(save or AsyncMock())
        enrollment.loaded = True
        enrollment.records[session] = {
            "enabled": True, "query": True, "source": "private|https://panel.invalid:443/mcp",
            "rooms": ["r1"], "notices": {"chat": True},
        }
        p = NS(config={"permission_mode": "group", **(config or {})},
               ensure_enrollment=AsyncMock(), allowed=lambda _: True,
               enrollment=enrollment, group_roles=NS(refresh=AsyncMock(return_value=role)))
        event = NS(unified_msg_origin=session)
        return p, event

    async def test_all_cannot_override_global_denials_or_change_other_roles(self):
        from astrbot_plugin_dst_companion.group_setup import handle

        p, event = self.plugin(config={"group_permissions": {"admin": {"deny": ["player.give_item"]}}})
        replies = AsyncMock()
        await handle(p, event, "权限", "管理员 全部 开", replies)
        record = p.enrollment.records[event.unified_msg_origin]
        rule, _ = grants(p.config, record, "admin")
        self.assertNotIn("player.give_item", rule["write"])
        self.assertIn("room.restart", rule["write"])
        self.assertEqual(rule["workbench"], "standard")
        member, _ = grants(p.config, record, "member")
        self.assertEqual(member["write"], [])
        self.assertNotIn("发物品、", replies.call_args.args[0])
        await handle(p, event, "权限", "管理员 全部 关", replies)
        rule, _ = grants(p.config, p.enrollment.records[event.unified_msg_origin], "admin")
        self.assertEqual(rule["read"], [])
        self.assertEqual(rule["write"], [])
        self.assertEqual(rule["workbench"], "off")

    async def test_failed_save_cannot_report_or_apply_success(self):
        from astrbot_plugin_dst_companion.group_setup import handle

        for save in (AsyncMock(return_value=False), AsyncMock(side_effect=OSError("fixture"))):
            p, event = self.plugin(save=save)
            replies = AsyncMock()
            with self.assertRaises((QueryError, OSError)):
                await handle(p, event, "权限", "管理员 全部 开", replies)
            self.assertNotIn("rolePermissions", p.enrollment.records[event.unified_msg_origin])
            replies.assert_not_awaited()

    async def test_concurrent_group_change_is_not_overwritten(self):
        from astrbot_plugin_dst_companion.configuration import fingerprint

        p, event = self.plugin()
        old = dict(p.enrollment.records[event.unified_msg_origin])
        expected = fingerprint(old)
        await p.enrollment.update(event.unified_msg_origin, {**old, "query": False})
        with self.assertRaises(QueryError):
            await p.enrollment.update(event.unified_msg_origin, old, expected=expected)
        self.assertFalse(p.enrollment.records[event.unified_msg_origin]["query"])

    async def test_group_disabled_during_identity_read_cannot_receive_grants(self):
        from astrbot_plugin_dst_companion.group_setup import handle
        from unittest.mock import Mock

        p, event = self.plugin()
        p.allowed = Mock(side_effect=[True, False])
        replies = AsyncMock()
        await handle(p, event, "权限", "管理员 全部 开", replies)
        self.assertNotIn("rolePermissions", p.enrollment.records[event.unified_msg_origin])
        p.enrollment.save.assert_not_awaited()
        self.assertIn("授权已变更", replies.call_args.args[0])

    def test_permission_parser_accepts_only_complete_direct_requests(self):
        from astrbot_plugin_dst_companion.group_setup import permission_request

        for text in ("给管理员添加所有权限", "请为管理员授予全部权限", "把管理员的所有权限打开"):
            self.assertEqual(permission_request(text), "管理员 全部 开")
        self.assertEqual(permission_request("为管理员收回所有权限"), "管理员 全部 关")
        for text in ("给管理员添加所有权限吗", "不要给管理员添加所有权限", "如果给管理员添加所有权限", "他说：给管理员添加所有权限", "给管理员添加所有权限，然后重启全部世界"):
            self.assertEqual(permission_request(text), "")
