"""Small model-facing facades; every operation enters the same authorized engine."""

from __future__ import annotations

import json
import time

from .gateway import QueryError
from .routing import Intent
from .state import Target


def capability_page(items, actor, parameters, access):
    from .capabilities import permitted

    if len(parameters) > 8192:
        raise ValueError("参数过大")
    paging = json.loads(parameters)
    if not isinstance(paging, dict) or set(paging) - {"search", "offset", "limit"}:
        raise ValueError("目录只接受 search、offset、limit")
    search, offset, limit = (
        paging.get("search", ""),
        paging.get("offset", 0),
        paging.get("limit", 8),
    )
    if (
        not isinstance(search, str)
        or len(search) > 80
        or type(offset) is not int
        or not 0 <= offset <= 256
        or type(limit) is not int
        or not 1 <= limit <= 8
    ):
        raise ValueError("分页或搜索参数无效")
    rows = [
        c
        for c in items
        if c["access"] == access
        and permitted(actor, c)
        and (not search or search.casefold() in (c["id"] + " " + c.get("title", "")).casefold())
    ]
    end = offset + limit
    return json.dumps(
        {
            "items": rows[offset:end],
            "total": len(rows),
            "nextOffset": end if end < len(rows) else None,
        },
        ensure_ascii=False,
    )


