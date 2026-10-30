"""AstrBot adapter for MCP-only DST broadcasts."""

from __future__ import annotations

import asyncio
import json
import re
import time
from datetime import timedelta
from urllib.parse import urljoin, urlsplit

import httpx
from astrbot.api import AstrBotConfig
from astrbot.api.event import MessageChain
from astrbot.api.star import Context, Star, register

from .broadcast import READ_TOOLS, BroadcastEngine


class MCPReadError(RuntimeError):
    pass


def unpack(result):
    if getattr(result, "isError", False):
        raise MCPReadError("MCP tool failed")
    texts = [item.text for item in result.content if getattr(item, "type", None) == "text"]
    try:
        value = json.loads(texts[0]) if texts else getattr(result, "structuredContent", None)
    except (TypeError, ValueError):
        raise MCPReadError("Invalid MCP response") from None
    if not isinstance(value, dict) or value.get("status") in (
        "failed",
        "error",
        "denied",
        "rejected",
    ):
        raise MCPReadError("MCP operation failed")
    data = value.get("data", value)
    if isinstance(data, dict) and "httpStatus" in data:
        code = data["httpStatus"]
        if not isinstance(code, int) or code < 200 or code >= 300:
            raise MCPReadError("MCP API returned an unsuccessful status")
        data = data.get("data")
    if not isinstance(data, dict):
        raise MCPReadError("MCP returned no business data")
    return data


def resource_request(config, resource):
    """Follow only a resource issued by this same configured MCP server."""
    origin = urlsplit(str(config.get("url") or ""))
    target = urlsplit(urljoin(config.get("url", ""), str(resource.get("url") or "")))
    if origin.scheme not in ("http", "https") or origin.username or origin.password:
        raise MCPReadError("MCP server must have an HTTP endpoint without URL credentials")
    if (target.scheme, target.hostname, target.port) != (
        origin.scheme,
        origin.hostname,
        origin.port,
    ):
        raise MCPReadError("Refusing a resource from a different origin")
    if (
        not re.fullmatch(r"/api/v2/system/ai/resources/[A-Za-z0-9_-]+", target.path)
        or target.fragment
        or target.username
    ):
        raise MCPReadError("Invalid MCP resource path")
    if resource.get("kind") != "stream":
        raise MCPReadError("Expected a streaming resource")
    return target.geturl(), {**config.get("headers", {}), "Accept": "text/event-stream"}


async def sse_frames(lines):
    """Parse complete SSE frames, including multiline data and heartbeat comments."""
    event, event_id, data, size = "message", "", [], 0
    async for raw in lines:
        line = raw.rstrip("\r\n")
        if not line:
            if data:
                yield event, event_id, json.loads("\n".join(data))
            event, event_id, data, size = "message", "", [], 0
            continue
        if line.startswith(":"):
            continue
        size += len(line.encode("utf-8"))
        if size > 128 * 1024:
            raise MCPReadError("SSE event exceeds the size limit")
        field, _, value = line.partition(":")
        value = value[1:] if value.startswith(" ") else value
        if field == "event":
            event = value
        elif field == "id" and "\x00" not in value:
            event_id = value
        elif field == "data":
            data.append(value)
    # An incomplete frame at EOF is deliberately not committed.


class MCPGateway:
    def __init__(self, manager, server, bridge=None):
        self.manager, self.server = manager, server
        self.limit = asyncio.Semaphore(1)
        self.bridge = bridge

    def config(self):
        value = self.manager.load_mcp_config().get("mcpServers", {}).get(self.server, {})
        if value.get("active", True) is False:
            raise MCPReadError("Configured MCP server is disabled")
        return value

    async def __call__(self, name, arguments):
        if name not in READ_TOOLS:
            raise PermissionError("Broadcasts may only use the fixed read-only tool allowlist")
        bridge = self.bridge() if self.bridge else None
        if bridge is not None and bridge.alive:
            return await bridge.broadcast_read(name, arguments, self.read)
        return await self.read(name, arguments)

    async def read(self, name, arguments):
        async with self.limit:
            runtime = self.manager.mcp_server_runtime_view.get(self.server)
            if runtime is None:
                raise MCPReadError("MCP server is not connected yet")
            tool = next((tool for tool in runtime.client.tools if tool.name == name), None)
            if (
                tool is None
                or tool.annotations is None
                or tool.annotations.readOnlyHint is not True
            ):
                raise MCPReadError("Required tool is missing or is not explicitly read-only")
            result = await runtime.client.call_tool_with_reconnect(
                name,
                arguments,
                read_timeout_seconds=timedelta(seconds=20),
            )
            return unpack(result)


