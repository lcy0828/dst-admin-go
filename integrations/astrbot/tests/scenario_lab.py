"""Dialogue acceptance against the real plugin/framework and isolated panel fixtures.

The optional model bridge is an explicit subprocess argument. This module never
connects to a real panel or sends QQ messages; those require separate live evidence.
"""

import argparse
import asyncio
import copy
import hashlib
import json
import re
import shlex
import time
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock

import test_qq_pipeline as pipeline
from astrbot.api import message_components as C
from astrbot.core.agent.tool import ToolSet
from astrbot.core.provider.register import llm_tools
from test_companion import mcp
from test_v41 import capability

from astrbot_plugin_dst_companion.gateway import QueryError, unpack
from astrbot_plugin_dst_companion.task_tools import READ_TOOLS, WRITE_TOOLS

STAMP = "2026-01-01T04:00:00Z"
WRITES = {"api_runBotCapability", "api_runRoomAction", "api_createRoomBackup", "api_kickRoomPlayer"}


def player(id="KU_ALPHA", name="阿青", **extra):
    return dict(
        id=id,
        name=name,
        prefab="wendy",
        online=True,
        presenceStatus="live",
        worldConfirmed=True,
        worldId="m",
        worldName="Master",
        netScore=0,
        fields={"netScore": {"status": "live"}},
        presenceObservedAt=STAMP,
        **extra,
    )


def directory():
    specs = [
        ("room.status", "房间状态", "status", {}),
        ("room.players", "在线玩家", "players", {}),
        ("room.mods", "模组列表", "mods", {}),
        ("room.network", "网络档位", "network", {}),
        ("player.resolve", "查找玩家", "profile", {"query": "string"}),
        ("player.inventory", "玩家背包", "inventory", {"playerId": "player"}),
        ("room.performance", "性能报表", "performance", {}),
        (
            "player.activity",
            "玩家活动",
            "profile",
            {"playerId": "player", "from": "string", "to": "string"},
        ),
        (
            "player.trajectory",
            "移动轨迹",
            "profile",
            {"playerId": "player", "from": "string", "to": "string"},
        ),
        (
            "player.combat",
            "战斗汇总",
            "profile",
            {"playerId": "player", "from": "string", "to": "string"},
        ),
        ("world.world_info", "世界信息", "world", {"worldId": "world"}),
        ("world.online_players", "世界在线玩家", "world", {"worldId": "world"}),
        ("world.entity_count", "实体数量", "world", {"worldId": "world", "prefab": "string"}),
    ]
    items = []
    for ident, title, category, params in specs:
        props = {k: {"type": "string", "maxLength": 128} for k in params}
        if ident in {"player.activity", "player.trajectory"}:
            props.update(before={"type": "string", "maxLength": 1024},
                         limit={"type": "integer", "minimum": 1, "maximum": 50})
        if ident == "room.performance":
            props.update(window={"type": "string", "enum": ["1h", "6h", "24h", "7d"]},
                         **{key: {"type": "string", "format": "date-time", "maxLength": 64}
                            for key in ("from", "to")})
        schema = {
            "type": "object",
            "properties": props,
            "required": [k for k in params if k not in {"from", "to"}],
            "additionalProperties": False,
        }
        item = capability(ident, "read", schema)
        item.update(
            title=title,
            category=category,
            parameterHints={k: v for k, v in params.items() if v in {"player", "world"}},
        )
        items.append(item)
    for ident, title in [
        ("room.start", "启动房间"),
        ("room.stop", "停止房间"),
        ("room.restart", "重启房间"),
        ("room.save", "保存世界"),
        ("room.backup.create", "立即备份"),
        ("room.backups.configure", "自动备份"),
        ("player.kick", "踢人"),
        ("player.resurrect", "复活玩家"),
        ("player.give_item", "发物品"),
        ("player.spawn_entity", "生成生物"),
        ("workbench.clear_player_inventory", "清空背包"),
    ]:
        props = {}
        hints = {}
        if ident.startswith("player.") or "inventory" in ident:
            props["playerId"] = {"type": "string", "maxLength": 128}
            hints["playerId"] = "player"
        if ident in {"player.give_item", "player.spawn_entity"}:
            props["item" if "give" in ident else "entity"] = {"type": "string", "maxLength": 128}
            props["count"] = {"type": "integer", "minimum": 1, "maximum": 40}
            if ident == "player.spawn_entity":
                props["count"]["maximum"] = 5
                props["entity"]["description"] = "仅支持座狼/狗王(warg)、猎犬(hound)、蜘蛛(spider)、猪人(pigman)、兔子(rabbit)、蜜蜂(bee)"
        if ident == "room.backups.configure":
            props["enabled"] = {"type": "boolean"}
        if ident.startswith("workbench."):
            props["confirmed"] = {"type": "boolean", "enum": [True]}
        if ident == "player.resurrect":
            props["player_id"] = props.pop("playerId")
            hints = {"player_id": "player"}
        item = capability(
            ident,
            "write",
            {
                "type": "object",
                "properties": props,
                "required": list(props),
                "additionalProperties": False,
            },
        )
        item.update(
            title=title,
            parameterHints=hints,
            risk="critical" if ident.startswith("workbench.") else "high",
            resultKind="workbench-run" if ident.startswith(("player.", "workbench.")) else "job",
        )
        if ident == "player.give_item":
            item["resultKind"] = "item-grant"
        elif ident == "player.spawn_entity":
            item["resultKind"] = "entity-spawn"
        items.append(item)
    return items


