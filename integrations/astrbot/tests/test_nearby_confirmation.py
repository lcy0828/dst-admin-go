"""Full actor-bound preview/confirmation flow through the existing write journal."""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import copy
import unittest
from dataclasses import replace
from types import SimpleNamespace as NS
from test_companion import actor, mcp
import test_workbench
from test_workbench import recipe
from astrbot_plugin_dst_companion.routing import Intent
from astrbot_plugin_dst_companion.state import Target


def nearby():
    item = recipe('workbench.act_nearby_entities', 'critical')
    props = item['parameters']['properties']
    props.update(action={'type':'string','enum':['freeze','delete']}, radius={'type':'integer','minimum':1,'maximum':64},
                 phase={'type':'string','enum':['preview','execute','cancel']}, preview_id={'type':'string','maxLength':500},
                 layout={'type':'string','enum':['3x3'],'default':'3x3'})
    item['parameters']['required'] = ['player_id','action','radius']
    item['actions'] = [{'id':'nearby.freeze','title':'冻结附近生物','searchTerms':['冻住附近生物'], 'parameters':{'action':'freeze'}}]
    return item


class NearbyConfirmationTests(unittest.IsolatedAsyncioTestCase):
    fixture = test_workbench.WorkbenchTests.fixture

    async def setup_flow(self, uncertain=False):
        panel, engine = await self.fixture(nearby())
        original = engine.gateway.transport
        async def transport(name,args,timeout,write):
            result = await original(name,args,timeout,write)
            if name == 'api_runBotCapability':
                params=args['body']['parameters']
                preview=params.get('phase','preview') == 'preview'
                data={'status':'uncertain' if uncertain else 'succeeded','complete':not uncertain,
                      'requestId':args['requestId'],'roomId':'r1','resultKind':'workbench-run',
                      'data':{'batch':{'state':'preview' if preview else 'completed','previewId':'00000000-0000-4000-8000-000000000001',
                                      'matched':2,'attempted':0 if preview else 2,'verified':0 if preview else 2,
                                      'unconfirmed':0,'skipped':0,'failed':0}}}
                panel.requests[args['requestId']] = data
                return mcp(data)
            return result
        engine.gateway.transport = transport
        manager=replace(actor(role='game-manage'),workbench_mode='all')
        intent=Intent('capability',capability=nearby()['id'],parameters={'player_id':'KU_TEST123','action':'freeze','radius':10,'confirmed':True})
        return panel,engine,manager,intent

    def message(self, manager, identity):
        return replace(manager,event=NS(message_obj=NS(message_id=identity)))

    async def test_preview_then_human_confirmation_submits_each_phase_once(self):
        panel,engine,manager,intent=await self.setup_flow()
        result=await engine.execute(manager,intent,Target('r1'))
        self.assertEqual(result.outcome,'needs_selection',result.text)
        self.assertIn('尚未修改游戏',result.text)
        sent=[args['body']['parameters'] for name,args in panel.calls if name=='api_runBotCapability']
        self.assertEqual(len(sent),1)
        self.assertNotIn('confirmed',sent[0])
        await engine.handle(self.message(manager,'not-confirmed'),'true',woken=True)
        self.assertEqual(panel.executions,1)
        confirmed=self.message(manager,'human-confirmed')
        result=await engine.handle(confirmed,'确认',woken=True)
        self.assertEqual(result.outcome,'matched',result.text)
        self.assertEqual(panel.executions,2)
        sent=[args['body']['parameters'] for name,args in panel.calls if name=='api_runBotCapability']
        self.assertEqual(sent[-1]['phase'],'execute')
        self.assertTrue(sent[-1]['confirmed'])
        self.assertEqual(sent[-1]['preview_id'],'00000000-0000-4000-8000-000000000001')
        await engine.handle(confirmed,'确认',woken=True)
        self.assertEqual(panel.executions,2)

    async def test_cancel_expiry_and_permission_revocation_never_execute_preview(self):
        for scenario in ('cancel','expiry','role','other-user','schema'):
            with self.subTest(scenario=scenario):
                panel,engine,manager,intent=await self.setup_flow()
                await engine.execute(manager,intent,Target('r1'))
                confirmation=self.message(manager,'next-message')
                if scenario=='expiry':
                    engine.state.forms[manager.key]['previewUntil']=engine.state.clock()-1
                if scenario=='role': confirmation=replace(confirmation,role='game-read')
                if scenario=='other-user': confirmation=replace(confirmation,key=(*manager.key[:-1],'other'),user='other')
                if scenario=='schema':
                    engine.state.forms[manager.key]['item']=copy.deepcopy(nearby())
                    engine.state.forms[manager.key]['item']['schemaRevision']='old'
                await engine.handle(confirmation,'取消' if scenario=='cancel' else '确认',woken=True)
                self.assertEqual(panel.executions,1)

    async def test_uncertain_preview_does_not_arm_or_automatically_resubmit(self):
        panel,engine,manager,intent=await self.setup_flow(uncertain=True)
        await engine.execute(manager,intent,Target('r1'))
        self.assertIsNone(engine.state.pending_form(manager.key))
        await engine.handle(self.message(manager,'uncertain-confirm'),'确认',woken=True)
        self.assertEqual(panel.executions,1)

    async def test_direct_action_alias_prompts_only_missing_field_and_discussion_never_dispatches(self):
        panel,engine,manager,_=await self.setup_flow()
        result=await engine.handle(manager,'冻结附近生物 KU_TEST123',woken=True)
        self.assertEqual(result.outcome,'needs_selection',result.text)
        form=engine.state.pending_form(manager.key)
        self.assertEqual(form['parameters'],{'action':'freeze','player_id':'KU_TEST123','phase':'preview'})
        self.assertEqual(form['field'],'radius')
        self.assertEqual(panel.executions,0)
        for i,text in enumerate(('别冻结附近生物','不要冻结附近生物','如果冻结附近生物','冻结附近生物会不会影响玩家','冻结附近生物，然后删除','“冻结附近生物”是什么意思')):
            engine.state.begin(manager.key)
            result=await engine.handle(self.message(manager,f'discussion-{i}'),text,woken=True)
            self.assertEqual(result.outcome,'pass',text)
            self.assertEqual(panel.executions,0)
