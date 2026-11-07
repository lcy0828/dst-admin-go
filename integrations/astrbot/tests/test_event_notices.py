"""Synthetic notice records only; no QQ, model, Steam or live game calls."""
import asyncio
import copy
import sys
import unittest
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock, patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from astrbot_plugin_dst_companion.broadcast import BroadcastEngine
from astrbot_plugin_dst_companion.event_notices import accept_page, poll_platform, poll_room, room_kinds, room_feed, platform_feed
from astrbot_plugin_dst_companion.message_templates import validate_templates, DEFINITIONS
from astrbot_plugin_dst_companion.queries import Queries
from astrbot_plugin_dst_companion.routing import parse
from astrbot_plugin_dst_companion.state import Target
from astrbot_plugin_dst_companion.streams import MCPReadError
from astrbot_plugin_dst_companion.jobs import Actions

NOW = 1800000000

def fact(key="event", category="maintenance", status="succeeded", age=0, **extra):
    import datetime
    at = datetime.datetime.fromtimestamp(NOW-age,datetime.timezone.utc).isoformat()
    kind = extra.pop("kind", {"maintenance":"room.operation", "updates":"game.available", "season":"world.season", "backup":"backup.result", "players":"player.death", "boss":"boss.killed", "mods":"mod.notice", "alerts":"runtime.alert"}.get(category,"room.operation"))
    return dict(id=key,roomId="example",category=category,kind=kind,status=status,title="示例操作",message="合成结果",occurredAt=at,**extra)

def page(items, cursor="next",more=False):
    return dict(items=items,nextCursor=cursor,hasMore=more)