def base_data():
    return {
        "rooms": [
            {"id": "r1", "name": "营地", "maxPlayers": 12},
            {"id": "r2", "name": "远山", "maxPlayers": 8},
        ],
        "people": {
            "r1": [
                player(),
                {
                    **player("KU_BETA", "小满"),
                    "online": False,
                    "worldId": "c",
                    "worldName": "Caves",
                },
            ],
            "r2": [player("KU_GAMMA", "远客")],
        },
        "worlds": [
            {"id": "m", "name": "Master", "role": "master"},
            {"id": "c", "name": "Caves", "role": "caves"},
        ],
        "states": [
            {
                "worldId": w,
                "runtimeState": "running",
                "freshness": "live",
                "stale": False,
                "cycles": 100,
                "season": "autumn",
                "remainingDaysInSeason": 7,
                "phase": "day",
                "observedAt": STAMP,
            }
            for w in ("m", "c")
        ],
        "mods": [
            {"id": "123456", "name": "示例血量条", "enabled": True},
            {"id": "234567", "name": "示例地图", "enabled": True},
        ],
        "inventory": {
            "source": "live",
            "status": "complete",
            "worldName": "Master",
            "capturedAt": STAMP,
            "inventory": [{"prefab": "twigs", "stackSize": 12}],
        },
        "capabilities": directory(),
        "errors": {},
        "receiptStatus": "succeeded",
        "receiptComplete": True,
        "dynamic": {
            "player.activity": {
                "items": [
                    {
                        "kind": "action.harvest",
                        "prefab": "wendy",
                        "fields": {"targetPrefab": "sapling", "count": 3},
                        "userId": "KU_ALPHA",
                        "worldId": "m",
                        "occurredAt": STAMP,
                    }
                ],
                "coverage": [{"worldId": "m", "complete": True}],
            },
            "player.trajectory": {
                "items": [
                    {"worldId": "m", "position": {"x": 10, "z": 20}, "occurredAt": STAMP, "source": "sample"},
                    {"worldId": "m", "position": {"x": 30, "z": 40}, "occurredAt": STAMP, "source": "sample"},
                ],
                "coverage": [{"worldId": "m", "complete": True}],
            },
            "player.combat": {
                "items": [
                    {"worldId": "m", "targetPrefab": "spider", "direction": "outgoing", "damage": 120, "kills": 2, "hits": 4, "unknownDamageHits": 0, "firstAt": STAMP, "lastAt": STAMP},
                    {"worldId": "m", "targetPrefab": "spider", "direction": "incoming", "damage": 30, "kills": 0, "hits": 2, "unknownDamageHits": 0, "firstAt": STAMP, "lastAt": STAMP}
                ],
                "truncated": False,
                "coverage": [{"worldId": "m", "complete": True}],
            },
            "room.performance": {
                "score": 85,
                "sampleCount": 20,
                "cpuPercent": 40,
                "memoryPercent": 60,
                "observedAt": STAMP,
            },
            "world.entity_count": {"count": 7, "complete": True, "prefab": "spiderden"},
            "world.world_info": {"cycles": 100, "season": "autumn", "phase": "day"},
            "world.online_players": {"items": [player()]},
            "room.chat": {
                "items": [{"playerName": "阿青", "text": "一起去打龙蝇吗", "occurredAt": STAMP}]
            },
            "panel.version": {"current": "1.3.20", "latest": "1.3.21", "updateAvailable": True},
            "game.version": {"current": "800000", "latest": "800001", "updateAvailable": True},
            "mods.compatibility": {
                "compatible": None,
                "evidence": "日志包含 mod 示例地图 的调用栈；尚不能证明是唯一原因",
            },
        },
    }


