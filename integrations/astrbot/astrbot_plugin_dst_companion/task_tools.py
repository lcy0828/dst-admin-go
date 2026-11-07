"""Nine guarded task facades, sharing direct-command resolution and execution.

The model never receives the private service credential or chooses an API/Lua.
Legacy method adapters remain callable for saved conversations but are hidden.
"""

import copy
import hashlib
import json
import re
import time

from .capabilities import permitted, validate
from .discovery import (
    actions as discover_actions,
    example_score,
    negated as discovery_negated,
    score as discovery_score,
)
from .gateway import QueryError
from .presentation import business
from .routing import Intent, performance_window
from .state import Target
from .task_results import read_result

INTERNAL_SLOTS = frozenset(
    {"feedback", "phase", "preview_id", "confirmed", "confirmation", "expected_session"}
)

READ_TOOLS = {"dst_servers", "dst_player", "dst_world", "dst_find", "dst_read", "dst_result"}
WRITE_TOOLS = {"dst_player_action", "dst_server_action", "dst_run"}
READ_VIEWS = {
    "dst_servers": {"overview": "status", "players": "players", "mods": "mods", "performance": "performance"},
    "dst_player": {"profile": "profile", "inventory": "inventory", "network": "network", "activity": "profile", "trajectory": "profile", "combat": "profile"},
    "dst_world": {"world_info": "world", "online_players": "world", "entity_count": "world", "player_modes": "world"},
}
ACTION_VALUES = {
    "dst_player_action": ["resurrect", "give_item", "spawn_entity", "kick", "god_mode", "creative_mode",
                          "announce", "ban", "unban", "kill", "reset_character"],
    "dst_server_action": ["start", "stop", "restart", "save", "backup", "configure_backup", "reset"],
}


def tool_view(tool, actor):
    """Narrow this request's schema, retaining the original permission wrapper."""
    field, values = "", []
    if tool.name in READ_VIEWS:
        field = "view"
        values = [v for v, category in READ_VIEWS[tool.name].items() if category in actor.visibility]
        if not values:
            return None
    elif tool.name in ACTION_VALUES:
        field, values = "action", ACTION_VALUES[tool.name]
    if not field:
        return tool
    narrowed = copy.copy(tool)
    narrowed.parameters = copy.deepcopy(tool.parameters)
    schema = narrowed.parameters.setdefault("properties", {}).setdefault(field, {"type": "string"})
    schema["enum"] = list(values)
    if tool.name in {"dst_world", "dst_player"}:
        properties = ({"prefab": {"type": "string", "maxLength": 128}}
                      if tool.name == "dst_world" else {
                          "from": {"type": "string", "format": "date-time", "maxLength": 64},
                          "to": {"type": "string", "format": "date-time", "maxLength": 64},
                          "before": {"type": "string", "maxLength": 1024},
                          "limit": {"type": "integer", "minimum": 1, "maximum": 50}})
        narrowed.parameters["properties"]["parameters"] = {
            "type": "object", "properties": properties, "additionalProperties": False,
            "description": "只填写当前查询需要的字段；世界数量查询仅填 prefab，不需要 count。" if tool.name == "dst_world"
                           else "活动/轨迹可填写时间范围、before 和 limit；战斗仅填时间范围，普通资料/背包/网络留空。"}
    elif tool.name == "dst_servers" and "performance" in values:
        narrowed.parameters["properties"]["parameters"] = {
            "type": "object",
            "properties": {
                "window": {"type": "string", "enum": ["1h", "6h", "24h", "7d"]},
                "from": {"type": "string", "format": "date-time", "maxLength": 64},
                "to": {"type": "string", "format": "date-time", "maxLength": 64},
                "worldId": {"type": "string", "maxLength": 128},
            },
            "additionalProperties": False,
            "description": "仅 performance 视图可选填写 window（1h/6h/24h/7d，默认 24h）或时间范围；其他视图留空。",
        }
    return narrowed