class EventNoticeTests(unittest.IsolatedAsyncioTestCase):
    def engine(self, **options):
        self.saved=[]
        async def save(state): self.saved.append(copy.deepcopy(state))
        return BroadcastEngine(AsyncMock(),AsyncMock(return_value=True),save,{"example":"示例服"},options=options,clock=lambda:NOW)

    def actions(self, *, state="received", data=None):
        self.journal = {"request": dict(jobId="job", roomId="example", sessionId="qq:GroupMessage:123", connectionId="source", state=state, data=data or {})}
        async def load(): return copy.deepcopy(self.journal)
        async def save(records): self.journal = copy.deepcopy(records)
        return Actions(None, load, save, notify=AsyncMock())

    async def test_task_reply_and_broadcast_have_one_durable_sender(self):
        actions = self.actions()
        related = dict(jobId="job", roomId="example", status="partial")
        self.assertFalse(await actions.claim_notice("qq:GroupMessage:9_123", "source", related))
        self.assertTrue(await actions.claim_notice("qq:GroupMessage:123", "source", related))
        await actions._finish_watch("request", {"record": self.journal["request"]}, {"status":"failed", "outcome":"partial"})
        actions.notify.assert_not_awaited()
        self.assertEqual(self.journal["request"]["delivery"], "via_broadcast")

    async def test_finished_task_reply_suppresses_identical_notice_only_in_its_group(self):
        actions = self.actions(state="terminal", data={"status":"succeeded", "outcome":"full"})
        related = dict(jobId="job", roomId="example", status="succeeded")
        self.assertTrue(await actions.claim_notice("qq:GroupMessage:123", "source", related))
        self.assertFalse(await actions.claim_notice("qq:GroupMessage:456", "source", related))
        self.assertFalse(await actions.claim_notice("qq:GroupMessage:123", "changed-source", related))
        self.assertFalse(await actions.claim_notice("qq:GroupMessage:123", "source", {**related, "status":"uncertain"}))

    async def test_task_notice_claim_failure_does_not_mutate_persistent_journal(self):
        actions = self.actions()
        before = copy.deepcopy(self.journal)
        actions.save = AsyncMock(side_effect=OSError("disk unavailable"))
        with self.assertRaises(OSError):
            await actions.claim_notice("qq:GroupMessage:123", "source", dict(jobId="job", roomId="example", status="failed"))
        self.assertEqual(self.journal, before)

    async def test_disabled_update_notices_do_not_request_game_version_work(self):
        e = self.engine(maintenance=True)
        hub = NS(read=AsyncMock(return_value=page([])), wakeup=asyncio.Event())
        await poll_room(hub, "example", [e])
        query = hub.read.call_args.args[1]["query"]
        self.assertNotIn("game.available", query["kinds"])
        self.assertTrue(query["tail"])
        self.assertEqual(e.room("example")["notice_kinds"], query["kinds"])

    async def test_shared_category_filter_changes_seed_tail_without_replaying_history(self):
        first, second = self.engine(season=True), self.engine(updates=True)
        hub = NS(read=AsyncMock(return_value=page([])), wakeup=asyncio.Event())
        await poll_room(hub, "example", [first])
        hub.read.reset_mock()
        await poll_room(hub, "example", [first, second])
        calls = hub.read.call_args_list
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[0].args[1]["query"]["kinds"], calls[1].args[1]["query"]["kinds"])
        self.assertIn("game.available", room_kinds([first, second]))
        self.assertTrue(all(c.args[1]["query"]["tail"] for c in calls))
        self.assertFalse(first.state["outbox"])

    async def test_page_commits_cursor_outbox_and_deduplicates_after_restart(self):
        e=self.engine(maintenance=True)
        await accept_page(e,"example",page([fact()]))
        self.assertEqual(e.room("example")["notice_cursor"],"next")
        self.assertEqual(len(self.saved[-1]["outbox"]),1)
        e.state=copy.deepcopy(self.saved[-1])
        await accept_page(e,"example",page([fact()]))
        self.assertEqual(len(e.state["outbox"]),1)

    async def test_persistence_failure_rolls_back_cursor_and_outbox(self):
        e=self.engine(maintenance=True);before=copy.deepcopy(e.state);e.save=AsyncMock(side_effect=OSError())
        with self.assertRaises(OSError): await accept_page(e,"example",page([fact()]))
        self.assertEqual(e.state,before)

    async def test_cross_room_page_rejected_atomically(self):
        e=self.engine(maintenance=True);foreign=fact("other");foreign["roomId"]="other"
        before=copy.deepcopy(e.state)
        with self.assertRaises(MCPReadError): await accept_page(e,"example",page([fact(),foreign]))
        self.assertEqual(e.state,before)

    async def test_unknown_unversioned_and_disabled_categories_do_not_send(self):
        e=self.engine(season=True)
        await accept_page(e,"example",page([fact("a",category="future"),fact("b",category=""),fact("c",category="maintenance")]))
        self.assertFalse(e.state["outbox"])
        self.assertEqual(e.room("example")["notice_cursor"],"next")

    async def test_history_preserves_real_category_and_discards_old_countdown(self):
        e=self.engine(updates=True,season=True)
        await accept_page(e,"example",page([fact("a",category="updates",age=1000),fact("b",category="season",age=1000),fact("c",status="running",age=1000),fact("d",category="updates",age=90000)]))
        notices=e.state["outbox"]
        self.assertEqual({n["related"]["kind"] for n in notices},{"updates","season"})
        self.assertTrue(all("历史记录" in n["text"] for n in notices))

    async def test_backup_success_quiet_failure_visible(self):
        e=self.engine(backup=True)
        await accept_page(e,"example",page([fact("a",category="backup",status="succeeded"),fact("b",category="backup",status="failed")]))
        self.assertEqual(len(e.state["outbox"]),1)
        self.assertIn("失败",e.state["outbox"][0]["text"])

    async def test_maintenance_quiet_period_tracks_operation_until_finished(self):
        e=self.engine(maintenance=True)
        begin=fact(status="running",facts={"operationId":"op","action":"restart"})
        await accept_page(e,"example",page([begin]))
        self.assertEqual(e.room("example")["quiet_until"],NOW+3600)
        end=fact("end",facts={"operationId":"op","action":"restart"})
        await accept_page(e,"example",page([end],"next2"))
        self.assertEqual(e.room("example")["quiet_until"],NOW+e.grace)

    async def test_result_outbox_lives_longer_than_chat_and_unknown_send_not_retried(self):
        e=self.engine(maintenance=True)
        await accept_page(e,"example",page([fact()]))
        e.clock=lambda:NOW+600;e.emit=AsyncMock(side_effect=TimeoutError())
        await e.deliver_one()
        e.emit.assert_awaited_once()
        self.assertEqual(e.state["deliveries"][-1]["status"],"unknown")
        await e.deliver_one();e.emit.assert_awaited_once()

    async def test_software_baseline_stays_silent_across_multiple_pages_and_rounds(self):
        e=self.engine(software=True)
        h=NS(read=AsyncMock(side_effect=[page([fact("old1")],"c1",True),page([fact("old2")],"c2",True),page([],"c3",True),page([],"c4",True),page([],"c5")]))
        with patch("astrbot_plugin_dst_companion.event_notices.supports",return_value=False):
            await poll_platform(h,e);await poll_platform(h,e)
        self.assertFalse(e.state["outbox"])
        self.assertTrue(e.state["platform"]["software_seeded"])
        self.assertTrue(all("software.result" not in call.args[1]["query"]["kinds"] for call in h.read.await_args_list))
        self.assertNotIn("software_cursor",e.room("example"))

    async def test_software_record_has_system_label_and_independent_cursor(self):
        e=self.engine(software=True);item=fact();item.update(kind="software.result",roomId="")
        await accept_page(e,"example",page([item]),platform=True)
        self.assertIn("管理程序",e.state["outbox"][0]["text"])
        self.assertNotIn("notice_cursor",e.room("example"))
        self.assertEqual(e.state["platform"]["software_cursor"],"next")

    async def test_old_panel_feature_is_local_and_retries_without_model(self):
        e=self.engine(maintenance=True);h=NS(receivers=lambda _: [e],engines={"session":e},session_errors={},warn=lambda _:None)
        with patch("astrbot_plugin_dst_companion.event_notices.supports",return_value=False),patch("astrbot_plugin_dst_companion.event_notices.asyncio.sleep",side_effect=asyncio.CancelledError):
            with self.assertRaises(asyncio.CancelledError):await room_feed(h,"example")
        self.assertIn("原有播报继续",h.session_errors["session"])

    async def test_platform_disconnect_does_not_kill_other_feeds(self):
        e=self.engine(software=True);h=NS(engines={"session":e},warn=lambda _:None)
        with patch("astrbot_plugin_dst_companion.event_notices.supports",return_value=True),patch("astrbot_plugin_dst_companion.event_notices.poll_platform",side_effect=OSError()),patch("astrbot_plugin_dst_companion.event_notices.asyncio.sleep",side_effect=asyncio.CancelledError):
            with self.assertRaises(asyncio.CancelledError): await platform_feed(h)
        self.assertIs(h.engines["session"],e)

    async def test_recent_dynamic_read_uses_only_room_read_tool(self):
        gateway=NS(call=AsyncMock(return_value={"items":[fact()]}))
        result,target=await Queries(gateway).run(None,"recent",Target("example"),None,0)
        self.assertIn("合成结果",result)
        self.assertEqual(gateway.call.await_args.args[1],"api_listRoomRecentEvents")
        self.assertEqual(gateway.call.await_args.args[2]["path"],{"roomId":"example"})

    def test_event_template_cannot_hide_outcome_or_evaluate_expressions(self):
        for body in ("{body}", "{result}", "{result} {body} {body.__class__}"):
            with self.assertRaises(ValueError): validate_templates([dict(type="event_maintenance",body=body)])
        valid=validate_templates([dict(type="event_updates",body="{result}\n{body}")])
        self.assertIn("event_updates",valid)

    def test_recent_dynamic_phrases_route_without_model(self):
        for text in ("最近动态", "最近发生了什么", "1服最近更新记录"):
            intent=parse(text,{"1服":"example"})
            self.assertIsNotNone(intent,text)
            self.assertEqual(intent.kind,"recent")

    async def test_malformed_pages_and_limits_do_not_advance(self):
        e=self.engine(maintenance=True)
        for value in ({},page([{}]),page([fact()]*101),page([],"x"*1025)):
            with self.subTest(value=str(value)[:50]):
                before=copy.deepcopy(e.state)
                with self.assertRaises(MCPReadError):await accept_page(e,"example",value)
                self.assertEqual(e.state,before)