def merge(target, changes):
    for key, value in changes.items():
        if isinstance(value, dict) and isinstance(target.get(key), dict):
            merge(target[key], value)
        else:
            target[key] = copy.deepcopy(value)


class Panel:
    def __init__(self, patch):
        self.data = base_data()
        merge(self.data, patch)
        self.calls = []
        self.requests = {}
        self.counts = {}

    async def transport(self, name, args, timeout, write):
        self.calls.append({"api": name, "arguments": copy.deepcopy(args), "write": write})
        self.counts[name] = self.counts.get(name, 0) + 1
        error = self.data["errors"].get(name)
        if isinstance(error, list):
            error = error[min(self.counts[name] - 1, len(error) - 1)]
        if error:
            raise QueryError(error)
        path = args.get("path", {})
        query = args.get("query", {})
        room = path.get("roomId", "r1")
        d = self.data
        if name == "api_listRooms":
            return mcp({"items": d["rooms"]})
        if name == "api_listRoomWorlds":
            return mcp({"items": d["worlds"]})
        if name == "api_listWorldStates":
            return mcp({"items": d.get("roomStates", {}).get(room, d["states"])})
        if name == "api_listRoomPlayers":
            people = d["people"].get(room, [])
            if query.get("query"):
                needle = query["query"].casefold()
                people = [p for p in people if needle in p["name"].casefold() or needle == p["id"].casefold()]
            if query.get("status") == "online":
                people = [p for p in people if p.get("online")]
            return mcp(
                {
                    "items": people,
                    "online": sum(
                        p.get("online") and p.get("presenceStatus") == "live" for p in people
                    ),
                    "total": len(people),
                    **d.get("playerList", {}),
                }
            )
        if name == "api_resolveRoomPlayer":
            q = query["query"]
            people = d["people"].get(room, [])
            exact = [p for p in people if q in {p["id"], p["name"]}]
            rows = exact or [p for p in people if q.casefold() in p["name"].casefold()]
            return mcp(
                {
                    "status": "resolved"
                    if len(rows) == 1
                    else "ambiguous"
                    if rows
                    else "not_found",
                    "match": "exact_id"
                    if q.startswith("KU_")
                    else "exact_name"
                    if exact
                    else "fuzzy",
                    "items": rows,
                    "total": len(rows),
                }
            )
        if name == "api_getRoomPlayerInventory":
            assert any(p["id"] == path["playerId"] for p in d["people"].get(room, [])), (
                "wrong player identity"
            )
            data = copy.deepcopy(d["inventory"])
            person = next(p for p in d["people"][room] if p["id"] == path["playerId"])
            if not person["online"] and data.get("source") == "live":
                data.update(source="save", savedAt=STAMP, worldId=person["worldId"], worldName=person["worldName"])
            return mcp(data)
        if name == "api_listRoomMods":
            return mcp({"items": d["mods"], "total": len(d["mods"]), **d.get("modList", {})})
        if name == "api_listRoomBackupSchedules":
            return mcp(d.get("schedules", {"items": []}))
        if name == "api_listBotCapabilities":
            offset = int(query.get("cursor") or 0)
            limit = int(query.get("limit", 32))
            items = d["capabilities"][offset:offset + limit]
            end = offset + len(items)
            return mcp(
                {
                    "protocolVersion": query.get("protocolVersion", 1),
                    "revision": d.get("revision", "lab-1"),
                    "items": items,
                    "nextCursor": str(end) if end < len(d["capabilities"]) else "",
                }
            )
        if name in {"api_queryBotCapability", "api_runBotCapability"}:
            rid = args.get("requestId") or query.get("requestId")
            ident = path["capabilityId"]
            params = args.get("body", {}).get("parameters") if write else json.loads(query.get("parameters", "{}"))
            data = copy.deepcopy(d["dynamic"].get(ident, {"summary": "隔离面板已确认该操作完成"}))
            if ident == "player.activity":
                for row in data.get("items", []):
                    row["userId"] = params.get("playerId")
            if params.get("before") and data.get("nextCursor"):
                data.update(items=[], nextCursor="", hasMore=False)
            if ident in {"player.give_item", "player.spawn_entity"}:
                data = {"requested": params.get("count"), "name": params.get("item") or params.get("entity"),
                        "message": "隔离面板回执", "granted" if ident.endswith("give_item") else "spawned": params.get("count")}
            item = next(c for c in d["capabilities"] if c["id"] == ident)
            if ident == "player.spawn_entity" and params.get("entity") not in {"warg", "座狼", "狗王", "hound", "猎犬", "spider", "蜘蛛", "pigman", "猪人", "rabbit", "兔子", "bee", "蜜蜂"}:
                return mcp({"status": "rejected", "complete": True, "error": {"message": "生物不在固定白名单中"}})
            result = {
                "requestId": rid,
                "roomId": room,
                "capabilityId": ident,
                "status": d["receiptStatus"],
                "complete": d["receiptComplete"],
                "source": "platform",
                "resultKind": item["resultKind"],
                "data": data,
                "summary": "隔离面板回执",
            }
            self.requests[rid] = result
            if d.get("loseReply") and write:
                raise QueryError("RESULT_UNCERTAIN")
            return mcp(result)
        if name == "api_getBotCapabilityResult":
            if d.get("uncertainReadback"):
                return mcp({"status": "uncertain", "complete": False})
            return mcp(
                self.requests.get(path["requestId"], {"status": "uncertain", "complete": False})
            )
        raise QueryError("CAPABILITY_MISSING")


