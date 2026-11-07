"""Real Go contract gateway + synthetic domain data; never calls a game server."""

import asyncio
import copy
import json
from unittest.mock import AsyncMock
from urllib.parse import urlsplit

import httpx

from astrbot_plugin_dst_companion.gateway import QueryError, unpack
from test_companion import mcp


class NativePanelBridge:
    def __init__(self, url):
        parsed = urlsplit(url)
        if parsed.scheme != "http" or parsed.hostname != "127.0.0.1":
            raise ValueError("only explicitly configured loopback test gateways are allowed")
        self.url = url.rstrip("/")
        self.client = httpx.AsyncClient(timeout=35, trust_env=False)
        self.server = None

    async def attach(self, journey, case):
        self.journey, self.panel = journey, journey.panel
        self.original = self.panel.transport
        self.server = await asyncio.start_server(self.callback, "127.0.0.1", 0)
        port = self.server.sockets[0].getsockname()[1]
        response = await self.client.post(self.url + "/configure", json={"callback": f"http://127.0.0.1:{port}/"})
        response.raise_for_status()
        if "capabilities" not in case.get("fixture", {}):
            self.panel.data["capabilities"] = response.json()["capabilities"]
        journey.bot.generic.available = AsyncMock(return_value=True)
        journey.bot.gateway.transport = self.transport

    async def transport(self, name, args, timeout, write):
        if name != "companion_gateway":
            return await self.original(name, args, timeout, write)
        response = await self.client.post(self.url + "/gateway", json=args)
        response.raise_for_status()
        envelope = response.json()
        if envelope.get("error"):
            raise QueryError("NATIVE_GATEWAY_ERROR", message=envelope["error"])
        return mcp(envelope["data"])

    async def callback(self, reader, writer):
        try:
            header = await asyncio.wait_for(reader.readuntil(b"\r\n\r\n"), 5)
            fields = dict(line.split(b":", 1) for line in header.split(b"\r\n")[1:] if b":" in line)
            length = int(next((v for k, v in fields.items() if k.lower() == b"content-length"), b"0"))
            if not 0 < length <= 65536:
                raise ValueError("invalid fixture request")
            request = json.loads(await reader.readexactly(length))
            data = await self.domain(request)
            result = {"data": data}
        except QueryError as error:
            result = {"error": error.code}
        except Exception as error:
            result = {"error": "FIXTURE_" + type(error).__name__}
        encoded = json.dumps(result, ensure_ascii=False).encode()
        writer.write(b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: " + str(len(encoded)).encode() + b"\r\nConnection: close\r\n\r\n" + encoded)
        await writer.drain()
        writer.close()
        await writer.wait_closed()

    async def domain(self, request):
        op, args, write = request["operationId"], request["input"], request["write"]
        args = {k: v for k, v in args.items() if v is not None}
        name = "api_" + op
        # Native domain fixtures, not a production capability registry. Discovery,
        # authorization, schema validation, confirmation and dedup all run in Go.
        mapping = {
            "getRoomPerformanceReport": "room.performance", "listRoomPlayerActivity": "player.activity",
            "getRoomPlayerTrajectory": "player.trajectory", "getRoomPlayerCombat": "player.combat",
            "getSoftwareStatus": "panel.version", "checkSoftwareRelease": "panel.version",
            "getGameVersion": "game.version", "getInstalledGameVersions": "game.version",
            "listRoomChatLogs": "room.chat", "analyzeWorldModCompatibility": "mods.compatibility",
        }
        d = self.panel.data
        if op in mapping or op in {"getRoomPlayer", "queryRoomWorld", "listRoomMaps", "getModConfiguration", "getRoomModProfile", "getRoom", "getRoomRuntimeOverview"} or write:
            self.panel.calls.append({"api": name, "arguments": copy.deepcopy(args), "write": write})
            error = d["errors"].get(name)
            if error:
                raise QueryError(error if isinstance(error, str) else error[0])
            room = args.get("path", {}).get("roomId", "r1")
            if op in mapping:
                value = copy.deepcopy(d["dynamic"].get(mapping[op], {}))
                user_id = args.get("query", {}).get("userId")
                if user_id and "items" in value and mapping[op].startswith("player."):
                    value["items"] = [row for row in value["items"] if row.get("userId", "KU_ALPHA") == user_id]
                if args.get("query", {}).get("before") and value.get("nextCursor"):
                    value.update(items=[], nextCursor="", hasMore=False)
                return value
            if op == "getRoomPlayer":
                return next((p for p in d["people"].get(room, []) if p["id"] == args["path"]["playerId"]), None)
            if op == "queryRoomWorld":
                query = args.get("path", {}).get("queryId", "")
                data = copy.deepcopy(d["dynamic"].get("world." + query, {}))
                complete = bool(d["receiptComplete"] and data.get("complete", True))
                status = d["receiptStatus"]
                if status == "succeeded" and not complete:
                    status = "partial"
                if status in {"uncertain", "timed_out", "failed", "unavailable"}:
                    data = None
                return {"queryId": query, "roomId": room, "worldId": args["path"]["worldId"],
                        "status": status, "complete": complete, "source": "runtime", "cached": False,
                        "transportOutcome": "sent", "executionOutcome": "confirmed" if complete else "unknown",
                        "resultState": "output" if data is not None else "unconfirmed", "mayHaveExecuted": True,
                        "data": data, "error": None, "observedAt": "2026-01-01T04:00:00Z"}
            if op == "getRoom":
                return next((p for p in d["rooms"] if p["id"] == room), None)
            if op == "listRoomMaps":
                return {"items": [], "renderer": {"available": False}, "reason": "合成环境尚未生成地图"}
            if op in {"getModConfiguration", "getRoomModProfile"}:
                return {"configurationOptions": [], "reason": "合成环境没有安装选项文件，不能编造配置"}
            if op == "getRoomRuntimeOverview":
                return {"worlds": copy.deepcopy(d["states"])}
            if write:
                if args.get("body", {}).get("dryRun") is True:
                    self.panel.calls[-1]["preview"] = True
                    return {"status": "preview", "complete": False, "summary": "仅预览，未修改游戏"}
                result = {"status": d["receiptStatus"], "complete": d["receiptComplete"], "summary": "合成游戏执行回执"}
                body = args.get("body", {})
                if op == "giveRoomPlayerItem":
                    result.update(granted=body.get("count"), requested=body.get("count"), name=body.get("item"))
                if d.get("loseReply"):
                    raise QueryError("RESULT_UNCERTAIN")
                return result
        return unpack(await self.original(name, args, 30, write))

    async def close(self):
        if self.server:
            self.server.close()
            await self.server.wait_closed()
        await self.client.aclose()
