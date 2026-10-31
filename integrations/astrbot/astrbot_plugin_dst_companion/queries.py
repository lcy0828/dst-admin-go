"""Fixed MCP read recipes, independent of chat syntax and the AstrBot framework."""

import asyncio

from . import formatters as fmt
from .gateway import QueryError
from .state import Target


class Queries:
    def __init__(self, gateway):
        self.gateway = gateway

    async def rooms(self, actor, deadline):
        data = await self.gateway.call(actor, "api_listRooms", {}, deadline)
        rows = data.get("items")
        if not isinstance(rows, list):
            raise QueryError("MCP_RESPONSE_INVALID")
        return [r for r in rows if r.get("id") in actor.rooms]

    async def resolve(self, actor, room, player, deadline):
        return await self.gateway.call(
            actor,
            "api_resolveRoomPlayer",
            {"path": {"roomId": room}, "query": {"query": player}},
            deadline,
        )

    async def run(self, actor, kind, target, player, deadline, *, room=None, compact=False):
        path = {"roomId": target.room}

        async def call(name, query=None):
            args = {"path": path}
            if query is not None:
                args["query"] = query
            return await self.gateway.call(actor, name, args, deadline)

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
            data = await self.gateway.call(
                actor,
                "api_getRoomPlayerInventory",
                {
                    "path": {**path, "playerId": target.player},
                    "query": {"source": "auto", "refresh": False},
                },
                deadline,
            )
            return fmt.safe(target.name) + "\n" + fmt.inventory(data), target
        if kind == "mods":
            data = await call("api_listRoomMods")
            return fmt.mods(data), target
        if kind == "backups":
            return fmt.backups(await call("api_listRoomBackupSchedules")), target
        raise QueryError("CAPABILITY_MISSING")
