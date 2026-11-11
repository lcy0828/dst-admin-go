"""Original request shapes through the real engine, with all writes intercepted."""
import unittest
from dataclasses import replace
from types import SimpleNamespace as NS

from test_companion import actor, mcp
import test_workbench
from test_workbench import recipe
from test_teleport import teleport_recipe
from astrbot_plugin_dst_companion.routing import parse


class PlayerRequestRepairs(unittest.IsolatedAsyncioTestCase):
    fixture = test_workbench.WorkbenchTests.fixture

    async def grant_fixture(self, maximum=400):
        item = recipe('player.give_item', 'low')
        item.update(title='发放玩家物品', resultKind='item-grant',
                    parameterHints={'playerId': 'player'}, nativeTools=['api_giveRoomPlayerItem'])
        item['parameters'].update(properties={
            'playerId': {'type': 'string', 'maxLength': 128},
            'item': {'type': 'string', 'maxLength': 100},
            'count': {'type': 'integer', 'minimum': 1, 'maximum': maximum}},
            required=['playerId', 'item', 'count'])
        panel, engine = await self.fixture(item)
        original = engine.gateway.transport
        async def transport(name, args, timeout, write):
            if name == 'api_resolveRoomPlayer':
                query = args['query']['query']
                rows = [{'id': 'KU_TEST123', 'name': 'Example', 'online': True}] if query == 'Example' else []
                return mcp({'status': 'resolved' if rows else 'not_found', 'match': 'exact_name',
                            'items': rows, 'total': len(rows), 'hasMore': False})
            return await original(name, args, timeout, write)
        engine.gateway.transport = transport
        manager = replace(actor(role='game-manage'), write_allowlist=('*',), workbench_mode='standard')
        return panel, engine, manager

    async def test_complete_grant_variants_resolve_identity_and_write_once(self):
        for text in ('给玩家 Example 发 100 个木头', '给玩家 Example 100 个木头',
                     '给玩家Example100个木头', '发 100 个木头给 Example'):
            with self.subTest(text=text):
                panel, engine, manager = await self.grant_fixture()
                reply = await engine.handle(manager, text, woken=True, message_id='request')
                self.assertEqual(reply.outcome, 'matched', reply.text)
                writes = [a for n,a in panel.calls if n == 'api_runBotCapability']
                self.assertEqual(len(writes), 1)
                self.assertEqual(writes[0]['body']['parameters'],
                                 {'playerId':'KU_TEST123', 'item':'木头', 'count':100})
                await engine.handle(manager, text, woken=True, message_id='request')
                self.assertEqual(panel.executions, 1)

    async def test_missing_player_keeps_item_and_quantity_across_followup(self):
        panel, engine, manager = await self.grant_fixture()
        reply = await engine.handle(manager, '给玩家发 100 个木头', woken=True, message_id='one')
        self.assertIn('玩家昵称', reply.text)
        self.assertEqual(panel.executions, 0)
        reply = await engine.handle(manager, 'Example', woken=True, message_id='two')
        self.assertEqual(reply.outcome, 'matched', reply.text)
        args = next(a for n,a in panel.calls if n == 'api_runBotCapability')
        self.assertEqual(args['body']['parameters']['count'], 100)
        self.assertEqual(args['body']['parameters']['item'], '木头')

    async def test_old_panel_limit_prompts_exact_bound_and_keeps_remaining_parameters(self):
        panel, engine, manager = await self.grant_fixture(40)
        reply = await engine.handle(manager, '给玩家 Example 发 100 个木头', woken=True, message_id='one')
        self.assertIn('最多 40', reply.text)
        self.assertEqual(panel.executions, 0)
        reply = await engine.handle(manager, '40', woken=True, message_id='two')
        self.assertEqual(reply.outcome, 'matched', reply.text)
        args = next(a for n,a in panel.calls if n == 'api_runBotCapability')
        self.assertEqual(args['body']['parameters'], {'playerId':'KU_TEST123','item':'木头','count':40})

    def test_spawn_position_and_compact_ambiguity(self):
        intent = parse('在玩家 Example 的位置生成一个独眼巨鹿', {})
        self.assertEqual(intent.capability, 'player.spawn_entity')
        self.assertEqual(intent.parameters, {'playerId':'玩家 Example', 'entity':'独眼巨鹿', 'count':1})
        for text in ('给玩家三三个木头', '给Example100个木头', '如果给玩家Example100个木头'):
            self.assertIsNone(parse(text, {}), text)

    async def test_common_destination_skips_saved_list_and_offers_actual_candidates(self):
        item = teleport_recipe()
        item['parameters']['properties'].update(worldId={'type':'string','maxLength':255},
                                                landmarkRef={'type':'string','maxLength':100})
        item['actions'] = [{'id':'pig_king','parameters':{'locationName':'猪王'},'searchTerms':['猪王村']}]
        panel, engine = await self.fixture(item)
        manager = replace(actor(role='game-manage'), write_allowlist=('*',), workbench_mode='standard')
        original = engine.gateway.transport
        async def transport(name, args, timeout, write):
            if name == 'api_listRoomLocations':
                self.fail('common destination must not require a saved location')
            value = await original(name,args,timeout,write)
            if name == 'api_runBotCapability':
                value = {'status':'succeeded','complete':True,'data':{
                    'state':'needs_selection','worldId':'w1','candidates':[
                        {'id':'pig_king:1','landmarkId':'pig_king','x':12,'z':24},
                        {'id':'pig_king:2','landmarkId':'pig_king','x':36,'z':48}]}}
                panel.requests[args['requestId']] = value
                return mcp(value)
            return value
        engine.gateway.transport = transport
        reply = await engine.handle(manager, '传送 KU_TEST123 到猪王村', woken=True, message_id='one')
        self.assertEqual(reply.outcome,'needs_selection',reply.text)
        self.assertIn('X 12.0 / Z 24.0',reply.text)
        self.assertEqual(engine.state.choices[manager.key]['mode'],'landmark')
        selection_actor = replace(manager, event=NS(message_obj=NS(message_id='two')))
        selected = await engine.handle(selection_actor, '2', woken=True, message_id='two')
        writes = [a for n,a in panel.calls if n=='api_runBotCapability']
        self.assertEqual(len(writes),2,selected.text)
        self.assertEqual(writes[-1]['body']['parameters']['landmarkRef'],'pig_king:2')
        self.assertEqual(writes[-1]['body']['parameters']['locationName'],'猪王村')
