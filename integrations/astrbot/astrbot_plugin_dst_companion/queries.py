"""Fixed MCP read recipes, independent of chat syntax and the AstrBot framework."""

import asyncio
import time
from datetime import datetime, timedelta, timezone

from . import formatters as fmt
from .gateway import QueryError
from .state import Target


class Queries:
    def __init__(self, gateway, reader=None):
        self.gateway = gateway
        self.reader = reader

    async def call(self, actor, name, arguments, deadline):
        if self.reader:
            result = await self.reader(actor, name, arguments, deadline)
            if result is not NotImplemented:
                return result
        return await self.gateway.call(actor, name, arguments, deadline)

    async def rooms(self, actor, deadline):
        data = await self.call(actor, "api_listRooms", {}, deadline)
        rows = data.get("items")
        if not isinstance(rows, list):
            raise QueryError("MCP_RESPONSE_INVALID")
        return [r for r in rows if r.get("id") in actor.rooms]

    async def resolve(self, actor, room, player, deadline):
        result = await self.call(
            actor,
            "api_resolveRoomPlayer",
            {"path": {"roomId": room}, "query": {"query": player}},
            deadline,
        )
        # “玩家小明” may mean the full nickname or “the player 小明”. Never
        # discard a real full-name match, an ambiguity, or a read failure.
        if result.get("status") == "not_found" and isinstance(player, str) and player.startswith("玩家"):
            nickname = player[2:].strip()
            if nickname:
                return await self.call(
                    actor, "api_resolveRoomPlayer",
                    {"path": {"roomId": room}, "query": {"query": nickname}}, deadline,
                )
        return result

    async def run(self, actor, kind, target, player, deadline, *, room=None, compact=False, parameters=None):
        path = {"roomId": target.room}
        parameters = parameters or {}

        async def call(name, query=None):
            args = {"path": path}
            if query is not None:
                args["query"] = query
            return await self.call(actor, name, args, deadline)

        if kind == "recent":
            data = await call("api_listRoomRecentEvents", {"limit": 10})
            lines = ["📋 最近动态"]
            for item in data.get("items", []):
                if not item.get("category"):
                    continue
                lines.append(fmt.time_text(item.get("occurredAt")) + " · " + fmt.safe(item.get("title", "动态")))
                lines.append(fmt.safe(item.get("message", ""))[:500])
            if len(lines) == 1:
                lines.append("暂无可展示记录。")
            lines.append("仅显示已采集记录，缺失不代表事件未发生。")
            return "\n".join(lines), target
        if kind == "status":
            # Independent reads share Gateway's global limit and one deadline.
            worlds, states = await asyncio.gather(
                call("api_listRoomWorlds"), call("api_listWorldStates"), return_exceptions=True
            )
            # Preserve the original timeout/permission failure instead of
            # wrapping it as an ExceptionGroup and calling it malformed data.
            for value in (worlds, states):
                if isinstance(value, BaseException):
                    raise value
            parts = [fmt.status(worlds, states, include_sample=False)]
            reads = []
            if "players" in actor.visibility and "room.players" not in actor.disabled_capabilities:
                reads.append(
                    (
                        "players",
                        call(
                            "api_listRoomPlayers",
                            {"status": "online", "includeAccessLists": False, "limit": 100},
                        ),
                    )
                )
            if "mods" in actor.visibility and "room.mods" not in actor.disabled_capabilities:
                reads.append(("mods", call("api_listRoomMods")))
            values = await asyncio.gather(*(read for _, read in reads), return_exceptions=True)
            for (section, _), data in zip(reads, values, strict=True):
                if isinstance(data, Exception):
                    parts.append(
                        ("👥 在线玩家" if section == "players" else "📦 模组")
                        + "：暂未读取成功，数量未确认"
                    )
                elif section == "players":
                    parts.append(
                        fmt.players(
                            data, (room or {}).get("maxPlayers"), limit=2 if compact else 10
                        )
                    )
                else:
                    parts.append(fmt.mods(data, limit=2 if compact else 12))
            parts.append(fmt.sample_time(worlds, states))
            return "\n\n".join(parts), target
        if kind in {"players", "network"}:
            data = (
                None
                if player
                else await call(
                    "api_listRoomPlayers",
                    {"status": "online", "includeAccessLists": False, "limit": 100},
                )
            )
            if kind == "network":
                return fmt.network([player] if player else data.get("items", [])), target
            rows = [
                p
                for p in data.get("items", [])
                if p.get("online") and p.get("presenceStatus") == "live"
            ]
            if len(rows) == 1:
                target = Target(target.room, rows[0]["id"], rows[0].get("name", ""))
            return fmt.players(data, (room or {}).get("maxPlayers")), target
        if kind == "profile":
            return (
                fmt.player_line(player)
                + "\n🕒 最近观测："
                + fmt.time_text(
                    player.get("presenceObservedAt") or player.get("lastSeenAt"), year=True
                )
                + "（北京时间）"
                + "\n所在世界以确认信息为准；未采集实时坐标时不猜测坐标。"
            ), target
        if kind == "inventory":
            query = {"source": parameters.get("source", "auto"), "refresh": False}
            for key in ("worldId", "sourceId"):
                if key in parameters:
                    query[key] = parameters[key]
            data = await self.call(
                actor,
                "api_getRoomPlayerInventory",
                {
                    "path": {**path, "playerId": target.player},
                    "query": query,
                },
                deadline,
            )
            return fmt.safe(target.name) + "\n" + fmt.inventory(data), target
        if kind in {"activity", "trajectory", "combat"}:
            now = datetime.now(timezone.utc)
            hours = parameters.get("hours", 24)
            if type(hours) is not int or not 1 <= hours <= 90 * 24:
                raise QueryError("PARAMETER_INVALID")
            query = {"from": (now - timedelta(hours=hours)).isoformat(), "to": now.isoformat()}
            if kind != "combat":
                query["limit"] = 20
            if target.player:
                query["userId"] = target.player
            if kind == "trajectory":
                if not target.player:
                    raise QueryError("PARAMETER_INVALID")
                query["projection"] = "compact"
            operation = {"activity": "api_listRoomPlayerActivity", "trajectory": "api_getRoomPlayerTrajectory",
                         "combat": "api_getRoomPlayerCombat"}[kind]
            async def world_names():
                until = min(deadline, time.monotonic() + 1.5)
                try:
                    async with asyncio.timeout(max(0, until - time.monotonic())):
                        return await self.call(actor, "api_listRoomWorlds", {"path": path}, until)
                except (TimeoutError, QueryError):
                    return {}

            data, worlds = await asyncio.gather(call(operation, query), world_names(), return_exceptions=True)
            if isinstance(data, BaseException):
                raise data
            if not isinstance(data, dict) or not isinstance(data.get("items"), list):
                raise QueryError("MCP_RESPONSE_INVALID")
            names = {w["id"]: w.get("name", "") for w in worlds.get("items", [])
                     if isinstance(w, dict) and isinstance(w.get("id"), str)} if isinstance(worlds, dict) else {}
            return fmt.activity(data, target.name or target.player, hours, view=kind, worlds=names), target
        if kind == "mods":
            data = await call("api_listRoomMods")
            return fmt.mods(data), target
        if kind == "backups":
            return fmt.backups(await call("api_listRoomBackupSchedules")), target
        raise QueryError("CAPABILITY_MISSING")