class Bridge:
    def __init__(self, command):
        self.command = command
        self.process = None

    async def ask(self, messages, tools):
        if not self.command:
            raise RuntimeError("REAL_MODEL_REQUIRED")
        if self.process is None:
            self.process = await asyncio.create_subprocess_exec(
                *shlex.split(self.command),
                stdin=asyncio.subprocess.PIPE,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.DEVNULL,
            )
        self.process.stdin.write(
            (json.dumps({"messages": messages, "tools": tools}, ensure_ascii=False) + "\n").encode()
        )
        await self.process.stdin.drain()
        raw = await asyncio.wait_for(self.process.stdout.readline(), 100)
        if not raw:
            raise RuntimeError("MODEL_BRIDGE_CLOSED")
        result = json.loads(raw)
        if not result.get("ok"):
            raise RuntimeError("MODEL_BRIDGE_" + result.get("errorType", "FAILED"))
        return result

    async def close(self):
        if self.process and self.process.returncode is None:
            self.process.stdin.close()
            try:
                await asyncio.wait_for(self.process.wait(), 3)
            except TimeoutError:
                self.process.terminate()
                await self.process.wait()


class Journey(pipeline.QQPipelineTests):
    def apply_config(self, changes):
        for key, value in changes.items():
            if key == "defaults":
                self.bot.config["defaults"].update(copy.deepcopy(value))
            else:
                self.bot.config[key] = copy.deepcopy(value)

    async def run_scenario(self, case, bridge, native=None):
        await self.asyncSetUp()
        self.ctx.astrbot_config["admins_id"] = ["123"]
        self.panel = Panel(case.get("fixture", {}))
        self.bot.gateway.transport = self.panel.transport
        self.bot.config.update(
            configuration_version=2,
            defaults={
                "rooms": ["r1", "r2"],
                "defaultRoom": "r1",
                "roomAliases": {"1服": "r1", "2服": "r2", "营地": "r1", "远山": "r2"},
                "categories": [
                    "status",
                    "players",
                    "profile",
                    "inventory",
                    "network",
                    "mods",
                    "performance",
                    "backups",
                    "world",
                    "help",
                ],
                "writeAllowlist": [
                    "room.start",
                    "room.stop",
                    "room.restart",
                    "room.save",
                    "room.backup.create",
                    "room.backups.configure",
                    "player.kick",
                    "player.give_item",
                    "player.spawn_entity",
                    "player.resurrect",
                ],
                "workbenchMode": "all",
            },
        )
        self.apply_config(case.get("config", {}))
        if self.bot.config.get("labNativeDenied"):
            self.bot.policy.native = AsyncMock(return_value=False)
        elif self.bot.config.get("permission_mode") == "group":
            self.bot.policy.native = self.bot.native
        ledger = {}

        async def load():
            return copy.deepcopy(ledger)

        async def save(v):
            ledger.clear()
            ledger.update(copy.deepcopy(v))

        self.bot.load_journal = load
        self.bot.save_journal = save
        self.bot.engine.actions.load = load
        self.bot.engine.actions.save = save
        self.bot.engine.actions.notify = None
        storage = {}

        async def kv_load(key, default=None):
            return copy.deepcopy(storage.get(key, default))

        async def kv_save(key, value):
            storage[key] = copy.deepcopy(value)

        self.bot.get_kv_data = kv_load
        self.bot.put_kv_data = kv_save
        self.bot.read_receipts.load = kv_load
        self.bot.read_receipts.save = kv_save
        self.bot.enrollment.save = kv_save
        self.bot.enrollment.loaded = True
        if self.bot.config.get("permission_mode") == "group":
            self.bot.enrollment.records["adapter:GroupMessage:789"] = {
                "enabled": True,
                "query": True,
                "rooms": ["r1", "r2"],
                "source": self.bot.connections.source(),
            }

            async def discover(name, args):
                return unpack(await self.panel.transport(name, args, 8, False))

            self.bot.catalog.read = discover
            await self.bot.catalog.refresh()
        if native:
            await native.attach(self, case)
        history = {}
        turns = []
        tool_calls = []
        model_requests = []
        events = []
        try:
            for index, step in enumerate(case["turns"]):
                step = {"text": step} if isinstance(step, str) else step
                if "fixture" in step:
                    merge(self.panel.data, step["fixture"])
                if "config" in step:
                    self.apply_config(step["config"])
                if step.get("expire"):
                    for table in (self.bot.state.forms, self.bot.state.choices):
                        for value in table.values():
                            value["until"] = 0
                user = step.get("user", case.get("user", "123"))
                scope = step.get("session", "789")
                parts = ([C.At(qq="456")] if step.get("wake", True) else []) + [
                    C.Plain(step["text"])
                ]
                if "quote" in step:
                    previous = events[step["quote"]]
                    parts.insert(
                        0,
                        C.Reply(
                            id=str(1000 + int(previous.message_obj.message_id)), sender_id="456"
                        ),
                    )
                event = self.event(
                    parts, user=user, session=scope, private=step.get("private", False)
                )
                if self.bot.config.get("permission_mode") == "group":

                    async def group_io(action, _event=event, **kwargs):
                        if action == "get_group_member_info":
                            identity = str(kwargs["user_id"])
                            role = self.bot.config.get("labRoles", {}).get(identity, "member")
                            return {
                                "user_id": kwargs["user_id"],
                                "group_id": kwargs["group_id"],
                                "role": role,
                            }
                        return {"message_id": str(1000 + int(_event.message_obj.message_id))}

                    event.bot.call_action.side_effect = group_io
                if step.get("duplicate") is not None:
                    event.message_obj.message_id = events[step["duplicate"]].message_obj.message_id
                events.append(event)
                before = len(self.models)
                start = time.monotonic()
                output = await self.dispatch(event)
                chat = history.setdefault((user, scope), [])
                chat.append({"role": "user", "content": step["text"]})
                if len(self.models) > before:
                    selected = [
                        t for t in llm_tools.func_list if t.name in READ_TOOLS | WRITE_TOOLS
                    ]
                    req = NS(
                        system_prompt="你是饥荒服务器助手。现在北京时间2026年1月1日12:00。使用可用工具核实事实，简洁回答。",
                        func_tool=ToolSet(selected),
                    )
                    await self.bot.on_model_request(event, req)
                    schemas = req.func_tool.openai_schema()
                    allowed = set(req.func_tool.names())
                    for _ in range(8):
                        answer = await bridge.ask(
                            [{"role": "system", "content": req.system_prompt}, *chat], schemas
                        )
                        model_requests.append(
                            {
                                "turn": index,
                                "model": answer.get("model"),
                                "provider": answer.get("provider"),
                                "usage": answer.get("usage"),
                            }
                        )
                        message = answer["message"]
                        chat.append(message)
                        calls = message.get("tool_calls") or []
                        if not calls:
                            response = NS(
                                role="assistant",
                                completion_text=message.get("content") or "",
                                is_chunk=False,
                                tools_call_name=[],
                                result_chain=None,
                            )
                            await self.bot.on_model_response(event, response)
                            chat[-1]["content"] = response.completion_text
                            output.append(response.completion_text)
                            break
                        for call in calls:
                            name = call["function"]["name"]
                            try:
                                arguments = json.loads(call["function"]["arguments"])
                            except json.JSONDecodeError:
                                arguments = {"invalid": True}
                            await self.bot.on_model_tool(event, NS(name=name), arguments)
                            result = (
                                await self.bot.tasks.perform(event, name, **arguments)
                                if name in allowed
                                else '{"code":"TOOL_NOT_EXPOSED"}'
                            )
                            tool_calls.append(
                                {
                                    "turn": index,
                                    "name": name,
                                    "arguments": arguments,
                                    "result": result,
                                }
                            )
                            chat.append(
                                {"role": "tool", "tool_call_id": call["id"], "content": result}
                            )
                    else:
                        raise RuntimeError("MODEL_TOOL_LIMIT")
                elif output:
                    chat.append({"role": "assistant", "content": "\n".join(output)})
                turns.append(
                    {
                        "input": step,
                        "output": output,
                        "seconds": round(time.monotonic() - start, 3),
                        "model": len(self.models) > before,
                        "callsAfter": len(self.panel.calls),
                    }
                )
            return {
                "id": case["id"],
                "title": case["title"],
                "turns": turns,
                "apiCalls": self.panel.calls,
                "toolCalls": tool_calls,
                "modelRequests": model_requests,
                "ledger": ledger,
            }
        except Exception as error:
            error.scenario_result = {"turns": turns, "apiCalls": self.panel.calls,
                                     "toolCalls": tool_calls, "modelRequests": model_requests, "ledger": ledger}
            raise
        finally:
            await self.asyncTearDown()
            if native:
                await native.close()


