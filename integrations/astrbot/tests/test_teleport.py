import sys
import unittest
from pathlib import Path
from datetime import datetime, timedelta, timezone
from dataclasses import replace
from types import SimpleNamespace as NS
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from test_companion import actor, mcp
import test_workbench
from test_workbench import recipe
from astrbot_plugin_dst_companion.routing import parse, Intent
from astrbot_plugin_dst_companion.state import Target
from astrbot_plugin_dst_companion.teleport import remaining
from astrbot_plugin_dst_companion.private_connection import PrivatePanel
from test_private_connection import config
import httpx
from unittest.mock import AsyncMock


def teleport_recipe():
    item=recipe('player.teleport','high')
    item.update(title='传送到已保存地点',resultKind='facts',nativeTools=['api_prepareRoomTeleport'],parameterHints={'playerId':'player'},
        parameters={'type':'object','additionalProperties':False,'properties':{'playerId':{'type':'string','maxLength':128},'locationName':{'type':'string','maxLength':48}},'required':['playerId','locationName']})
    return item

class TeleportTests(unittest.IsolatedAsyncioTestCase):
    fixture=test_workbench.WorkbenchTests.fixture
    async def flow(self):
        panel,engine=await self.fixture(teleport_recipe())
        original=engine.gateway.transport
        now=datetime.now(timezone.utc)
        preview={'id':'00000000-0000-4000-8000-000000000001','roomId':'r1','worldId':'w1','state':'prepared','playerId':'KU_TEST123','playerName':'Example','locationName':'基地','x':12,'z':24,'expiresAt':(now+timedelta(seconds=60)).isoformat(),'serverTime':now.isoformat()}
        sends=[]
        async def transport(name,args,timeout,write):
            if name=='api_executeRoomTeleport':
                sends.append(args)
                return mcp({**preview,'state':'confirmed'})
            result=await original(name,args,timeout,write)
            if name=='api_runBotCapability':
                data={'status':'prepared','complete':True,'requestId':args['requestId'],'roomId':'r1','resultKind':'facts','data':preview}
                panel.requests[args['requestId']]=data
                return mcp(data)
            return result
        engine.gateway.transport=transport
        manager=replace(actor(role='game-manage'),workbench_mode='standard')
        intent=Intent('capability',capability='player.teleport',parameters={'playerId':'KU_TEST123','locationName':'基地'},confirm_required=True)
        result=await engine.execute(manager,intent,Target('r1'))
        self.assertEqual(result.outcome,'needs_selection',result.text)
        self.assertEqual(panel.executions,1) # prepare first, no redundant confirmation
        self.assertFalse(sends)
        return panel,engine,manager,sends

    async def test_plain_and_quoted_confirmation_execute_once(self):
        for quote in ('','sent-confirmation'):
            panel,engine,manager,sends=await self.flow()
            form=engine.state.pending_form(manager.key)
            engine.state.form_sent(manager.key,'sent-confirmation',form)
            message=replace(manager,event=NS(message_obj=NS(message_id='human-confirmation')))
            result=await engine.handle(message,'确认',quote=quote,message_id='human-confirmation')
            self.assertEqual(result.outcome,'matched',result.text)
            self.assertIn('已传送，位置已确认',result.text)
            self.assertEqual(len(sends),1)
            await engine.handle(message,'确认',quote=quote,message_id='human-confirmation')
            self.assertEqual(len(sends),1)

    async def test_task_model_prepares_but_cannot_answer_its_own_confirmation(self):
        from astrbot_plugin_dst_companion.task_tools import TaskTools
        panel,engine,manager,sends=await self.flow()
        engine.state.begin(manager.key)
        event=NS(message_obj=NS(message_id='model-turn'),unified_msg_origin='group')
        manager=replace(manager,event=event)
        engine.queries.rooms=AsyncMock(return_value=[{'id':'r1','name':'示例房间'}])
        plugin=NS(prepare=AsyncMock(),actor=lambda e:manager,engine=engine,state=engine.state,
                  directory=engine.directory,gateway=engine.gateway,
                  effective=lambda scope:{'roomAliases':{},'defaultRoom':'r1','bindings':{}},
                  input=lambda e:('把 KU_TEST123 传送到基地',))
        tasks=TaskTools(plugin)
        result=await tasks.perform(event,'dst_run',capability_id='player.teleport',room='r1',
                                   player='KU_TEST123',parameters={'locationName':'基地'})
        self.assertIn('needs_input',result)
        self.assertIn('确认',result)
        self.assertEqual(panel.executions,2)
        # Even another model tool call in the same turn cannot supply approval.
        repeated=await tasks.perform(event,'dst_run',capability_id='player.teleport',room='r1',
                                     player='KU_TEST123',parameters={'locationName':'基地','confirmed':True})
        self.assertEqual(repeated,result)
        self.assertFalse(sends)
        human=replace(manager,event=NS(message_obj=NS(message_id='human-after-model')))
        await engine.handle(human,'确认',message_id='human-after-model')
        self.assertEqual(len(sends),1)

    async def test_wrong_quote_user_expiry_and_permission_never_submit(self):
        for case in ('quote','user','expiry','permission','cancel'):
            panel,engine,manager,sends=await self.flow()
            engine.state.form_sent(manager.key,'sent-confirmation',engine.state.pending_form(manager.key))
            manager=replace(manager,event=NS(message_obj=NS(message_id='reply')))
            if case=='user':manager=replace(manager,key=(*manager.key[:-1],'other'),user='other')
            if case=='expiry':engine.state.forms[manager.key]['previewUntil']=engine.state.clock()-1
            if case=='permission':manager=replace(manager,role='game-read')
            await engine.handle(manager,'取消' if case=='cancel' else '确认',quote='other-message' if case=='quote' else '',woken=True,message_id='reply')
            self.assertFalse(sends,case)

    def test_clear_commands_and_discussion_fallback(self):
        for text,expected in [('有哪些地点','room.locations'),('把玩家甲当前位置记为基地','location.save'),('把玩家甲传送到基地','player.teleport')]:
            self.assertEqual(parse(text,{}).capability,expected)
        for text in ('不要把玩家甲传送到基地','如果把玩家甲传送到基地','把玩家甲传送到基地，然后保存','为什么把玩家甲传送到基地'):
            self.assertIsNone(parse(text,{}),text)
        self.assertEqual(remaining({}),0)

    async def test_confirmation_private_http_route_is_single_request(self):
        calls=[]
        async def handler(req):
            calls.append(req)
            self.assertEqual(req.url.path,'/api/v2/rooms/r1/teleport-previews/00000000-0000-4000-8000-000000000001/execute')
            self.assertEqual(req.content,b'{}')
            return httpx.Response(200,json={'data':{'state':'confirmed'}})
        panel=PrivatePanel(config());panel.http=httpx.AsyncClient(transport=httpx.MockTransport(handler))
        result=await panel.call_tool('api_executeRoomTeleport',{'path':{'roomId':'r1','previewId':'00000000-0000-4000-8000-000000000001'},'body':{}})
        self.assertEqual(result.structuredContent['data']['state'],'confirmed')
        self.assertEqual(len(calls),1)
        await panel.close()

    async def test_legacy_moves_require_human_and_use_fixed_private_route(self):
        item = recipe('workbench.teleport_player', 'high')
        panel, engine = await self.fixture(item)
        manager = replace(actor(role='game-manage'), workbench_mode='all')
        intent = Intent('capability', capability=item['id'], parameters={'player_id':'KU_TEST123'})
        result = await engine.execute(manager, intent, Target('r1'))
        self.assertEqual(result.outcome, 'needs_selection')
        self.assertEqual(panel.executions, 0)
        await engine.handle(manager, '确认', message_id='legacy-confirm')
        self.assertEqual(panel.executions, 1)
        calls=[]
        async def handler(req):
            calls.append(req)
            self.assertEqual(req.url.path, '/api/v2/rooms/r1/bot/actions/workbench.teleport_player')
            self.assertEqual(req.headers['Idempotency-Key'], 'astrbot-example-request')
            return httpx.Response(200, json={'data':{'status':'succeeded','complete':True,'data':{'movement':{'state':'confirmed'}}}})
        private=PrivatePanel(config());private.http=httpx.AsyncClient(transport=httpx.MockTransport(handler))
        result=await private.call_tool('api_runBotCapability', {'path':{'roomId':'r1','capabilityId':item['id']},'body':{'parameters':{}},'requestId':'astrbot-example-request'})
        from astrbot_plugin_dst_companion.gateway import unpack
        self.assertTrue(unpack(result)['complete'])
        self.assertEqual(len(calls),1)
        await private.close()

    async def test_group_movement_previews_once_before_human_confirmation(self):
        item=recipe('workbench.relocate_players','high')
        item['parameters']['properties'].update(phase={'type':'string','enum':['preview','execute','cancel']},preview_id={'type':'string','maxLength':500})
        panel,engine=await self.fixture(item)
        original=engine.gateway.transport
        phases=[]
        async def transport(name,args,timeout,write):
            if name=='api_runBotCapability':
                result=await original(name,args,timeout,write)
                params=args['body']['parameters'];phases.append(params['phase'])
                move={'state':'preview' if params['phase']=='preview' else 'confirmed',
                      'previewId':'00000000-0000-4000-8000-000000000001','observedAt':100,'expiresAt':160,
                      'items':[{'playerId':'KU_TEST123','playerName':'Example','point':{'x':12,'z':24},'state':'prepared'}]}
                data={'status':'succeeded','complete':True,'resultKind':'workbench-run','data':{'movement':move}}
                panel.requests[args['requestId']]=data
                return mcp(data)
            return await original(name,args,timeout,write)
        engine.gateway.transport=transport
        manager=replace(actor(role='game-manage'),workbench_mode='all')
        result=await engine.execute(manager,Intent('capability',capability=item['id'],parameters={'player_id':'KU_TEST123','phase':'execute','preview_id':'model-forged'}),Target('r1'))
        self.assertEqual(result.outcome,'needs_selection',result.text)
        self.assertEqual(phases,['preview'])
        self.assertIn('Example',result.text)
        human=replace(manager,event=NS(message_obj=NS(message_id='human-batch-confirm')))
        result=await engine.handle(human,'确认',message_id='human-batch-confirm')
        self.assertEqual(phases,['preview','execute'],result.text)