@register("astrbot_plugin_dst_broadcast", "lcy", "饥荒 MCP 事件播报", "0.1.2")
class DSTBroadcast(Star):
    def __init__(self, context: Context, config: AstrBotConfig):
        super().__init__(context, config)
        self.config = config
        self.tasks = []
        self.http = None
        self.engine = None
        self.gateway = MCPGateway(
            context.get_llm_tool_manager(),
            str(config.get("mcp_server", "dst-admin")),
            lambda: getattr(context, "dst_companion_v1", None),
        )
        self.poll_interval = max(15, min(60, int(config.get("poll_interval_seconds", 15))))
        self.warned = {}

    def warn(self, message):
        if time.monotonic() - self.warned.get(message, -1000) >= 60:
            self.warned[message] = time.monotonic()
            self.logger.warning(message)

    async def initialize(self):
        if not self.config.get("enabled", False):
            self.logger.info("MCP 事件播报已加载，群播报开关关闭。")
            return
        group = str(self.config.get("target_group_id", "")).strip()
        platform = str(self.config.get("platform_id", "")).strip()
        rooms = {}
        for suffix in ("", "_2"):
            room = str(self.config.get("room_id" + suffix, "")).strip()
            if room:
                if room in rooms:
                    raise ValueError("两个房间的 roomId 不能相同")
                rooms[room] = str(self.config.get("room_label" + suffix, "") or room)
        if not group.isdigit() or not platform or ":" in platform or not rooms:
            raise ValueError("请配置有效的 QQ 群号、平台 ID 和至少一个 roomId")
        self.target = f"{platform}:GroupMessage:{group}"
        bot_config = self.context.get_config(umo=self.target)
        settings = bot_config.get("platform_settings", {})
        allowed = [
            str(value).strip() for value in settings.get("id_whitelist", []) if str(value).strip()
        ]
        if (
            settings.get("enable_id_white_list")
            and allowed
            and group not in allowed
            and self.target not in allowed
        ):
            raise ValueError("目标群未包含在当前 AstrBot 白名单中")
        state = await self.get_kv_data("broadcast_state_v1", {})
        self.engine = BroadcastEngine(
            self.gateway,
            self.send,
            self.save_state,
            rooms,
            options=self.config,
            state=state,
            log=self.warn,
            emit_notice=self.send_notice,
        )
        self.http = httpx.AsyncClient(
            timeout=httpx.Timeout(connect=10, read=45, write=10, pool=10),
            limits=httpx.Limits(max_connections=4, max_keepalive_connections=2),
            follow_redirects=False,
        )
        self.tasks = [
            asyncio.create_task(self.run_room(room), name=f"dst-broadcast-{room}") for room in rooms
        ]
        self.tasks.append(asyncio.create_task(self.tick(), name="dst-broadcast-delivery"))
        self.logger.info("MCP 事件播报已启动；游戏查询只使用固定只读工具。")

    async def save_state(self, value):
        await self.put_kv_data("broadcast_state_v1", value)

    async def send(self, text):
        return await self.context.send_message(self.target, MessageChain().message(text))

    async def send_notice(self, text, related):
        bridge = getattr(self.context, "dst_companion_v1", None)
        if bridge is not None and bridge.alive:
            result = await bridge.send_notice(self.target, text, related)
            if result is not None:
                return result
        return await self.send(text)

    async def run_room(self, room):
        children = {}
        try:
            while True:
                try:
                    worlds = await self.engine.seed(room)
                    break
                except Exception as error:
                    self.warn(f"房间初始化暂不可用（{type(error).__name__}），稍后重试")
                    await asyncio.sleep(15)
            if self.config.get("presence", True):
                children = {
                    world: asyncio.create_task(
                        self.stream(room, world), name=f"dst-events-{room}-{world}"
                    )
                    for world in worlds
                }
            last_discovery = time.monotonic()
            while True:
                if time.monotonic() - last_discovery >= 120:
                    try:
                        current = set(await self.engine.seed(room))
                        removed = [
                            children.pop(world) for world in list(children) if world not in current
                        ]
                        for task in removed:
                            task.cancel()
                        await asyncio.gather(*removed, return_exceptions=True)
                        if self.config.get("presence", True):
                            for world in current - set(children):
                                children[world] = asyncio.create_task(
                                    self.stream(room, world), name=f"dst-events-{room}-{world}"
                                )
                        last_discovery = time.monotonic()
                    except Exception:
                        last_discovery = time.monotonic()
                        self.warn("世界目录暂未同步，保留现有订阅")
                if self.config.get("server", True):
                    try:
                        await self.engine.poll_audit(room)
                    except Exception as error:
                        self.warn(f"分片状态记录查询暂不可用（{type(error).__name__}）")
                if self.config.get("chat", False):
                    try:
                        await self.engine.poll_chat(room)
                    except Exception as error:
                        self.warn(f"公开聊天查询暂不可用（{type(error).__name__}）")
                await asyncio.sleep(self.poll_interval)
        finally:
            for task in children.values():
                task.cancel()
            await asyncio.gather(*children.values(), return_exceptions=True)

    async def stream(self, room, world):
        delay = 2
        while True:
            try:
                arguments = {"path": {"roomId": room, "worldId": world}}
                cursor = self.engine.state["cursors"].get(room + "/" + world)
                if cursor:
                    arguments["query"] = {"cursor": cursor}
                data = await self.gateway("api_streamWorldRuntimeEvents", arguments)
                url, headers = resource_request(self.gateway.config(), data.get("resource", {}))
                async with self.http.stream("GET", url, headers=headers) as response:
                    if (
                        response.status_code != 200
                        or "text/event-stream" not in response.headers.get("content-type", "")
                    ):
                        raise MCPReadError("Runtime event resource is not available")
                    async for kind, event_id, payload in sse_frames(response.aiter_lines()):
                        if not isinstance(payload, dict) or kind == "runtime.error":
                            raise MCPReadError("Runtime event source reported an error")
                        await self.engine.accept(room, world, kind, payload, event_id)
                        if kind in ("runtime.gap", "runtime.reset"):
                            await self.engine.seed(room)
                        delay = 2
            except asyncio.CancelledError:
                raise
            except Exception as error:
                self.warn(
                    f"Runtime 事件流暂不可用（{type(error).__name__}）；不会据此判定服务器崩溃"
                )
            await asyncio.sleep(delay)
            delay = min(30, delay * 2)

    async def tick(self):
        while True:
            try:
                await self.engine.resolve_presence()
                await self.engine.deliver_one()
            except Exception as error:
                self.warn(f"播报处理暂不可用（{type(error).__name__}）")
            await asyncio.sleep(2)

    async def terminate(self):
        for task in self.tasks:
            task.cancel()
        await asyncio.gather(*self.tasks, return_exceptions=True)
        self.tasks.clear()
        if self.http is not None:
            await self.http.aclose()
            self.http = None
