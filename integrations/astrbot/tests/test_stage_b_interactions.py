"""Synthetic plugin dialogues and permission transitions; no QQ/game writes."""
import copy
import json
import unittest
from dataclasses import replace
from types import SimpleNamespace as NS
from test_companion import actor
from test_companion import mcp
from test_nearby_confirmation import nearby
import test_workbench
from astrbot_plugin_dst_companion.capabilities import permitted
from astrbot_plugin_dst_companion.interactions import interaction, preview_seconds
from astrbot_plugin_dst_companion.model_tools import capability_page
from astrbot_plugin_dst_companion.gateway import QueryError
from astrbot_plugin_dst_companion.routing import Intent
from astrbot_plugin_dst_companion.state import Target


class StageBMetadataTests(unittest.TestCase):
    def item(self):
        item=nearby()
        item['behaviorRevision']='4'
        item['parameters']['properties']['action']['enum']=['pick','delete']
        item['actions']=[{'id':'act_nearby_entities.'+name,'title':title,'parameters':{'action':name},'searchTerms':[title],
                         'risk':risk,'interaction':{'kind':'preview-confirm-v1','resultVersion':version,'effectVersion':'1'}}
                        for name,title,risk,version in [('pick','拾取','high',2),('delete','删除','critical',1)]]
        return item

    def test_action_permission_is_checked_again_with_actual_parameters(self):
        item=self.item()
        for role in ['game-query','game-manage','panel-manage']:
            for mode in ['none','standard','all']:
                user=replace(actor(role=role),workbench_mode=mode)
                for action in ['pick','delete','forged']:
                    with self.subTest(role=role,mode=mode,action=action):
                        allowed=role in {'game-manage','panel-manage'} and (mode=='all' or mode=='standard' and action=='pick')
                        self.assertEqual(permitted(user,item,{'action':action}),allowed)

    def test_standard_directory_never_lists_critical_subactions(self):
        user=replace(actor(role='game-manage'),workbench_mode='standard')
        rows=json.loads(capability_page([self.item()],user,'{}','write'))['items']
        self.assertEqual([a['parameters']['action'] for a in rows[0]['actions']],['pick'])
        self.assertEqual(len(self.item()['actions']),2)

    def test_version_metadata_is_finite_and_cannot_contain_scripts(self):
        item=self.item()
        self.assertEqual(interaction(item,{'action':'pick'})['resultVersion'],2)
        for change in [{'resultVersion':3},{'kind':'eval'},{'script':'do bad() end'},{'effectVersion':None}]:
            with self.subTest(change=change):
                altered=copy.deepcopy(item);altered['actions'][0]['interaction'].update(change)
                with self.assertRaises(QueryError):interaction(altered,{'action':'pick'})

    def test_expiry_uses_server_interval_and_accounts_for_transport_time(self):
        batch={'version':2,'observedAt':100,'expiresAt':130}
        self.assertEqual(preview_seconds(batch,4),26)
        self.assertEqual(preview_seconds(batch,40),0)
        for batch in [{'version':3},{'version':2,'observedAt':100,'expiresAt':160},{'version':2,'observedAt':'100','expiresAt':130}]:
            with self.assertRaises(QueryError):preview_seconds(batch,0)

    def test_direct_grammar_preserves_clear_intent_and_falls_back_for_discussion(self):
        from astrbot_plugin_dst_companion.interactions import direct_request
        manager=replace(actor(role='game-manage'),workbench_mode='standard')
        item=self.item()
        item['parameters']['properties']['action']['enum']=['fertilize','grow','ripen','harvest','pick']
        item['actions']=[{'parameters':{'action':action},'risk':'high','interaction':{'kind':'preview-confirm-v1','resultVersion':2,'effectVersion':'1'}} for action in ['fertilize','grow','ripen','harvest','pick']]
        for text,action in [('给示例玩家附近的作物施肥','fertilize'),('示例玩家附近催熟','ripen'),('示例玩家附近推进生长','grow'),('示例玩家附近采摘','harvest'),('示例玩家附近拾取掉落物','pick')]:
            with self.subTest(text=text):
                result=direct_request(text,[item],manager)
                self.assertEqual(result[1]['action'],action)
                self.assertEqual(result[1]['player_id'],'示例玩家')
        for text in ['不要给示例玩家附近施肥','如果有空给示例玩家附近施肥','给示例玩家附近施肥然后催熟','附近修复','给示例玩家附近施肥是什么','能不能给示例玩家附近施肥？']:
            self.assertIsNone(direct_request(text,[item],manager),text)