def grade(case, result):
    failures = []
    check = case.get("expect", {})
    text = "\n".join(s for t in result.get("turns", []) for s in t["output"])
    last = "\n".join(result["turns"][-1]["output"]) if result.get("turns") else ""
    calls = result.get("apiCalls", [])
    apis = {c["api"] for c in calls}
    # Count the actual domain boundary, including future native operations.
    # Gateway bookkeeping is not a game write; previews are still checked as
    # calls but don't count as executed mutations.
    writes = [c for c in calls if c.get("write") and not c.get("preview")]
    if len(writes) > check.get("maxWrites", 0):
        failures.append("unexpected_write")
    if len(writes) < check.get("minWrites", 0):
        failures.append("missing_write")
    for api in check.get("apis", []):
        if api not in apis:
            failures.append("missing_api:" + api)
    for api in check.get("noApis", []):
        if api in apis:
            failures.append("forbidden_api:" + api)
    if check.get("anyApis") and not apis.intersection(check["anyApis"]):
        failures.append("missing_alternative_read")
    boundary = check.get("noWritesBeforeTurn")
    if boundary is not None and boundary > 0:
        count = result.get("turns", [])[boundary - 1]["callsAfter"]
        if any(c.get("write") and not c.get("preview") for c in calls[:count]):
            failures.append("write_before_user_confirmation")
    for pattern in check.get("match", []):
        if not re.search(pattern, text, re.I):
            failures.append("missing_evidence:" + pattern)
    for pattern in check.get("not", []):
        if re.search(pattern, text, re.I):
            failures.append("forbidden_claim:" + pattern)
    for pattern in check.get("lastMatch", []):
        if not re.search(pattern, last, re.I):
            failures.append("missing_final_evidence:" + pattern)
    if check.get("lastSilent") and last:
        failures.append("expected_silence")
    if check.get("model") is True and not result.get("modelRequests"):
        failures.append("model_not_exercised")
    if check.get("model") is False and result.get("modelRequests"):
        failures.append("unexpected_model")
    for expected in check.get("targets", []):
        if not any(
            all(c["arguments"].get("path", {}).get(k) == v for k, v in expected.items())
            for c in calls
        ):
            failures.append("missing_target:" + json.dumps(expected))
    for expected in check.get("calls", []):
        def matches(call):
            if call["api"] != expected["api"]:
                return False
            for pointer, value in expected.get("arguments", {}).items():
                actual = call["arguments"]
                for key in pointer.split("."):
                    actual = actual.get(key) if isinstance(actual, dict) else None
                if actual != value:
                    return False
            return True
        if not any(matches(call) for call in calls):
            failures.append("missing_domain_call:" + json.dumps(expected))
    for target in check.get("noTargets", []):
        if any(c["arguments"].get("path", {}).get("roomId") == target for c in calls):
            failures.append("forbidden_room:" + target)
    if not text and not check.get("lastSilent"):
        failures.append("no_reply")
    return failures


