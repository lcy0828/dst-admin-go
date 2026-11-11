"""Chat help stays short while every authorized catalog entry remains reachable."""

import importlib.util
import sys
import time
import unittest
from dataclasses import replace
from pathlib import Path
from unittest.mock import AsyncMock

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from test_companion import actor
from test_v41 import Panel, capability

from astrbot_plugin_dst_companion.capabilities import Directory
from astrbot_plugin_dst_companion.configuration import DEFAULTS
from astrbot_plugin_dst_companion.engine import Engine
from astrbot_plugin_dst_companion.gateway import Gateway, QueryError
from astrbot_plugin_dst_companion.menus import GROUPS, ROOT_GROUPS
from astrbot_plugin_dst_companion.policy import Policy
from astrbot_plugin_dst_companion.routing import parse
from astrbot_plugin_dst_companion.state import State


def sample(ident, title, category="status", access="read", **fields):
    return {**capability(ident, access=access), "title": title, "category": category, **fields}


class GroupedMenuTests(unittest.IsolatedAsyncioTestCase):
    def setup_engine(self, items=(), *, native=None, config=None):
        panel = Panel()
        panel.items = list(items)
        policy = Policy(native or AsyncMock(return_value=True))
        gateway = Gateway(panel.transport, policy.authorize)
        directory = Directory(gateway)
        policy.directory = directory
        engine = Engine(gateway, State(), directory=directory,
                        settings=(lambda _: config) if config is not None else None)
        return engine, panel, directory

    def admin(self, **changes):
        return replace(actor(role="game-manage"), write_allowlist=("*",),
                       visibility=actor().visibility | {"performance", "world", "backups"}, **changes)

    def index(self, engine, user, ident):
        pending = engine.state.choices[user.key]
        size, page = pending["pageSize"], pending["page"]
        return str(next(i for i, r in enumerate(pending["candidates"][page*size:(page+1)*size], 1) if r["id"] == ident))

    async def test_reported_nineteen_page_catalog_becomes_one_home_screen(self):
        items = [sample(f"room.read-{i}", f"扩展查询{i}") for i in range(95)]
        engine, panel, _ = self.setup_engine(items)
        user = self.admin()
        reply = await engine.handle(user, "帮助", woken=True)
        pending = engine.state.choices[user.key]
        self.assertLessEqual(len(pending["candidates"]), 10)
        self.assertNotIn("页", reply.text)
        self.assertNotIn("下一页", reply.text)
        self.assertLess(len(reply.text), 1000)
        self.assertIn("搜索功能", reply.text)
        self.assertEqual([r["id"] for r in pending["candidates"][:5]],
                         ["rooms", "status", "players", "profile", "inventory"])
        self.assertEqual(panel.executions, 0)

    async def test_all_authorized_rows_are_reachable_via_category_numbers(self):
        items = [sample("player.extra", "更多玩家信息", "profile"),
                 sample("player.resurrect", "复活玩家", "management", "write"),
                 sample("room.release", "游戏更新", "management", "write"),
                 sample("room.backups.extra", "备份记录", "backups"),
                 sample("room.extra", "额外状态")]
        engine, panel, _ = self.setup_engine(items)
        user = self.admin()
        found = set()
        await engine.handle(user, "菜单", woken=True)
        for key in ROOT_GROUPS:
            number = self.index(engine, user, "menu:" + key)
            reply = await engine.handle(user, number)
            self.assertIn(GROUPS[key], reply.text)
            entries = list(engine.state.choices[user.key]["candidates"])
            for entry in entries:
                if entry["kind"] == "help":
                    await engine.handle(user, entry["name"], woken=True)
                    found.update(r["id"] for r in engine.state.choices[user.key]["candidates"])
                else:
                    found.add(entry["id"])
            await engine.handle(user, "返回菜单")
        self.assertTrue({r["id"] for r in items} <= found)
        self.assertEqual(panel.executions, 0)

    async def test_large_game_management_catalog_is_split_by_targets_and_parameters(self):
        examples = [sample("player.extra", "玩家状态", "management", "write", parameterHints={"playerId": "player"}),
                    sample("player.give_item", "发物品", "management", "write"),
                    sample("workbench.spawn_entity_at", "生成生物", "management", "write"),
                    sample("player.teleport", "传送玩家", "management", "write"),
                    sample("workbench.set_world_day", "世界时间", "management", "write"),
                    sample("room.reset", "重置世界", "management", "write")]
        engine, panel, _ = self.setup_engine(examples)
        user = self.admin()
        reply = await engine.handle(user, "游戏管理", woken=True)
        self.assertNotIn("下一页", reply.text)
        self.assertEqual(len(engine.state.choices[user.key]["candidates"]), 6)
        self.assertIn("玩家状态与模式", reply.text)
        self.assertIn("物品与装备", reply.text)
        self.assertIn("世界与时间", reply.text)
        await engine.handle(user, "1")
        self.assertEqual([r["id"] for r in engine.state.choices[user.key]["candidates"]], ["kick", "player.extra"])
        await engine.handle(user, "返回游戏管理")
        self.assertEqual(engine.state.choices[user.key]["title"], "游戏管理")
        await engine.handle(user, "返回菜单")
        self.assertTrue(engine.state.choices[user.key]["menuHome"])
        self.assertEqual(panel.executions, 0)

    async def test_category_page_keeps_title_and_return_search_guidance(self):
        engine, panel, _ = self.setup_engine([
            sample(f"room.diag-{i}", f"运行检查{i}") for i in range(25)
        ])
        user = self.admin()
        reply = await engine.handle(user, "面板与运行诊断", woken=True)
        self.assertIn("1/3 页", reply.text)
        second = await engine.handle(user, "下一页")
        self.assertIn("面板与运行诊断（2/3 页）", second.text)
        self.assertIn("返回菜单", second.text)
        self.assertIn("搜索功能", second.text)
        self.assertIn("运行检查10", second.text)
        self.assertNotIn("运行检查0\n", second.text)
        self.assertEqual(panel.executions, 0)

    async def test_search_by_title_alias_or_action_never_executes_even_single_match(self):
        item = sample("player.modes", "玩家模式", "management", "write",
                      aliases=["无敌"],
                      parameters={"type": "object", "properties": {"mode": {"type": "string", "enum": ["kill"]}},
                                  "required": ["mode"], "additionalProperties": False},
                      actions=[{"id": "modes.kill", "title": "一击必杀", "parameters": {"mode": "kill"}, "searchTerms": ["秒杀"]}])
        engine, panel, _ = self.setup_engine([item])
        user = self.admin()
        for query in ("玩家模式", "无敌", "一击必杀", "秒杀"):
            reply = await engine.handle(user, "搜索功能 " + query, woken=True)
            self.assertEqual(reply.outcome, "needs_selection")
            self.assertEqual([r["id"] for r in engine.state.choices[user.key]["candidates"]], [item["id"]])
            self.assertIn("玩家模式", reply.text)
        no_match = await engine.handle(user, "搜索功能 复活;重置世界", woken=True)
        self.assertIn("没有找到", no_match.text)
        self.assertEqual(panel.executions, 0)

    async def test_search_selection_uses_existing_parameter_and_execution_flow(self):
        item = sample("room.sample-report", "示例报表",
                      parameters={"type": "object", "properties": {"days": {"type": "integer", "minimum": 1, "maximum": 7}},
                                  "required": ["days"], "additionalProperties": False})
        engine, panel, _ = self.setup_engine([item])
        user = actor()
        await engine.handle(user, "搜索功能 示例报表", woken=True)
        reply = await engine.handle(user, "1")
        self.assertIn("days", reply.text)
        self.assertEqual(panel.executions, 0)
        reply = await engine.handle(user, "3", woken=True)
        self.assertEqual(reply.data["data"], {"answer": 42})
        self.assertEqual(panel.executions, 1)

    async def test_common_query_search_accepts_existing_shortcut_phrases(self):
        engine, panel, _ = self.setup_engine()
        user = actor()
        for phrase, ident in (("查背包", "inventory"), ("谁在线", "players"), ("网络延迟", "network")):
            reply = await engine.handle(user, "搜索功能 " + phrase, woken=True)
            self.assertEqual(reply.outcome, "needs_selection")
            self.assertIn(ident, [r["id"] for r in engine.state.choices[user.key]["candidates"]])
        self.assertEqual(panel.executions, 0)

    async def test_hidden_disabled_native_denied_and_ungranted_writes_not_disclosed(self):
        items = [sample("player.secret", "禁止操作", "management", "write"),
                 sample("room.disabled", "已停用查询"),
                 sample("room.native-denied", "原生拒绝查询", nativeTools=["api_getRoomPlayerInventory"]),
                 sample("room.hidden-data", "未开放性能", "performance"),
                 sample("room.visible", "允许查询")]

        async def native(user, tool):
            return tool != "api_getRoomPlayerInventory"

        engine, panel, _ = self.setup_engine(items, native=native)
        user = replace(actor(), disabled_capabilities=("room.disabled",))
        for phrase in ("菜单", "面板与运行诊断", "搜索功能 查询", "搜索功能 禁止", "搜索功能 性能"):
            reply = await engine.handle(user, phrase, woken=True)
            for hidden in ("禁止操作", "已停用查询", "原生拒绝查询", "未开放性能"):
                self.assertNotIn(hidden, reply.text)
        self.assertEqual(panel.executions, 0)

    async def test_selection_rechecks_config_and_permissions(self):
        engine, panel, _ = self.setup_engine([sample("room.extra", "可见扩展")])
        user = actor()
        await engine.handle(user, "搜索功能 可见扩展", woken=True)
        reply = await engine.handle(replace(user, config_revision="changed"), "1")
        self.assertIn("配置已改变", reply.text)
        self.assertEqual(panel.executions, 0)
        await engine.handle(user, "搜索功能 可见扩展", woken=True)
        reply = await engine.handle(replace(user, disabled_capabilities=("room.extra",)), "1")
        self.assertEqual(reply.outcome, "handled_error")
        self.assertIn("未开放此操作", reply.text)
        self.assertEqual(panel.executions, 0)

    async def test_custom_menu_visibility_and_order_remain_effective(self):
        config = {**DEFAULTS, "menuItems": ["room.second", "room.first"], "defaultRoom": "r1"}
        engine, panel, _ = self.setup_engine([sample("room.first", "查询甲"), sample("room.second", "查询乙"),
                                             sample("room.hidden", "隐藏项目")], config=config)
        user = actor()
        reply = await engine.handle(user, "菜单", woken=True)
        self.assertNotIn("服务器列表", reply.text)
        await engine.handle(user, "面板与运行诊断")
        self.assertEqual([r["id"] for r in engine.state.choices[user.key]["candidates"]], ["room.second", "room.first"])
        reply = await engine.handle(user, "搜索功能 隐藏", woken=True)
        self.assertIn("没有找到", reply.text)
        self.assertEqual(panel.executions, 0)

    async def test_catalog_upgrade_adds_new_item_without_plugin_mapping(self):
        engine, panel, directory = self.setup_engine([])
        user = actor()
        await engine.handle(user, "菜单", woken=True)
        panel.items.append(sample("room.added-later", "新增加的查询"))
        panel.revision = "revision-2"
        await directory.items(user, time.monotonic()+5, force=True)
        reply = await engine.handle(user, "搜索功能 新增加", woken=True)
        self.assertIn("新增加的查询", reply.text)
        self.assertEqual(panel.executions, 0)

    async def test_unavailable_catalog_and_empty_custom_menu_do_not_claim_absence(self):
        engine, _, directory = self.setup_engine()
        directory.items = AsyncMock(side_effect=QueryError("PANEL_DISCONNECTED"))
        reply = await engine.handle(actor(), "帮助", woken=True)
        self.assertIn("更多功能暂时没能读取", reply.text)
        self.assertIn("玩家背包", reply.text)
        engine, _, _ = self.setup_engine(config={**DEFAULTS, "menuItems": []})
        reply = await engine.handle(actor(), "帮助", woken=True)
        self.assertFalse(reply.choice)
        self.assertIn("没有可展示", reply.text)

    async def test_menu_navigation_is_user_scoped_and_rejects_old_quotes(self):
        engine, panel, _ = self.setup_engine([sample("room.extra", "额外查询")])
        user = actor()
        reply = await engine.handle(user, "菜单", woken=True)
        engine.state.choice_sent(user.key, "old-menu", reply.sequence)
        self.assertEqual((await engine.handle(actor(user="other"), "返回菜单")).outcome, "pass")
        self.assertEqual((await engine.handle(actor(user="other"), "搜索功能 额外")).outcome, "pass")
        reply = await engine.handle(user, "面板与运行诊断")
        self.assertIn("额外查询", reply.text)
        self.assertEqual((await engine.handle(user, "1", quote="old-menu")).outcome, "pass")
        self.assertEqual(panel.executions, 0)

    def test_every_category_is_a_read_intent_and_search_is_bounded(self):
        for phrase in [*GROUPS.values(), "返回菜单", "搜索功能 复活", "查找功能 地图", "找功能 更新"]:
            self.assertEqual(parse(phrase, {}).kind, "help")
        self.assertIsNone(parse("搜索功能 " + "字"*65, {}))