class StageBFlowTests(unittest.IsolatedAsyncioTestCase):
    fixture = test_workbench.WorkbenchTests.fixture

    async def setup_flow(self, *, matched=2, version=2, uncertain_execute=False):
        item=StageBMetadataTests().item()
        panel,engine=await self.fixture(item)
        original=engine.gateway.transport
        async def transport(name,args,timeout,write):
            result=await original(name,args,timeout,write)
            if name=='api_runBotCapability':
                preview=args['body']['parameters'].get('phase')=='preview'
                uncertain=uncertain_execute and not preview
                batch={'version':version,'state':'preview' if preview else 'partial' if uncertain else 'completed',
                       'previewId':'00000000-0000-4000-8000-000000000001','observedAt':100,'expiresAt':130,
                       'matched':matched,'attempted':0 if preview else matched,'verified':0 if preview or uncertain else matched,
                       'unconfirmed':matched if uncertain else 0,'skipped':0,'failed':0,'units':0,'items':[]}
                data={'status':'uncertain' if uncertain else 'succeeded','complete':not uncertain,
                      'requestId':args['requestId'],'roomId':'r1','resultKind':'workbench-run','data':{'batch':batch}}
                panel.requests[args['requestId']]=data
                return mcp(data)
            return result
        engine.gateway.transport=transport
        manager=replace(actor(role='game-manage'),workbench_mode='standard')
        intent=Intent('capability',capability=item['id'],parameters={'player_id':'KU_TEST123','action':'pick','radius':10,
                      'phase':'execute','preview_id':'forged','confirmed':True})
        return panel,engine,manager,intent

    def message(self,user,identity):
        return replace(user,event=NS(message_obj=NS(message_id=identity)))

    async def test_standard_pick_uses_one_real_confirmation_and_never_model_boolean(self):
        panel,engine,user,intent=await self.setup_flow()
        reply=await engine.execute(user,intent,Target('r1'))
        self.assertEqual(reply.outcome,'needs_selection',reply.text)
        sent=[a['body']['parameters'] for n,a in panel.calls if n=='api_runBotCapability']
        self.assertEqual(sent[0]['phase'],'preview')
        self.assertNotIn('confirmed',sent[0]);self.assertNotIn('preview_id',sent[0])
        await engine.handle(self.message(user,'boolean'),'true',woken=True)
        self.assertEqual(panel.executions,1)
        confirm=self.message(user,'human')
        await engine.handle(confirm,'确认',woken=True)
        sent=[a['body']['parameters'] for n,a in panel.calls if n=='api_runBotCapability']
        self.assertEqual(len(sent),2)
        self.assertEqual(sent[-1]['phase'],'execute');self.assertTrue(sent[-1]['confirmed'])
        await engine.handle(confirm,'确认',woken=True)
        self.assertEqual(panel.executions,2)

    async def test_preview_cannot_execute_after_identity_permission_or_expiry_changes(self):
        for scenario in ['cancel','expiry','role','disabled','other-user','wrong-quote','schema','configuration']:
            with self.subTest(scenario=scenario):
                panel,engine,user,intent=await self.setup_flow()
                await engine.execute(user,intent,Target('r1'))
                confirmation=self.message(user,'next')
                quote=''
                if scenario=='cancel': text='取消'
                else: text='确认'
                if scenario=='expiry':engine.state.forms[user.key]['previewUntil']=engine.state.clock()-1
                if scenario=='role':confirmation=replace(confirmation,role='game-query')
                if scenario=='disabled':confirmation=replace(confirmation,disabled_capabilities=(intent.capability,))
                if scenario=='other-user':confirmation=replace(confirmation,key=(*user.key[:-1],'other'),user='other')
                if scenario=='wrong-quote':quote='unrelated-message'
                if scenario=='schema':engine.state.forms[user.key]['item']['schemaRevision']='old'
                if scenario=='configuration':confirmation=replace(confirmation,config_revision='changed')
                await engine.handle(confirmation,text,woken=True,quote=quote)
                self.assertEqual(panel.executions,1)

    async def test_new_protocol_and_empty_preview_never_arm_confirmation(self):
        for kwargs in [{'matched':0},{'version':3}]:
            with self.subTest(kwargs=kwargs):
                panel,engine,user,intent=await self.setup_flow(**kwargs)
                await engine.execute(user,intent,Target('r1'))
                self.assertIsNone(engine.state.pending_form(user.key))
                await engine.handle(self.message(user,'confirmation'),'确认',woken=True)
                self.assertEqual(panel.executions,1)

    async def test_uncertain_execute_and_duplicate_message_read_original_without_replay(self):
        panel,engine,user,intent=await self.setup_flow(uncertain_execute=True)
        await engine.execute(user,intent,Target('r1'))
        confirmation=self.message(user,'execute-once')
        reply=await engine.handle(confirmation,'确认',woken=True)
        self.assertIn('未确认',reply.text)
        await engine.handle(confirmation,'确认',woken=True)
        await engine.handle(self.message(user,'late-confirm'),'确认',woken=True)
        self.assertEqual(panel.executions,2)

    async def test_standard_selector_hides_delete_and_rejects_explicit_critical_action(self):
        panel,engine,user,intent=await self.setup_flow()
        reply=await engine.execute(user,replace(intent,parameters={'player_id':'KU_TEST123','radius':10}),Target('r1'))
        self.assertEqual(reply.outcome,'needs_selection',reply.text)
        form=engine.state.pending_form(user.key)
        self.assertEqual(form['field'],'action')
        self.assertEqual(form['schema']['enum'],['pick'])
        engine.state.begin(user.key)
        await engine.execute(self.message(user,'delete'),replace(intent,parameters={'player_id':'KU_TEST123','action':'delete','radius':10}),Target('r1'))
        self.assertEqual(panel.executions,0)