if __name__=="__main__":unittest.main()

class GameplayNoticeTests(unittest.IsolatedAsyncioTestCase):
    engine = EventNoticeTests.engine
    async def test_engine_requires_explicit_category_options_and_does_not_infer_subscriptions(self):
        e = self.engine()
        records = [fact(key=c, category=c) for c in ("players", "boss", "mods", "alerts")]
        await accept_page(e, "example", page(records))
        self.assertEqual(e.state["outbox"], [])
        self.assertEqual(room_kinds([e]), "")
        e = self.engine(players=True, boss=True, mods=True, alerts=True)
        await accept_page(e, "example", page(records))
        self.assertEqual({n["related"]["kind"] for n in e.state["outbox"]}, {"players", "boss", "mods", "alerts"})
        self.assertEqual(set(room_kinds([e]).split(",")), {"player.death", "player.respawned", "boss.killed", "mod.notice", "runtime.alert"})
        self.assertNotIn("game.available", room_kinds([e]))

    async def test_mislabelled_kind_does_not_bypass_category_switch(self):
        e = self.engine(players=True)
        await accept_page(e, "example", page([fact(category="players",kind="world.change")]))
        self.assertEqual(e.state["outbox"], [])

    async def test_old_gameplay_and_alerts_are_not_replayed_as_current_events(self):
        e = self.engine(players=True, boss=True, mods=True, alerts=True)
        await accept_page(e, "example", page([fact(key=c,category=c,age=601) for c in ("players", "boss", "mods", "alerts")]))
        self.assertEqual(e.state["outbox"], [])
        self.assertEqual(e.room("example")["notice_cursor"], "next")

    async def test_new_category_templates_and_player_reply_identity(self):
        e = self.engine(players=True)
        e.worlds["example"]={"caves":dict(role="caves")}
        event = fact(category="players",facts=dict(playerId="KU_EXAMPLE",playerName="示例玩家",worldId="caves"))
        await accept_page(e, "example", page([event]))
        notice=e.state["outbox"][0]
        self.assertIn("洞穴", notice["text"])
        self.assertEqual(notice["related"]["playerId"], "KU_EXAMPLE")
        self.assertEqual(notice["related"]["expiresAt"], NOW+600)
        for category in ("players", "boss", "mods", "alerts"):
            self.assertIn("event_"+category, DEFINITIONS)
            validate_templates([{"type":"event_"+category,"body":"{title}\n{result}\n{body}"}])

    async def test_queued_gameplay_expires_from_event_time(self):
        e=self.engine(players=True)
        await accept_page(e,"example",page([fact(category="players",age=590)]))
        e.clock=lambda:NOW+11
        await e.deliver_one()
        e.emit.assert_not_awaited()
        self.assertEqual(e.state["deliveries"][-1]["status"],"expired")

    async def test_older_panel_lacks_only_added_categories_and_does_not_receive_unknown_kinds(self):
        from astrbot_plugin_dst_companion.event_notices import compatible_kinds
        e = self.engine(mods=True, alerts=True, season=True)
        hub = NS(read=AsyncMock(return_value=page([])), engines={"group":e}, session_errors={})
        kinds = await compatible_kinds(hub,"example",[e])
        self.assertEqual(kinds,"world.season")
        self.assertNotIn("runtime.alert", kinds)
        self.assertIn("部分新增播报",hub.session_errors["group"])
        query=hub.read.call_args.args[1]["query"]
        self.assertEqual(query["kinds"],"room.operation")
        self.assertNotIn("game.available",query["kinds"])
        await compatible_kinds(hub,"example",[e])
        hub.read.assert_awaited_once()

    async def test_new_panel_capabilities_share_one_probe_and_only_enable_supported_kinds(self):
        from astrbot_plugin_dst_companion.event_notices import compatible_kinds
        first,second=self.engine(players=True),self.engine(boss=True)
        data={**page([]),"supportedKinds":["player.death","player.respawned"]}
        hub=NS(read=AsyncMock(return_value=data),engines={"one":first,"two":second},session_errors={})
        self.assertEqual(await compatible_kinds(hub,"example",[first,second]),"player.death,player.respawned")
        self.assertEqual(await compatible_kinds(hub,"example",[second]),"")
        hub.read.assert_awaited_once()

    async def test_saved_collection_policy_is_visible_without_implicitly_enabling_it(self):
        e=self.engine(players=True,boss=True)
        await accept_page(e,"example",{**page([]),"collection":{"players":"disabled","boss":"disabled","alerts":"enabled"}})
        self.assertEqual(e.room("example")["notice_collection"]["players"],"disabled")
        e.call.assert_not_awaited()
        before=copy.deepcopy(e.state)
        with self.assertRaises(MCPReadError):
            await accept_page(e,"example",{**page([]),"collection":{"private_setting":"enabled"}})
        self.assertEqual(e.state,before)

    async def test_stream_link_is_recreated_when_selected_categories_change(self):
        from astrbot_plugin_dst_companion.event_notices import stream_room
        from unittest.mock import Mock
        import datetime
        e=self.engine(season=True)
        e.room("example").update(notice_cursor="new-cursor",notice_kinds="world.season")
        resource={"expiresAt":datetime.datetime.fromtimestamp(NOW+120,datetime.timezone.utc).isoformat()}
        hub=NS(event_resources={"example":(NOW+120,{},"room.operation")},read=AsyncMock(return_value={"resource":resource}),
               plugin=NS(connections=NS(broadcast=lambda _:"server",configuration=lambda _:{})),http=NS(stream=Mock(side_effect=RuntimeError("synthetic stop"))))
        with patch("astrbot_plugin_dst_companion.event_notices.time.time",return_value=NOW),patch("astrbot_plugin_dst_companion.event_notices.resource_request",return_value=("http://example.invalid",{})):
            with self.assertRaisesRegex(RuntimeError,"synthetic stop"):
                await stream_room(hub,"example",[e])
        self.assertEqual(hub.read.call_args.args[1]["query"]["kinds"],"world.season")
        self.assertEqual(hub.event_resources["example"][2],"world.season")

    async def test_mod_message_backlog_preserves_maintenance_failure(self):
        e=self.engine(mods=True,maintenance=True)
        e.queue("failed-maintenance","维护失败",{"roomId":"example","kind":"maintenance","status":"failed"})
        for i in range(205):
            e.queue("mod/"+str(i),"普通模组消息",{"roomId":"example","kind":"mods"})
        self.assertLessEqual(len(e.state["outbox"]),200)
        self.assertIn("failed-maintenance",{item["key"] for item in e.state["outbox"]})