AVAILABLE = importlib.util.find_spec("astrbot") is not None
if AVAILABLE:
    from astrbot.api import message_components as C
    import test_qq_pipeline as pipeline


@unittest.skipUnless(AVAILABLE, "requires installed AstrBot framework")
class MenuPipelineTests(unittest.IsolatedAsyncioTestCase):
    if AVAILABLE:
        asyncSetUp = pipeline.QQPipelineTests.asyncSetUp
        asyncTearDown = pipeline.QQPipelineTests.asyncTearDown
        event = pipeline.QQPipelineTests.event
        dispatch = pipeline.QQPipelineTests.dispatch

    async def test_actual_at_help_category_number_return_and_existing_inventory_shortcut(self):
        output = await self.dispatch(self.event([C.At(qq="456"), C.Plain("帮助")]))
        self.assertIn("活动与连接", output[-1])
        self.assertNotIn("页", output[-1])
        output = await self.dispatch(self.event([C.Plain("6")]))
        self.assertIn("活动与连接", output[-1])
        self.assertIn("玩家活动", output[-1])
        output = await self.dispatch(self.event([C.Plain("返回菜单")]))
        self.assertIn("可用查询与管理", output[-1])
        output = await self.dispatch(self.event([C.Plain("5")]))
        self.assertIn("玩家昵称", output[-1])
        self.assertEqual(self.models, [])
        self.assertEqual(self.other_handlers, [])

    async def test_actual_at_search_does_not_reach_model_or_run_management(self):
        output = await self.dispatch(self.event([C.At(qq="456"), C.Plain("搜索功能 背包")]))
        self.assertIn("玩家背包", output[-1])
        self.assertIn("查找功能", output[-1])
        output = await self.dispatch(self.event([C.Plain("1")]))
        self.assertIn("玩家昵称", output[-1])
        self.assertEqual(self.models, [])
        self.assertFalse(any(name in {"api_queryBotCapability", "api_runBotCapability"} for name, _ in self.panel.calls))
