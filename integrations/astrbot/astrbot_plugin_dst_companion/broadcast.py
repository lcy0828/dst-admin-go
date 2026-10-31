"""MCP-only game notifications. Transport and QQ delivery are injected."""

from __future__ import annotations

import asyncio
import copy
import re
import time
from datetime import datetime, timezone
from typing import Any

from .display import room_label, time_text
from .display import text as display_text
from .message_templates import DEFINITIONS, render_text

READ_TOOLS = frozenset(
    {
        "api_listRoomWorlds",
        "api_getRoom",
        "api_listRoomPlayers",
        "api_getRoomPlayer",
        "api_listWorldStates",
        "api_streamWorldRuntimeEvents",
        "api_listRoomRuntimeEvents",
        "api_listRoomChatLogs",
    }
)
CHARACTERS = {
    "wilson": "威尔逊",
    "willow": "薇洛",
    "wolfgang": "沃尔夫冈",
    "wendy": "温蒂",
    "wx78": "WX-78",
    "wickerbottom": "薇克巴顿",
    "woodie": "伍迪",
    "wes": "韦斯",
    "waxwell": "麦斯威尔",
    "wathgrithr": "薇格弗德",
    "webber": "韦伯",
    "winona": "薇诺娜",
    "warly": "沃利",
    "wortox": "沃拓克斯",
    "wormwood": "沃姆伍德",
    "wurt": "沃特",
    "walter": "沃尔特",
    "wanda": "旺达",
    "wonkey": "芜猴",
    "wixie": "薇茜",
    "winky": "温奇",
}
SEASONS = {"spring": "🌱", "summer": "☀️", "autumn": "🍂", "winter": "❄️"}


def clean(value: Any, length: int = 80) -> str:
    return display_text(value, length)


def chat_label(value):
    # Game font icons have no portable QQ glyph. Keep the source intact and
    # shorten only this display label, never the room identity or player query.
    return room_label(value)


def timestamp(value: Any) -> float:
    try:
        if isinstance(value, (int, float)):
            return float(value)
        return datetime.fromisoformat(str(value).replace("Z", "+00:00")).timestamp()
    except (TypeError, ValueError, OverflowError):
        return 0


def presence_text(label, kind, profile, world, stats):
    head = "🚪 有人来营地啦喵～" if kind == "player.joined" else "👋 有人先走啦喵～"
    prefab = clean(profile.get("prefab"))
    character = CHARACTERS.get(prefab, prefab or "选人中")
    name = clean(profile.get("name")) or clean(profile.get("id")) or "未知玩家"
    lines = [
        head,
        "────────────",
        f"{clean(label)} · 〖{name}〗·{character}·{clean(world) or '分片未知'}",
    ]
    age = profile.get("age")
    if isinstance(age, int) and not isinstance(age, bool) and age > 0:
        lines.append(f"角色年龄： {age} 天")
    bits = []
    if isinstance(stats.get("online"), int):
        capacity = stats.get("max_players")
        count = str(stats["online"]) + (
            f"/{capacity}" if isinstance(capacity, int) and capacity > 0 else ""
        )
        bits.append(f"在线 {count} 人")
    if isinstance(stats.get("day"), int):
        bits.append(
            f"{stats['day']} 天"
            + (f" {SEASONS[stats['season']]}" if stats.get("season") in SEASONS else "")
        )
    if bits:
        lines.append(" · ".join(bits))
    return "\n".join(lines)


