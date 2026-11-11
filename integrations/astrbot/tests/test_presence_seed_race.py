"""Membership snapshots must not swallow pending Runtime joins/leaves."""
import asyncio
import unittest
from unittest.mock import AsyncMock
from test_chat_stream import record
from astrbot_plugin_dst_companion.broadcast import BroadcastEngine
from astrbot_plugin_dst_companion.chat_stream import accept_chat


class PresenceSeedRaceTests(unittest.IsolatedAsyncioTestCase):
    def fixture(self):
        players = []
        now = [1000]
        async def call(name, args):
            if name == 'api_listRoomWorlds':
                return {'items':[{'id':'master','name':'Master','role':'master'}]}
            if name == 'api_listRoomPlayers':
                return {'items':players.copy(), 'total':len(players), 'staleOnline':False}
            if name == 'api_getRoomPlayer':
                return players[0] if players else {}
            return {}
        engine = BroadcastEngine(call, None, AsyncMock(), {'room':'示例房间'}, clock=lambda:now[0],
                                 emit_notice=AsyncMock(return_value='qq-synthetic'))
        return engine, players, now

    def event(self, kind='player.joined', cursor='one'):
        return {'cursor':cursor, 'event':{'kind':kind,'occurredAtUnix':1000,'fields':{'userId':'KU_EXAMPLE'}}}

    async def test_periodic_snapshot_ahead_of_join_still_delivers_one_join(self):
        engine, players, now = self.fixture()
        await engine.seed('room')
        players.append({'id':'KU_EXAMPLE','name':'Example','online':True,'presenceStatus':'live',
                        'worldConfirmed':True,'worldId':'master'})
        await engine.seed('room')
        await engine.accept('room','master','runtime.event',self.event())
        now[0] += 4
        await engine.resolve_presence()
        await engine.deliver_one()
        await engine.accept('room','master','runtime.event',self.event())
        await engine.resolve_presence()
        await engine.deliver_one()
        self.assertEqual(engine.emit_notice.await_count,1)
        self.assertIn('玩家加入', engine.emit_notice.call_args.args[0])

    async def test_startup_existing_players_and_periodic_stale_snapshot_do_not_create_join(self):
        engine, players, _ = self.fixture()
        players.append({'id':'KU_EXAMPLE','online':True,'presenceStatus':'live','worldConfirmed':True,'worldId':'master'})
        await engine.seed('room')
        self.assertFalse(engine.room('room')['pending'])
        players.clear()
        await engine.seed('room')
        self.assertIn('KU_EXAMPLE',engine.room('room')['online'])
        await engine.accept('room','master','runtime.event',self.event('player.joined'))
        self.assertFalse(engine.room('room')['pending'])

    async def test_event_during_seed_read_wins_over_old_snapshot(self):
        engine, _, _ = self.fixture()
        original = engine.call
        entered, finish = asyncio.Event(), asyncio.Event()
        async def slow(name,args):
            if name == 'api_listRoomPlayers':
                entered.set()
                await finish.wait()
            return await original(name,args)
        engine.call = slow
        seed = asyncio.create_task(engine.seed('room'))
        await asyncio.wait_for(entered.wait(),1)
        await engine.accept('room','master','runtime.event',self.event())
        finish.set()
        await seed
        self.assertIn('KU_EXAMPLE',engine.room('room')['online'])
        self.assertIn('KU_EXAMPLE',engine.room('room')['pending'])

    async def test_reset_during_seed_keeps_reseed_until_a_fresh_snapshot(self):
        engine, players, _ = self.fixture()
        await engine.seed('room')
        original = engine.call
        entered, finish = asyncio.Event(), asyncio.Event()
        async def slow(name,args):
            if name == 'api_listRoomPlayers':
                entered.set()
                await finish.wait()
            return await original(name,args)
        engine.call = slow
        seed = asyncio.create_task(engine.seed('room'))
        await asyncio.wait_for(entered.wait(),1)
        await engine.accept('room','master','runtime.reset',{'cursor':'reset'})
        finish.set()
        await seed
        self.assertTrue(engine.room('room')['presence_reseed'])
        players.append({'id':'KU_EXAMPLE','online':True,'presenceStatus':'live',
                        'worldConfirmed':True,'worldId':'master'})
        await engine.seed('room')
        self.assertNotIn('presence_reseed',engine.room('room'))
        self.assertIn('KU_EXAMPLE',engine.room('room')['online'])

    async def test_chat_replay_after_delivery_reload_and_update_never_resends(self):
        engine, _, _ = self.fixture()
        await accept_chat(engine,'room','chat.record','one',record())
        await engine.deliver_one()
        restored = BroadcastEngine(engine.call,None,AsyncMock(),engine.rooms,state=engine.state,
                                  clock=engine.clock,emit_notice=engine.emit_notice)
        await accept_chat(restored,'room','chat.record','replayed',record())
        await accept_chat(restored,'room','chat.record','updated',record(operation='updated'))
        await restored.deliver_one()
        self.assertEqual(engine.emit_notice.await_count,1)