class ModelTools:
    def __init__(self, plugin):
        self.plugin = plugin

    async def query(self, event, kind, room, player=""):
        await self.plugin.prepare(event)
        actor = self.plugin.actor(event)
        if not actor:
            return "此会话未开放 DST 查询。"
        if kind not in actor.visibility:
            return "此会话未开放这类信息。"
        if room not in actor.rooms:
            return "无权查询该房间。"
        try:
            result = await self.plugin.engine.execute(
                actor, Intent(kind, player, room), Target(room)
            )
            self.plugin.model_presentation.remember(
                event, actor, self.plugin.connection_identity(), kind, result, room
            )
            return result.text
        except QueryError as error:
            return "查询未完成：" + error.code

    async def dst_rooms(self, event: object) -> str:
        """列出当前调用者允许访问的 DST 房间。返回真实 roomId，不能猜测 ID。"""
        await self.plugin.prepare(event)
        actor = self.plugin.actor(event)
        if not actor:
            return "此会话未开放 DST。"
        data = await self.plugin.gateway.call(actor, "api_listRooms", {}, time.monotonic() + 8)
        return json.dumps(
            {
                "items": [
                    {"id": r["id"], "name": r.get("name", "")}
                    for r in data.get("items", [])
                    if r.get("id") in actor.rooms
                ]
            },
            ensure_ascii=False,
        )

    async def dst_room_query(
        self, event: object, room_id: str, query: str, parameters: str = "{}"
    ) -> str:
        """固定只读查询：status、players、network、mods；query=capabilities 获取兼容能力参数；query=cap:能力ID 调用已开放的动态读取。不能执行 Lua 或修改游戏。

        Args:
            room_id(string): dst_rooms 返回的真实房间 ID。
            query(string): status、players、network、mods、capabilities 或 cap:能力ID。
            parameters(string): 动态读取的 JSON 对象；其他查询留空对象。
        """
        if query == "capabilities" or query.startswith("cap:"):
            await self.plugin.prepare(event)
            actor = self.plugin.actor(event)
            if not actor or room_id not in actor.rooms:
                return "未授权。"
            try:
                if query == "capabilities":
                    items = await self.plugin.directory.items(actor, time.monotonic() + 8)
                    return capability_page(items, actor, parameters, "read")
                if len(parameters) > 8192:
                    return "参数过大。"
                values = json.loads(parameters)
                if not isinstance(values, dict):
                    return "参数必须是 JSON 对象。"
                result = await self.plugin.engine.execute(
                    actor,
                    Intent("capability", room=room_id, capability=query[4:], parameters=values),
                    Target(room_id),
                )
                return result.text
            except (ValueError, QueryError):
                return "能力或参数不可用，没有重复执行。"
        if query not in {"status", "players", "network", "mods"}:
            return "未知查询类型。"
        return await self.plugin.query(event, query, room_id)

    async def dst_player_query(self, event: object, room_id: str, player: str, query: str) -> str:
        """按玩家名或 KU 只读查询。重名返回选项，不能猜人。network 不把 netScore 转换为 Ping。

        Args:
            room_id(string): dst_rooms 返回的真实房间 ID。
            player(string): 完整昵称或 KU ID。
            query(string): 仅 profile、inventory、network。
        """
        if query not in {"profile", "inventory", "network"}:
            return "未知查询类型。"
        return await self.plugin.query(event, query, room_id, player)

    async def dst_game_action(
        self,
        event: object,
        room_id: str,
        action: str,
        player: str = "",
        parameters: str = "{}",
    ) -> str:
        """管理者执行固定操作。已提交不等于完成；不确定时不得重试。

        Args:
            room_id(string): 真实房间 ID。
            action(string): backup、start、stop、restart、kick；capabilities 按parameters的search、offset、limit分页读取已开放写能力与参数；复活用cap:player.resurrect；cap:能力ID 执行，例如 cap:player.give_item、cap:player.spawn_entity。
            player(string): kick 必须提供精确 KU ID，其他操作留空。
            parameters(string): 动态管理操作的结构化 JSON 对象，固定操作留空对象。
        """
        await self.plugin.prepare(event)
        actor = self.plugin.actor(event)
        if not actor or actor.role not in ("game-manage", "panel-manage"):
            return "没有管理权限。"
        if action == "capabilities":
            try:
                items = await self.plugin.directory.items(actor, time.monotonic() + 8)
                return capability_page(items, actor, parameters, "write")
            except (QueryError, ValueError):
                return "管理能力目录暂不可用；parameters只接受search、offset、limit。"
        if action.startswith("cap:"):
            try:
                if len(parameters) > 8192:
                    return "参数过大。"
                values = json.loads(parameters)
                if not isinstance(values, dict):
                    return "参数必须是 JSON 对象。"
                result = await self.plugin.engine.execute(
                    actor,
                    Intent("capability", room=room_id, capability=action[4:], parameters=values),
                    Target(room_id),
                )
                return result.text
            except ValueError:
                return "参数不是有效 JSON。"
        if action not in {"backup", "start", "stop", "restart", "kick"}:
            return "未知操作。"
        if action == "kick" and not player.startswith("KU_"):
            return "踢人必须先确认精确 KU ID。"
        result = await self.plugin.engine.execute(
            actor, Intent(action, player, room_id), Target(room_id)
        )
        return result.text

    async def dst_job(self, event: object, room_id: str, job_id: str) -> str:
        """只读原管理任务的真实结果，绝不重新提交任务。

        Args:
            room_id(string): 任务所属房间 ID。
            job_id(string): 原提交返回的 Job ID。
        """
        await self.plugin.prepare(event)
        actor = self.plugin.actor(event)
        if not actor:
            return "未授权。"
        return await self.plugin.engine.actions.read(
            actor, {"roomId": room_id, "jobId": job_id}, time.monotonic() + 8
        )

    async def dst_find_api(self, event: object, query: str) -> str:
        """面板管理员在授权私聊精确发现管理 API，返回完整输入 Schema。

        Args:
            query(string): 精确 operationId 或用途关键词。
        """
        await self.plugin.prepare(event)
        actor = self.plugin.actor(event)
        if not actor or actor.role != "panel-manage" or not event.is_private_chat():
            return "只允许面板管理员在授权私聊读取详细管理接口。"
        data = await self.plugin.gateway.call(
            actor,
            "find_api",
            {"query": query, "includeSchema": True, "limit": 3},
            time.monotonic() + 8,
        )
        return json.dumps(data, ensure_ascii=False)

    async def dst_call_api(self, event: object, operation_id: str, arguments: str) -> str:
        """面板管理员在授权私聊调用已发现接口；参数必须为完整 JSON。不要重新提交结果不确定的写操作。

        Args:
            operation_id(string): dst_find_api 返回的 operationId。
            arguments(string): JSON 对象，包含契约声明的 path/query/body/requestId。
        """
        await self.plugin.prepare(event)
        actor = self.plugin.actor(event)
        if not actor or actor.role != "panel-manage" or not event.is_private_chat():
            return "未授权。"
        if len(arguments) > 16384:
            return "参数过大。"
        parsed = json.loads(arguments)
        if not isinstance(parsed, dict):
            return "参数应为 JSON 对象。"
        # General writes are intentionally unavailable until the precise
        # operation has an approved persistent idempotency adapter.
        runtime = self.plugin.manager.mcp_server_runtime_view.get(self.plugin.server(True))
        tool = (
            next(
                (t for t in runtime.client.tools if t.name == "api_" + operation_id),
                None,
            )
            if runtime
            else None
        )
        if not tool or not tool.annotations or tool.annotations.readOnlyHint is not True:
            return "通用入口仅支持经契约确认的只读接口；写操作请使用固定管理工具或面板。"
        data = await self.plugin.gateway.call(
            actor,
            "call_api",
            {"operationId": operation_id, "arguments": parsed},
            time.monotonic() + 8,
        )
        return json.dumps(data, ensure_ascii=False)
