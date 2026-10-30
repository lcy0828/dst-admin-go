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

    async def run(self, actor, kind, target, player, deadline):
        path = {"roomId": target.room}

        async def call(name, query=None):
            args = {"path": path}
            if query is not None:
                args["query"] = query
            return await self.gateway.call(actor, name, args, deadline)

        if kind == "status":
            # Independent reads share Gateway's global limit and one deadline.
            async with asyncio.TaskGroup() as tasks:
                worlds = tasks.create_task(call("api_listRoomWorlds"))
                states = tasks.create_task(call("api_listWorldStates"))
            return fmt.status(worlds.result(), states.result()), target
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
            return fmt.players(data), target
        if kind == "profile":
            return (
                fmt.player_line(player)
                + "\n最近观测："
                + fmt.safe(player.get("presenceObservedAt") or player.get("lastSeenAt"))
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
            rows = data.get("items", [])
            text = "模组：\n" + "\n".join(fmt.safe(m.get("name") or m.get("id")) for m in rows[:12])
            if len(rows) > 12:
                text += "\n仅列前 12 个，请到面板查看完整列表。"
            return text, target
        if kind == "backups":
            return fmt.backups(await call("api_listRoomBackupSchedules")), target
        raise QueryError("CAPABILITY_MISSING")