TERMS = {
    "room.status": "查服 房间 服务器 概况 status server",
    "room.players": "谁在线 在线玩家 人数 players",
    "room.network": "卡 延迟 网络 客户端 ping netscore network",
    "player.inventory": "背包 装备 物品栏 离线 inventory backpack",
    "player.resolve": "玩家信息 查人 昵称 profile",
    "player.resurrect": "复活 救人 resurrect revive",
    "player.kick": "踢人 踢出 踢掉 kick",
    "room.mods": "模组 mod 配置 设置",
    "room.performance": "性能 负载 CPU 内存 报表 卡顿 performance",
    "player.give_item": "发物品 发放 给予 物品 肉丸 give item meatballs",
    "player.spawn_entity": "生成 刷生物 座狼 狗王 spawn",
    "world.entity_count": "统计 数量 蜘蛛巢 多少 count entity",
    "world.world_info": "世界信息 季节 昼夜 天气 月相 天数 season phase weather",
    "world.online_players": "分片玩家 世界玩家 地面玩家 洞穴玩家",
    "player.activity": "活动 做了什么 干了什么 最近行为 activity",
    "player.trajectory": "去哪 去了哪里 位置 地点 轨迹 trajectory",
    "player.combat": "打了谁 打怪 打boss 战斗 combat",
    "room.backup.create": "备份 快照 backup",
    "room.backups.configure": "定时备份 自动备份 schedule",
}


def encode(value):
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"))


def recommended(item):
    capability = item["id"]
    if capability in {"room.status", "room.players", "room.mods", "room.performance"}:
        return {
            "tool": "dst_servers",
            "input": {
                "view": {"room.status": "overview"}.get(capability, capability.split(".")[1])
            },
        }
    if capability in {
        "player.inventory",
        "player.resolve",
        "room.network",
        "player.activity",
        "player.trajectory",
        "player.combat",
    }:
        return {
            "tool": "dst_player",
            "input": {
                "view": "profile" if capability == "player.resolve" else capability.split(".")[1]
            },
        }
    if capability in {"world.world_info", "world.online_players", "world.entity_count", "world.player_modes"}:
        return {"tool": "dst_world", "input": {"view": capability.split(".")[1]}}
    if capability in {"player." + action for action in ACTION_VALUES["dst_player_action"]}:
        return {"tool": "dst_player_action", "input": {"action": capability.split(".")[1]}}
    if capability in {
        "room.start",
        "room.stop",
        "room.restart",
        "room.save",
        "room.backup.create",
        "room.backups.configure",
        "room.reset",
    }:
        action = {"room.backup.create": "backup", "room.backups.configure": "configure_backup"}.get(
            capability, capability.split(".")[1]
        )
        return {"tool": "dst_server_action", "input": {"action": action}}
    return {
        "tool": "dst_run" if item["access"] == "write" else "dst_read",
        "input": {"capability_id": capability},
    }


def _model_hidden_slot(field, prop, slots):
    slot = slots.get(field) if isinstance(slots, dict) else None
    if isinstance(slot, dict):
        return slot.get("model") is False
    if field == "phase":
        enum = prop.get("enum", []) if isinstance(prop, dict) else []
        return "preview" in enum
    return field in INTERNAL_SLOTS


def discover_item(item):
    """Describe the facade's input, without leaking required native ID or internal fields."""
    schema = copy.deepcopy(item["parameters"])
    selectors = {}
    slots = item.get("slotRoles") if isinstance(item.get("slotRoles"), dict) else {}
    for field, hint in item.get("parameterHints", {}).items():
        if hint in {"player", "world"}:
            schema.get("properties", {}).pop(field, None)
            required = schema.get("required", [])
            selectors[hint] = {"required": field in required, "description": "填写昵称或ID" if hint == "player" else "填写地面、洞穴或世界名称/ID"}
            schema["required"] = [key for key in required if key != field]
    props = schema.get("properties", {})
    hidden = {f for f, p in list(props.items()) if _model_hidden_slot(f, p, slots)}
    for field in hidden:
        props.pop(field, None)
    if hidden and "required" in schema:
        schema["required"] = [key for key in schema.get("required", []) if key not in hidden]
    card = {"id": item["id"], "title": item["title"], "parameters": schema,
            "selectors": selectors, **recommended(item)}
    if item.get("tier"):
        card["tier"] = item["tier"]
    if item.get("confirm"):
        card["confirm"] = item["confirm"]
    return card