async def main(options):
    catalog = json.loads(Path(options.catalog).read_text())
    selected = [
        c
        for i, c in enumerate(catalog)
        if i % options.workers == options.worker
        and (not options.ids or c["id"] in options.ids.split(","))
        and (options.select == "all" or bool(c.get("expect", {}).get("model")) == (options.select == "model"))
    ]
    path = Path(options.output)
    path.parent.mkdir(parents=True, exist_ok=True)
    done = (
        {json.loads(row)["id"] for row in path.read_text().splitlines()} if path.exists() else set()
    )
    bridge = Bridge(options.model_command)
    try:
        for case in selected:
            if case["id"] in done:
                continue
            started = time.monotonic()
            try:
                native = None
                if options.native_gateway:
                    from scenario_native import NativePanelBridge
                    native = NativePanelBridge(options.native_gateway)
                result = await Journey().run_scenario(case, bridge, native=native)
                result["failures"] = grade(case, result)
            except Exception as error:
                import traceback

                result = {
                    **getattr(error, "scenario_result", {}),
                    "id": case["id"],
                    "title": case["title"],
                    "failures": [
                        "harness_or_service_error:" + type(error).__name__ + ":" + str(error)
                    ],
                    "traceback": traceback.format_exc(),
                }
            result["seconds"] = round(time.monotonic() - started, 2)
            result["caseSHA256"] = hashlib.sha256(
                json.dumps(case, ensure_ascii=False, sort_keys=True).encode()
            ).hexdigest()
            with path.open("a") as f:
                f.write(json.dumps(result, ensure_ascii=False) + "\n")
            print(
                json.dumps(
                    {
                        "id": case["id"],
                        "failures": result["failures"],
                        "seconds": result["seconds"],
                    },
                    ensure_ascii=False,
                ),
                flush=True,
            )
    finally:
        await bridge.close()


if __name__ == "__main__":
    p = argparse.ArgumentParser()
    p.add_argument("--catalog", required=True)
    p.add_argument("--output", required=True)
    p.add_argument("--model-command", default="")
    p.add_argument("--workers", type=int, default=1)
    p.add_argument("--worker", type=int, default=0)
    p.add_argument("--ids", default="")
    p.add_argument("--select", choices=["all", "direct", "model"], default="all")
    p.add_argument("--native-gateway", default="", help="Explicit opt-in local Go contract harness URL")
    asyncio.run(main(p.parse_args()))