class BroadcastEngine:
    def __init__(
        self,
        call,
        emit,
        save,
        rooms,
        options=None,
        state=None,
        clock=time.time,
        log=None,
        emit_notice=None,
        message_renderer=None,
    ):
        self.call, self.emit, self.save = call, emit, save
        self.emit_notice = emit_notice
        self.message_renderer = message_renderer
        self.rooms, self.options, self.clock = rooms, options or {}, clock
        self.log = log or (lambda message: None)
        self.state = copy.deepcopy(state) if isinstance(state, dict) else {}
        self.state.setdefault("version", 1)
        for key, default in (
            ("cursors", {}),
            ("rooms", {}),
            ("seen", []),
            ("outbox", []),
            ("deliveries", []),
            ("references", []),
            ("presence_decisions", []),
        ):
            self.state.setdefault(key, default)
        # A crash after submitting to QQ has an unknown outcome. Never replay it.
        for delivery in self.state["deliveries"]:
            if delivery.get("status") == "sending":
                delivery["status"] = "unknown"
        self.lock = asyncio.Lock()
        self.worlds = {}
        self.cache = {}
        self.grace = max(5, min(60, int(self.options.get("leave_grace_seconds", 10))))

    def message(self, room_id, kind, values):
        label = chat_label(self.rooms[room_id])
        if sum(chat_label(name) == label for name in self.rooms.values()) > 1:
            label += " · " + clean(room_id, 128)
        values = {"room_name": label, "room_full_name": clean(self.rooms[room_id], 255), **values}
        if self.message_renderer:
            return self.message_renderer(kind, values)
        return render_text(kind, DEFINITIONS[kind].default, values)

    def room(self, room_id):
        return self.state["rooms"].setdefault(
            room_id,
            {
                "profiles": {},
                "online": {},
                "pending": {},
                "quiet_until": 0,
                "audit_seen": [],
                "audit_seeded": False,
                "chat_seen": [],
                "chat_seeded": False,
            },
        )

    async def persist(self):
        self.state["seen"] = self.state["seen"][-1024:]
        self.state["deliveries"] = self.state["deliveries"][-100:]
        self.state["references"] = [
            row for row in self.state["references"] if 0 <= self.clock() - row.get("at", 0) <= 86400
        ][-128:]
        self.state["presence_decisions"] = self.state["presence_decisions"][-100:]
        for room in self.state["rooms"].values():
            if len(room["profiles"]) > 256:
                ordered = sorted(
                    room["profiles"], key=lambda uid: room["profiles"][uid].get("_observed", 0)
                )
                for uid in ordered[:-256]:
                    room["profiles"].pop(uid, None)
        await self.save(copy.deepcopy(self.state))

    def presence_decision(self, room_id, event, reason):
        self.state["presence_decisions"].append(
            {
                "at": self.clock(),
                "room": room_id,
                "world": event.get("world"),
                "kind": event.get("kind"),
                "key": event.get("key"),
                "reason": reason,
            }
        )

    def pending_presence(self, key, kind, world, occurred_at, delay):
        return {
            "key": key,
            "kind": kind,
            "world": world,
            "due": self.clock() + delay,
            "occurred_at": occurred_at,
            "expires_at": self.clock() + 120,
            "confirm_until": self.clock() + 60,
        }

    def world_name(self, room_id, world_id):
        item = self.worlds.get(room_id, {}).get(world_id, {})
        role = item.get("role") or item.get("type")
        if role in ("master", "Master") or item.get("isMaster"):
            return "地面"
        if role in ("caves", "Caves"):
            return "洞穴"
        return clean(item.get("name")) or "分片未知"

    def merge_profile(self, room_id, item):
        uid = str(item.get("id") or "")
        if not uid:
            return
        profiles = self.room(room_id)["profiles"]
        old = profiles.get(uid, {})
        if old.get("sessionId") and item.get("sessionId") and old["sessionId"] != item["sessionId"]:
            old = {}
        if (
            item.get("prefab")
            and old.get("prefab")
            and item["prefab"] != old["prefab"]
            and "age" not in item
        ):
            old = {key: value for key, value in old.items() if key != "age"}
        profiles[uid] = {**old, **item, "_observed": self.clock()}

    def quiet_presence(self, room_id, duration, world_id=None):
        room = self.room(room_id)
        room["quiet_until"] = max(room["quiet_until"], self.clock() + duration)
        room["pending"].clear()
        self.state["outbox"] = [
            x for x in self.state["outbox"] if not x["key"].startswith(room_id + "/")
        ]
        if world_id is None:
            room["online"].clear()
        else:
            for uid, active in list(room["online"].items()):
                remaining = [world for world in active if world not in (world_id, "unknown")]
                if remaining:
                    room["online"][uid] = remaining
                else:
                    room["online"].pop(uid, None)

    async def seed(self, room_id):
        worlds = await self.call("api_listRoomWorlds", {"path": {"roomId": room_id}})
        self.worlds[room_id] = {item["id"]: item for item in worlds.get("items", [])}
        # Current presence replaces stale persisted membership; pending events survive.
        data = await self.call(
            "api_listRoomPlayers",
            {
                "path": {"roomId": room_id},
                "query": {"status": "online", "includeAccessLists": False, "limit": 100},
            },
        )
        async with self.lock:
            room = self.room(room_id)
            if (
                not data.get("staleOnline")
                and data.get("total", 0) <= 100
                and all(item.get("presenceStatus") == "live" for item in data.get("items", []))
            ):
                room["online"].clear()
            for item in data.get("items", []):
                self.merge_profile(room_id, item)
                if item.get("online") and item.get("presenceStatus") == "live":
                    world = item.get("worldId") if item.get("worldConfirmed") else "unknown"
                    room["online"][item["id"]] = [world or "unknown"]
            await self.persist()
        return list(self.worlds[room_id])

    async def accept(self, room_id, world_id, event_name, payload, event_id=""):
        cursor = str(payload.get("cursor") or event_id or "")
        stream = room_id + "/" + world_id
        async with self.lock:
            room = self.room(room_id)
            if event_name in ("runtime.gap", "runtime.reset"):
                self.quiet_presence(room_id, self.grace)
                self.log(f"{self.rooms[room_id]} 事件源重置或出现缺口，抑制迁移与重放播报")
            if event_name != "runtime.event":
                if cursor:
                    self.state["cursors"][stream] = cursor
                await self.persist()
                return
            key = stream + "/" + cursor
            if not cursor or key in self.state["seen"]:
                return
            event = payload.get("event") or {}
            self.state["seen"].append(key)
            self.state["cursors"][stream] = cursor
            kind = event.get("kind")
            age = self.clock() - timestamp(event.get("occurredAtUnix"))
            if not self.options.get("presence", True) or not -30 <= age <= 120:
                if kind in ("player.joined", "player.left"):
                    self.presence_decision(
                        room_id, {"key": key, "kind": kind, "world": world_id}, "stale_event"
                    )
                await self.persist()
                return
            if kind in ("world.shutdown", "world.reset"):
                self.quiet_presence(room_id, 60, world_id)
            fields = event.get("fields") or {}
            uid = str(fields.get("userId") or "")
            if kind not in ("player.joined", "player.left") or not re.fullmatch(
                r"[A-Za-z0-9_-]{1,128}", uid
            ):
                await self.persist()
                return
            self.merge_profile(
                room_id,
                {"id": uid, **({"prefab": fields["prefab"]} if fields.get("prefab") else {})},
            )
            active = set(room["online"].get(uid, []))
            pending = room["pending"].get(uid)
            quiet = room["quiet_until"] > self.clock()
            if kind == "player.joined":
                was_online = bool(active)
                active.discard("unknown")
                active.add(world_id)
                room["online"][uid] = sorted(active)
                if pending and pending["kind"] == "player.left":
                    room["pending"].pop(uid, None)
                elif not was_online and not pending and not quiet:
                    room["pending"][uid] = self.pending_presence(
                        key,
                        kind,
                        world_id,
                        timestamp(event.get("occurredAtUnix")),
                        3,
                    )
            else:
                active.discard(world_id)
                active.discard("unknown")
                if active:
                    room["online"][uid] = sorted(active)
                else:
                    room["online"].pop(uid, None)
                if pending and pending["kind"] == "player.joined":
                    room["pending"].pop(uid, None)
                elif not active and not quiet and not pending:
                    room["pending"][uid] = self.pending_presence(
                        key,
                        kind,
                        world_id,
                        timestamp(event.get("occurredAtUnix")),
                        self.grace,
                    )
            if len(room["pending"]) > 128:
                oldest = min(room["pending"], key=lambda uid: room["pending"][uid]["due"])
                room["pending"].pop(oldest)
                self.log("进出事件积压超过上限，已丢弃最旧的待确认事件")
            await self.persist()

    async def stats(self, room_id):
        previous = self.cache.get(room_id)
        if previous and self.clock() - previous[0] < 3:
            return previous[1]
        result = {}
        try:
            players = []
            complete = False
            healthy = True
            for offset in range(0, 500, 100):
                data = await self.call(
                    "api_listRoomPlayers",
                    {
                        "path": {"roomId": room_id},
                        "query": {
                            "status": "online",
                            "includeAccessLists": False,
                            "limit": 100,
                            "offset": offset,
                        },
                    },
                )
                items = data.get("items", [])
                players.extend(items)
                healthy = healthy and not data.get("staleOnline")
                if len(items) < 100 or offset + len(items) >= data.get("total", 0):
                    complete = True
                    break
            if complete and healthy and all(p.get("presenceStatus") == "live" for p in players):
                result["online"] = len(
                    {p["id"] for p in players if p.get("online") and p.get("id")}
                )
            async with self.lock:
                for player in players:
                    self.merge_profile(room_id, player)
        except Exception:
            self.log("在线人数查询暂不可用，本次通知省略人数")
        try:
            data = await self.call("api_listWorldStates", {"path": {"roomId": room_id}})
            items = data.get("items", [])
            world = next(
                (x for x in items if x.get("worldRole") == "master"), items[0] if items else {}
            )
            cycles = world.get("cycles")
            if not world.get("stale", True) and isinstance(cycles, int) and cycles >= 0:
                result.update(day=cycles + 1, season=world.get("season", ""))
        except Exception:
            self.log("世界状态查询暂不可用，本次通知省略世界天数")
        try:
            room = await self.call("api_getRoom", {"path": {"roomId": room_id}})
            capacity = room.get("maxPlayers")
            if isinstance(capacity, int) and capacity > 0:
                result["max_players"] = capacity
        except Exception:
            pass
        self.cache[room_id] = (self.clock(), result)
        return result

    def queue(self, key, text, related=None):
        if any(item["key"] == key for item in self.state["outbox"]):
            return
        self.state["outbox"].append(
            {"key": key, "text": text, "created": self.clock(), "related": related}
        )
        if len(self.state["outbox"]) > 200:
            self.state["outbox"].pop(0)
            self.log("通知积压超过上限，已丢弃最旧通知")

    async def resolve_presence(self):
        async with self.lock:
            pending = [
                (rid, uid, dict(event))
                for rid in self.rooms
                for uid, event in self.room(rid)["pending"].items()
                if event["due"] <= self.clock()
            ]
        for room_id, uid, event in pending[:10]:
            profile = {}
            try:
                profile = await self.call(
                    "api_getRoomPlayer", {"path": {"roomId": room_id, "playerId": uid}}
                )
            except Exception:
                pass
            stats = await self.stats(room_id)
            async with self.lock:
                room = self.room(room_id)
                if room["pending"].get(uid, {}).get("key") != event["key"]:
                    continue
                if profile:
                    self.merge_profile(room_id, profile)
                expires = event.get("expires_at", event["due"] + 120)
                if self.clock() > expires or room["quiet_until"] > self.clock():
                    room["pending"].pop(uid)
                    self.presence_decision(room_id, event, "expired_or_maintenance")
                    await self.persist()
                    continue
                live = profile.get("presenceStatus") == "live"
                if event["kind"] == "player.left" and room["online"].get(uid):
                    room["pending"].pop(uid)
                    self.presence_decision(room_id, event, "migration_or_reconnection")
                    await self.persist()
                    continue
                # A cached query can still be marked live while describing the
                # state before the event. It cannot veto a newer Runtime event.
                field = (profile.get("fields") or {}).get("online") or {}
                observed = timestamp(field.get("observedAt") or profile.get("presenceObservedAt"))
                before_event = (
                    observed and event.get("occurred_at") and observed < event["occurred_at"]
                )
                contradicts = (
                    live
                    and not before_event
                    and (
                        event["kind"] == "player.left"
                        and profile.get("online") is True
                        or event["kind"] == "player.joined"
                        and profile.get("online") is False
                    )
                )
                if contradicts:
                    current = room["pending"][uid]
                    confirm_until = current.setdefault("confirm_until", event["due"] + 60)
                    current.setdefault("expires_at", expires)
                    if self.clock() < confirm_until:
                        current["due"] = self.clock() + 5
                        self.presence_decision(room_id, event, "awaiting_presence_refresh")
                        self.log(
                            f"{self.rooms[room_id]} 在线查询与进退事件暂不一致，5 秒后重新确认"
                        )
                    else:
                        room["pending"].pop(uid)
                        self.presence_decision(room_id, event, "newer_presence_contradicts_event")
                    await self.persist()
                    continue
                room["pending"].pop(uid)
                record = room["profiles"].get(uid, {"id": uid})
                render_stats = dict(stats)
                if before_event and (
                    event["kind"] == "player.left"
                    and profile.get("online") is True
                    or event["kind"] == "player.joined"
                    and profile.get("online") is False
                ):
                    render_stats.pop("online", None)
                world = self.world_name(room_id, event["world"])
                old_text = presence_text(
                    self.rooms[room_id], event["kind"], record, world, render_stats
                )
                prefab = clean(record.get("prefab"))
                text = self.message(
                    room_id,
                    "player_join" if event["kind"] == "player.joined" else "player_leave",
                    {
                        "player_name": clean(record.get("name") or record.get("id")) or "未知玩家",
                        "character": CHARACTERS.get(prefab, prefab or "选人中"),
                        "world_name": world,
                        "player_age": record.get("age", "未确认"),
                        "online": render_stats.get("online", "未确认"),
                        "max_players": render_stats.get("max_players", "未确认"),
                        "world_day": render_stats.get("day", "未确认"),
                        "season": SEASONS.get(render_stats.get("season"), "未确认"),
                        "details": "\n".join(old_text.splitlines()[3:])
                        or "在线人数与世界信息暂未确认。",
                        "time": time_text(
                            datetime.fromtimestamp(
                                event["occurred_at"], tz=timezone.utc
                            ).isoformat()
                        ),
                    },
                )
                self.queue(
                    event["key"],
                    text,
                    {
                        "roomId": room_id,
                        "playerId": uid,
                        "name": record.get("name", ""),
                        "kind": "player",
                    },
                )
                self.presence_decision(room_id, event, "queued_for_delivery")
                await self.persist()

    async def poll_audit(self, room_id):
        data = await self.call(
            "api_listRoomRuntimeEvents", {"path": {"roomId": room_id}, "query": {"limit": 100}}
        )
        items = data.get("items", [])
        async with self.lock:
            room = self.room(room_id)
            seen = set(room["audit_seen"])
            first = not room["audit_seeded"]
            for event in reversed(items):
                key = str(event.get("id") or "")
                if not key or key in seen:
                    continue
                seen.add(key)
                room["audit_seen"].append(key)
                kind = event.get("type")
                fresh = -30 <= self.clock() - timestamp(event.get("occurredAt")) <= 120
                if (
                    kind in ("stop_requested", "restart_requested", "stopped", "unexpected_exit")
                    and fresh
                ):
                    self.quiet_presence(room_id, 60, str(event.get("worldId") or "") or None)
                if first or not fresh or kind not in ("running", "stopped", "unexpected_exit"):
                    continue
                where = self.world_name(room_id, str(event.get("worldId") or ""))
                reason = (
                    clean(event.get("message") or event.get("reasonCode"), 160) or "退出原因未确认"
                )
                text = self.message(
                    room_id,
                    {
                        "running": "server_start",
                        "stopped": "server_stop",
                        "unexpected_exit": "server_error",
                    }[kind],
                    {
                        "world_name": where,
                        "reason": reason,
                        "time": time_text(event.get("occurredAt")),
                    },
                )
                self.queue(
                    "audit/" + room_id + "/" + key, text, {"roomId": room_id, "kind": "server"}
                )
            room["audit_seeded"] = True
            room["audit_seen"] = room["audit_seen"][-256:]
            await self.persist()

    async def poll_chat(self, room_id):
        async with self.lock:
            known = set(self.room(room_id)["chat_seen"])
            first = not self.room(room_id)["chat_seeded"]
        found = []
        for offset in range(0, 400, 100):
            data = await self.call(
                "api_listRoomChatLogs",
                {
                    "path": {"roomId": room_id},
                    "query": {"kind": "say", "limit": 100, "offset": offset},
                },
            )
            items = data.get("items", [])
            found.extend(items)
            if first or any(str(x.get("id")) in known for x in items) or len(items) < 100:
                break
        async with self.lock:
            room = self.room(room_id)
            seen = set(room["chat_seen"])
            changed = first
            batch = []
            batch_size = 0

            def flush():
                if not batch:
                    return
                text = self.message(
                    room_id,
                    "chat_batch",
                    {
                        "lines": "\n".join(row[1] for row in batch),
                        "time": batch[-1][2],
                    },
                )
                self.queue(
                    "chat/" + room_id + "/" + batch[0][0],
                    text,
                    {"roomId": room_id, "kind": "chat", "chatCount": len(batch)},
                )
                batch.clear()

            for item in reversed(found):
                key = str(item.get("id") or "")
                if not key or key in seen:
                    continue
                seen.add(key)
                room["chat_seen"].append(key)
                changed = True
                stamp = timestamp(item.get("occurredAt"))
                if (
                    first
                    or item.get("kind") != "say"
                    or not stamp
                    or not -30 <= self.clock() - stamp <= 120
                ):
                    continue
                compact = self.message(
                    room_id,
                    "chat_line",
                    {
                        "player_name": clean(item.get("playerName")) or "未知玩家",
                        "message": clean(item.get("content"), 1000),
                        "time": time_text(item.get("occurredAt")),
                    },
                )
                if batch and (len(batch) >= 5 or batch_size + len(compact) > 2600):
                    flush()
                    batch_size = 0
                batch.append((key, compact, time_text(item.get("occurredAt"))))
                batch_size += len(compact) + 2
            flush()
            room["chat_seeded"] = True
            room["chat_seen"] = room["chat_seen"][-1000:]
            if changed:
                await self.persist()

    async def deliver_one(self):
        async with self.lock:
            if not self.state["outbox"]:
                return
            notice = self.state["outbox"].pop(0)
            delivery = {"key": notice["key"], "status": "sending", "at": self.clock()}
            self.state["deliveries"].append(delivery)
            try:
                await self.persist()
            except BaseException:
                self.state["outbox"].insert(0, notice)
                self.state["deliveries"].remove(delivery)
                raise
        outcome = "expired"
        if self.clock() - notice["created"] <= 120:
            try:
                sent = (
                    await self.emit_notice(notice["text"], notice.get("related"))
                    if self.emit_notice
                    else await self.emit(notice["text"])
                )
                outcome = "sent" if sent else "not_sent"
                related = notice.get("related", {})
                if (
                    isinstance(sent, dict)
                    and isinstance(sent.get("messageId"), str)
                    and isinstance(sent.get("selfId"), str)
                    and related.get("playerId")
                ):
                    async with self.lock:
                        self.state["references"].append(
                            {
                                **sent,
                                "at": self.clock(),
                                "related": {
                                    k: related[k]
                                    for k in ("roomId", "playerId", "name")
                                    if k in related
                                },
                            }
                        )
                if outcome == "not_sent":
                    self.log("未找到目标 QQ 平台，通知未发送；请检查平台 ID")
            except Exception:
                outcome = "unknown"
                self.log("QQ 投递结果不确定，未自动重发，避免重复通知")
        async with self.lock:
            delivery["status"] = outcome
            await self.persist()