class TaskTools:
    def __init__(self, plugin):
        self.plugin = plugin

    def question(self, event, actor, value):
        raw = encode(value)
        event._dst_task_question = (self, actor.projection, str(event.message_obj.message_id), raw)
        return raw

    async def actor(self, event):
        await self.plugin.prepare(event)
        actor = self.plugin.actor(event)
        if not actor:
            raise QueryError("PERMISSION_DENIED")
        return actor

    async def room(self, actor, value):
        rows = await self.plugin.engine.queries.rooms(actor, time.monotonic() + 8)
        effective = self.plugin.effective(str(actor.event.unified_msg_origin))
        aliases = effective["roomAliases"]
        parsed = self.plugin.input(actor.event)
        text = parsed[0] if parsed else ""
        # A model omission/guess cannot resolve a name the user left ambiguous.
        duplicate = [r for r in rows if r.get("name") and r["name"] in text
                     and sum(other.get("name") == r["name"] for other in rows) > 1]
        explicit = [r for r in rows if r["id"] in text or any(
            alias in text and ident == r["id"] and alias != r.get("name")
            for alias, ident in aliases.items())]
        if duplicate and not explicit:
            return "", {"state": "needs_input", "summary": "存在同名房间，请使用房间 ID 或唯一别名选择。",
                        "choices": [{"id": r["id"], "name": r.get("name", "")} for r in duplicate[:5]]}
        if not value:
            value = effective["defaultRoom"]
        value = aliases.get(value, value)
        matches = [r for r in rows if not value or value in {r["id"], r.get("name")}]
        if len(matches) != 1:
            return "", {
                "state": "needs_input",
                "summary": "请选择房间。",
                "choices": [
                    {"id": r["id"], "name": r.get("name", "")} for r in (matches or rows)[:5]
                ],
            }
        return matches[0]["id"], None

    async def perform(self, event, name, **values):
        actor, message_id = None, ""
        try:
            if name not in READ_TOOLS | WRITE_TOOLS or len(encode(values).encode()) > 12288:
                raise QueryError("PARAMETER_INVALID")
            for key in ("room", "player", "world", "query", "capability_id", "task_ref"):
                val = values.get(key, "")
                if not isinstance(val, str) or len(val) > 255 or any(c in val for c in "\x00\r\n"):
                    raise QueryError("PARAMETER_INVALID")
            if values.get("parameters") is not None and not isinstance(values["parameters"], dict):
                raise QueryError("PARAMETER_INVALID")
            actor = await self.actor(event)
            message_id = str(getattr(event.message_obj, "message_id", ""))
            if not self.plugin.state.current_request(actor.key, message_id):
                return encode({"state": "superseded", "summary": "用户已发起新的请求，停止本轮工具调用；不要覆盖后续选择。"})
            uncertain = getattr(event, "_dst_uncertain_write", None)
            if name in WRITE_TOOLS and uncertain and uncertain[:2] == (actor.projection, message_id):
                return uncertain[2]
            pending = getattr(event, "_dst_task_question", None)
            if pending and pending[:3] == (self, actor.projection, message_id):
                return pending[3]
            from .routing import read_request_intent

            parsed = self.plugin.input(event)
            aliases = dict(self.plugin.effective(actor.scope[-1])["roomAliases"])
            original = parsed[0] if parsed else ""
            if name=='dst_world' and not values.get('world'):
                # Recover only an explicit, unique shard in the user's message;
                # the model omitting a required selector must not erase it.
                worlds=[label for label in ('地面','洞穴') if label in original]
                if len(worlds)==1: values['world']=worlds[0]
            own_query = re.search(r"(?:我|自己)(?:的)?(?:背包|信息|资料|网络|延迟|活动|轨迹|战斗|位置)|(?:给|复活|踢出|踢掉)我(?:[，。\s]|$)", original)
            discovery_identity = name=='dst_find' and own_query and re.search(r"(?:看看|查|看下|读取).*(?:我|自己)(?:的)?(?:背包|信息|资料|网络|延迟)",original)
            if (name in {"dst_player", "dst_player_action"} or discovery_identity or
                    name in {"dst_read", "dst_run"} and values.get("capability_id", "").startswith("player.")):
                supplied = values.get("player") or (values.get("parameters") or {}).get("playerId") or (values.get("parameters") or {}).get("player_id")
                explicit_name = bool(isinstance(supplied, str) and supplied and (
                    re.fullmatch(r"KU_[A-Za-z0-9_-]{3,64}", supplied) and supplied in original
                    or re.search(r"(?:游戏(?:昵称|名)|角色名|昵称|我叫)\s*(?:是|叫|为|[:：=])?\s*[「『“\"']?"
                                 + re.escape(supplied) + r"(?:[」』”\"'，。！？!?、\s]|$)", original)))
                if own_query and not explicit_name:
                    binding = self.plugin.effective(actor.scope[-1])["bindings"].get(actor.user)
                    if not binding:
                        # A model clarification must create the same bounded,
                        # per-user continuation as a direct query. The per-event
                        # latch below still prevents the model answering itself.
                        kind = values.get("view", "profile") if name == "dst_player" else {
                            "player.inventory": "inventory", "player.profile": "profile",
                            "player.network": "network",
                        }.get(values.get("capability_id", "")) if name == "dst_read" else None
                        if name in READ_TOOLS:
                            if own_query and own_query[0].endswith("背包"):
                                kind = "inventory"
                            elif own_query and own_query[0].endswith(("信息", "资料")):
                                kind = "profile"
                            elif own_query and own_query[0].endswith(("网络", "延迟")):
                                kind = "network"
                        if kind in {"profile", "inventory", "network"}:
                            if kind not in actor.visibility:
                                raise QueryError("PERMISSION_DENIED")
                            room, question = await self.room(actor, values.get("room", ""))
                            if not self.plugin.state.current_request(actor.key, message_id):
                                return encode({"state": "superseded", "summary": "用户已发起新的请求，请以新请求为准。"})
                            seq = self.plugin.state.begin(actor.key)
                            intent = Intent(kind, player="他", room=room)
                            if question:
                                result = self.plugin.engine.choose(actor, intent, question.get("choices", []), "", seq, question["summary"])
                                return self.question(event, actor, {"state": "needs_input", "summary": result.text})
                            self.plugin.state.form(actor.key, {"type": "player", "intent": intent,
                                "target": Target(room), "configRevision": actor.config_revision}, sequence=seq)
                        return self.question(event, actor, {"state": "needs_input",
                            "summary": "尚未确认你的游戏身份，请提供游戏昵称或 KU ID；QQ 名称或唯一在线玩家都不能代替绑定。"
                            + ("2 分钟内可直接回复昵称，或回复取消。" if kind in {"profile", "inventory", "network"} else "")})
                    values["player"] = binding
                    values["parameters"] = {k: v for k, v in (values.get("parameters") or {}).items() if k not in {"playerId", "player_id"}}
            if name in WRITE_TOOLS and parsed and read_request_intent(parsed[0], aliases) is not None:
                raise QueryError("READ_REQUEST_ONLY")
            if name in READ_VIEWS:
                view = values.get("view", "overview" if name == "dst_servers" else "profile" if name == "dst_player" else "")
                if view not in READ_VIEWS[name]:
                    raise QueryError("PARAMETER_INVALID")
                if READ_VIEWS[name][view] not in actor.visibility:
                    raise QueryError("PERMISSION_DENIED")
                values["view"] = view
            if name == "dst_find":
                generic = getattr(self.plugin, "generic", None)
                if generic and await generic.available():
                    return encode(await generic.find(actor, values.get("query", "")))
                return await self.find(actor, values.get("query", ""))
            if ((name in {"dst_read", "dst_run"} and values.get("capability_id", "").startswith("api:"))
                    or name == "dst_result" and values.get("task_ref", "").startswith(("generic:", "generic-request:", "resource:"))):
                return await self.plugin.generic.perform(event, actor, name, values)
            if name == "dst_result":
                return await self.result(actor, values.get("task_ref", ""))
            if (
                name == "dst_servers"
                and not values.get("room")
                and values.get("view", "overview") == "overview"
            ):
                result = json.loads(await self.plugin.models.dst_rooms(event))
                result["note"] = "这里只是房间目录，尚未读取运行状态。查询或比较状态时，请对所需房间逐一调用 dst_servers(room=真实ID, view=overview)。"
                return encode(result)
            from .references import PLAYER_REFERENCES, needs_reference

            reference = None
            selector_intent = Intent(
                values.get("view", "profile") if name == "dst_player" else "capability",
                player=values.get("player", ""),
                capability=("player." + values.get("action", "")) if name == "dst_player_action" else values.get("capability_id", ""),
                parameters=values.get("parameters") or {},
            )
            # A typed player always overrides a quote. Otherwise use only a
            # receipt verified against this group, bot, panel and allowed room.
            if parsed and len(parsed) > 2 and parsed[2] and values.get("player", "") in PLAYER_REFERENCES and needs_reference(selector_intent):
                reference = await self.plugin.references.resolve(actor, parsed[2], selector_intent)
                if reference is None:
                    return self.question(event, actor, {"state": "needs_input",
                        "summary": "未能确认引用中的玩家，请提供游戏昵称或 KU ID；不会改查上一次的玩家。"})
            room, question = await self.room(actor, values.get("room", "") or (reference.room if reference else ""))
            if question:
                return self.question(event, actor, question)
            if reference and reference.room != room:
                return self.question(event, actor, {"state": "needs_input",
                    "summary": "引用中的玩家与指定房间不一致，请明确房间和玩家。尚未执行。"})
            view = values.get("view", "")
            player = values.get("player", "")
            params = dict(values.get("parameters") or {})
            kind, capability = "capability", values.get("capability_id", "")
            if name == "dst_servers":
                kind = {"overview": "status", "players": "players", "mods": "mods"}.get(
                    view, "capability"
                )
                capability = "room.performance" if view == "performance" else ""
                if view == "performance" and not params.get("window") and not params.get("from"):
                    win = performance_window(original)
                    if win:
                        params["window"] = win
            elif name == "dst_player":
                if view in {"profile", "inventory", "network"}:
                    kind = view
                elif view in {"activity", "trajectory", "combat"}:
                    capability = "player." + view
                else:
                    raise QueryError("PARAMETER_INVALID")
            elif name == "dst_world":
                if view not in {"world_info", "online_players", "entity_count", "player_modes"}:
                    raise QueryError("PARAMETER_INVALID")
                capability = "world." + view
            elif name == "dst_player_action":
                action = values.get("action")
                if action not in ACTION_VALUES["dst_player_action"]:
                    raise QueryError("PARAMETER_INVALID")
                capability = "player." + action
            elif name == "dst_server_action":
                capability = {
                    "start": "room.start",
                    "stop": "room.stop",
                    "restart": "room.restart",
                    "save": "room.save",
                    "backup": "room.backup.create",
                    "configure_backup": "room.backups.configure",
                    "reset": "room.reset",
                }.get(values.get("action"), "")
            if kind != "capability":
                if params or kind not in actor.visibility:
                    raise QueryError("PERMISSION_DENIED")
                intent = Intent(kind, player, room)
            else:
                item = await self.plugin.directory.get(actor, capability, time.monotonic() + 8)
                if not item and name == "dst_read":
                    items = await self.plugin.directory.items(actor, time.monotonic() + 8)
                    # A view name is not a capability ID. Suggest only an
                    # authorized read; never infer/execute a replacement action.
                    alternatives = [c for c in items if c["access"] == "read"
                                    and permitted(actor, c)
                                    and c["id"].split(".")[-1] == capability]
                    if alternatives:
                        return encode({"state": "needs_input", "code": "CAPABILITY_ID_MISMATCH",
                                       "summary": "能力 ID 不完整，请使用下面的工具与参数继续查询。此调用没有执行。",
                                       "items": [discover_item(c) for c in alternatives[:3]]})
                if not item or not permitted(actor, item):
                    raise QueryError("PERMISSION_DENIED")
                if (item["access"] == "write") != (name in WRITE_TOOLS):
                    raise QueryError("PERMISSION_DENIED")
                if player:
                    hints = [k for k, h in item.get("parameterHints", {}).items() if h == "player"]
                    if len(hints) != 1 or hints[0] in params and params[hints[0]] != player:
                        raise QueryError("PARAMETER_INVALID")
                    params[hints[0]] = player
                world_fields = [k for k, h in item.get("parameterHints", {}).items() if h == "world"]
                world_field = world_fields[0] if len(world_fields) == 1 else "worldId"
                world = values.get("world", "") or (params.get(world_field, "") if world_fields else "")
                if world:
                    data = await self.plugin.gateway.call(
                        actor,
                        "api_listRoomWorlds",
                        {"path": {"roomId": room}},
                        time.monotonic() + 8,
                    )
                    rows = data.get("items", [])
                    original = parsed[0] if parsed else ""
                    labels = {r.get("name", "") for r in rows} | {"地面", "洞穴"}
                    named = []
                    for label in labels:
                        if not label or label not in original:
                            continue
                        peers = [r for r in rows if r.get("name") == label
                                 or (label == "洞穴" and r.get("role") == "caves")
                                 or (label == "地面" and r.get("role") == "master")]
                        if len(peers) > 1:
                            named = peers
                            break
                    explicit_ids = [r for r in rows if r.get("id") and r["id"] in original]
                    if len(named) > 1 and not explicit_ids:
                        return self.question(event, actor, {"state": "needs_input",
                            "summary": "有多个匹配的世界，请用户选择世界 ID；不能代选。", "choices": named[:5]})
                    matches = [
                        r
                        for r in data.get("items", [])
                        if world in {r.get("id"), r.get("name")}
                        or world == "地面"
                        and r.get("role") == "master"
                        or world == "洞穴"
                        and r.get("role") == "caves"
                    ]
                    if len(matches) != 1:
                        return self.question(event, actor,
                            {
                                "state": "needs_input",
                                "summary": "请选择世界。",
                                "choices": data.get("items", [])[:5],
                            }
                        )
                    if world_field in params and params[world_field] not in {world, matches[0]["id"]}:
                        raise QueryError("PARAMETER_INVALID")
                    params[world_field] = matches[0]["id"]
                try:
                    validate(params, {**item["parameters"], "required": []})
                except QueryError:
                    return encode({"state": "needs_input", "code": "PARAMETER_INVALID",
                                   "summary": "参数未通过校验，本次没有执行。请按以下结构纠正查询参数；不要猜测或改变用户指定的修改操作。",
                                   "items": [discover_item(item)]})
                # Confirmation follows the capability's risk tier (decision.py),
                # identically for direct commands and model tool calls.
                intent = Intent("capability", room=room, capability=capability, parameters=params)
            if not self.plugin.state.current_request(actor.key, message_id):
                return encode({"state": "superseded", "summary": "用户已发起新的请求，停止本轮工具调用；不要覆盖后续选择。"})
            from dataclasses import replace as _replace
            remembered = self.plugin.state.target(actor.key)
            exec_target = reference or Target(room)
            if (
                reference is None
                and not (parsed and len(parsed) > 2 and parsed[2])
                and isinstance(remembered, Target)
                and remembered.room == room
                and remembered.player
            ):
                if player in PLAYER_REFERENCES and needs_reference(intent):
                    exec_target = _replace(remembered, source="focus")
                elif name in {"dst_servers", "dst_world", "dst_server_action"} and view != "players":
                    exec_target = remembered
            result = await self.plugin.engine.execute(actor, intent, exec_target)
            if getattr(result, "outcome", "") == "needs_selection":
                pending = self.plugin.state.pending_form(actor.key)
                if pending is not None:
                    # The model relays this prompt; AstrBot core sends it, so
                    # its message id is unknown. Quoting the relay may confirm.
                    pending["relayed"] = True
                return self.question(event, actor, {"state": "needs_input", "summary": result.text,
                                                    "note": "等待用户回复；不得自行选择或代确认。"})
            self.plugin.model_presentation.remember(
                event, actor, self.plugin.connection_identity(), kind, result, room
            )
            text = result.text
            if (
                name == "dst_servers"
                and view == "overview"
                and "performance" in actor.visibility
                and re.search(r"cpu|内存|性能|负载", original, re.I)
            ):
                text += f"\n（提示：历史 CPU、内存与模拟评分请继续调用 dst_servers(room={room!r}, view='performance', parameters={{'window': '24h'}})）"
            if name in READ_TOOLS and isinstance(getattr(result, "data", None), dict):
                return await self.read_evidence(actor, text, result.data, room)
            if name in WRITE_TOOLS:
                message = str(getattr(event.message_obj, "message_id", ""))
                key = hashlib.sha256(json.dumps((*actor.key, message)).encode()).hexdigest()
                record = (await self.plugin.load_journal()).get(key)
                if record:
                    text += (
                        "\n任务编号：" + record["requestId"] + "（使用 dst_result 查询，勿重发）"
                    )
                    if record.get("state") == "uncertain" or record.get("data", {}).get("status") == "uncertain":
                        event._dst_uncertain_write = (actor.projection, message_id, text)
            return text
        except QueryError as error:
            from .engine import ERRORS
            # A mistyped result reference does not turn the original uncertain
            # mutation into a permission failure or authorize a replacement.
            if name == "dst_result" and actor is not None:
                pending = getattr(event, "_dst_uncertain_write", None)
                if pending and pending[:2] == (actor.projection, message_id):
                    return pending[2]
            return encode(
                {"state": "unavailable", "summary": error.message or ERRORS.get(error.code, "请求未完成；未自动重试。"), "code": error.code,
                 "details": error.details, "callId": error.call_id}
            )
        except (ValueError, TypeError, KeyError):
            return encode({"state": "needs_input", "summary": "参数无效，请按工具说明填写。"})

    async def find(self, actor, query):
        query = query.strip().casefold()
        if not query or len(query) > 128:
            raise QueryError("PARAMETER_INVALID")
        items = await self.plugin.directory.items(actor, time.monotonic() + 8)
        unsupported = self.plugin.directory.unsupported.get(actor.read_projection, [])
        unavailable = [c for c in unsupported if permitted(actor, c) and c.get("title")
                       and (query in c["title"].casefold() or c["title"].casefold() in query)]
        rows = []
        for item in items:
            if not permitted(actor, item) or discovery_negated(query, item.get("negatives")):
                continue
            aliases = item.get("aliases") if isinstance(item.get("aliases"), list) else []
            search_terms = item.get("searchTerms") if isinstance(item.get("searchTerms"), list) else []
            examples = item.get("examples") if isinstance(item.get("examples"), list) else []
            policies = item.get("actionPolicies") if isinstance(item.get("actionPolicies"), dict) else {}
            terms = [
                item["id"],
                item.get("title", ""),
                *TERMS.get(item["id"], "").split(),
                *search_terms[:64],
                *aliases[:64],
            ]
            matched = False
            for action in discover_actions(item):
                policy = policies.get(action["id"]) if isinstance(policies.get(action["id"]), dict) else {}
                if discovery_negated(query, policy.get("negatives")):
                    continue
                action_terms = [action["id"], action["title"], *action["searchTerms"], *(policy.get("aliases") or [])[:64]]
                score = max(discovery_score(query, action_terms), example_score(query, policy.get("examples")))
                if score:
                    card = discover_item(item)
                    card.update(title=action["title"], actionId=action["id"], description=action["description"])
                    if policy.get("tier"):
                        card["tier"] = policy["tier"]
                    if policy.get("confirm"):
                        card["confirm"] = policy["confirm"]
                    card["input"].setdefault("parameters", {}).update(action["parameters"])
                    prefer = policy.get("prefer") or item.get("prefer") or ""
                    rows.append({"id": item["id"], "score": score + 100, "card": card, "prefer": prefer})
                    matched = True
            if not matched:
                score = max(discovery_score(query, terms), example_score(query, examples))
                if score:
                    rows.append({"id": item["id"], "score": score, "card": discover_item(item),
                                 "prefer": item.get("prefer") or ""})
        best = {}
        for row in rows:
            best[row["id"]] = max(best.get(row["id"], 0), row["score"])
        for row in rows:
            prefer = row["prefer"]
            if prefer and prefer != row["id"] and prefer in best and row["score"] >= best[prefer]:
                row["score"] = best[prefer] - 1
        rows.sort(key=lambda r: (-r["score"], r["id"], r["card"].get("actionId", "")))
        return encode(
            {
                "items": [] if unavailable else [r["card"] for r in rows[:3]],
                "hasMore": len(rows) > 3,
                "note": "仅返回候选，尚未执行。查询、否定、条件或讨论不能当作操作请求；缺少目标或开关时请询问。权限与确认沿用原能力。",
                "needsUpdate": [
                    {"id": c["id"], "title": c["title"], "status": c["status"]}
                    for c in unavailable
                ][:3],
            }
        )

    async def read_evidence(self, actor, text, data, room):
        context = {}
        if data.get("capabilityId") in {"player.activity", "player.trajectory", "player.combat"}:
            context["scope"] = "本次结果涵盖该玩家在房间内各世界的记录；按每条 worldId 分组，不用增加 world 参数重复查询。分页或覆盖缺口仍以原结果为准。"
            facts = data.get("data")
            rows = facts.get("items", []) if isinstance(facts, dict) else []
            rows = rows if isinstance(rows, list) else []
            ids = {r.get("worldId") for r in rows[:50] if isinstance(r, dict) and isinstance(r.get("worldId"), str)}
            if ids:
                try:
                    worlds = await self.plugin.gateway.call(actor, "api_listRoomWorlds",
                        {"path": {"roomId": room}}, time.monotonic() + 3)
                    world_rows = worlds.get("items", [])
                    if not isinstance(world_rows, list):
                        raise QueryError("MCP_RESPONSE_INVALID")
                    context["worlds"] = [{"id": r["id"], "name": str(r.get("name", ""))[:255], "role": str(r.get("role", ""))[:32]}
                                         for r in world_rows[:50] if isinstance(r, dict) and r.get("id") in ids]
                except QueryError:
                    context["worldNamesUnavailable"] = True
        return read_result(text, data, context=context)

    async def result(self, actor, task_ref):
        owner = hashlib.sha256(json.dumps(actor.key).encode()).hexdigest()
        record = next(
            (
                r
                for r in (await self.plugin.load_journal()).values()
                if r.get("requestId") == task_ref and r.get("owner") == owner
            ),
            None,
        )
        if not record:
            record = await self.plugin.read_receipts.find(actor, task_ref)
            # This rechecks the current room, capability and native read grant.
            data = await self.plugin.engine.actions.get_job(actor, record, time.monotonic() + 8)
            body, footer = business(data)
            return await self.read_evidence(actor, body + "\n" + footer, data, record["roomId"])
        return await self.plugin.engine.actions.read(actor, record, time.monotonic() + 8)
