"""Deterministic orchestration. Commands and model wrappers share execute()."""

import asyncio
import time
from dataclasses import dataclass, replace

from . import formatters as fmt
from .capabilities import parse_parameter, permitted
from .commands import allowed, menu_commands
from .gateway import QueryError
from .presentation import business, options, render
from .queries import Queries
from .routing import Intent, parse
from .state import Target


@dataclass
class Reply:
    outcome: str
    text: str = ""
    target: Target | None = None
    choice: bool = False
    sequence: int = 0


ERRORS = {
    "PERMISSION_DENIED": "当前身份或会话未开放此操作。",
    "NATIVE_PERMISSION_DENIED": "AstrBot 原生工具权限未开放，请管理员检查工具权限。",
    "ROOM_DENIED": "此会话未开放该房间。",
    "QUERY_TIMEOUT": "读取超时，没有确认结果，也没有重新执行。",
    "PANEL_BUSY": "面板查询繁忙，请稍后重试。",
    "USER_BUSY": "你已有两项查询在处理，请稍等。",
    "RESULT_UNCERTAIN": "操作结果尚未确认，请查询原任务，不要重复提交。",
    "CAPABILITY_MISSING": "面板缺少该能力，请检查版本和 MCP 连接。",
    "PARAMETER_INVALID": "参数类型或范围不正确，请按提示重新输入，或取消。",
    "DIRECTORY_CHANGED": "能力目录已改变，请重新打开菜单。",
    "CONFIG_CHANGED": "会话配置已改变，请重新选择。",
}


class Engine:
    def __init__(
        self,
        gateway,
        state,
        aliases=None,
        default_room="",
        bindings=None,
        actions=None,
        directory=None,
        settings=None,
    ):
        self.gateway, self.state = gateway, state
        self.aliases, self.default_room, self.bindings = aliases or {}, default_room, bindings or {}
        self.actions, self.directory, self.settings = actions, directory, settings
        self.queries = Queries(gateway)

    def config(self, actor):
        return self.settings(actor.scope[-1]) if self.settings else None

    def parse(self, actor, text):
        config = self.config(actor)
        aliases = dict(config["roomAliases"]) if config else self.aliases
        custom = config["commandAliases"].get(text.strip()) if config else None
        if custom:
            if custom.startswith("cap:"):
                return Intent("capability", capability=custom[4:])
            return Intent(custom) if custom in {c.kind for c in menu_commands(actor)} else None
        if text.strip() in {"开启自动备份", "关闭自动备份"}:
            return Intent(
                "capability",
                capability="room.backups.configure",
                parameters={"enabled": text.strip() == "开启自动备份"},
            )
        if text.startswith("功能 "):
            from json import loads

            capability, _, values = text[3:].strip().partition(" ")
            try:
                parameters = loads(values) if values else None
                return Intent("capability", capability=capability, parameters=parameters)
            except ValueError:
                return None
        return parse(text, aliases)

    async def handle(self, actor, text, *, woken=False, quote="", message_id=""):
        if not woken and not self.state.handles(actor.key, text.strip(), quote, woken):
            return Reply("pass")
        # Duplicate deliveries must not consume another person's/current choice.
        if self.state.duplicate(actor.scope, message_id):
            return Reply("matched")
        intent = self.parse(actor, text)
        if text in {"下一页", "上一页"} and self.state.handles(actor.key, text, quote, woken):
            pending = self.state.page(actor.key, 1 if text == "下一页" else -1)
            return Reply(
                "needs_selection", options(pending), choice=True, sequence=pending["sequence"]
            )
        selection = self.state.selection(actor.key, text.strip(), quote, woken)
        if (
            selection is None
            and self.state.choice_matches(actor.key, quote)
            and self.state.number(text)
        ):
            return Reply("handled_error", "序号不在当前页，请按最新菜单中的序号选择，或回复 取消。")
        context = self.state.quoted(actor.scope, quote) or self.state.target(actor.key)
        form = self.state.pending_form(actor.key)
        frozen = None
        if selection:
            pending, index = selection
            if pending.get("configRevision", actor.config_revision) != actor.config_revision:
                return Reply("handled_error", ERRORS["CONFIG_CHANGED"])
            item = pending["candidates"][index]
            intent = pending["intent"]
            if pending.get("mode") == "menu":
                intent = Intent(item["kind"], capability=item.get("capability", ""))
                target = context
            elif pending.get("mode") == "world":
                form = pending["form"]
                form["parameters"][form["field"]] = item["id"]
                intent, target, frozen = form["intent"], form["target"], form
            elif pending["room"]:
                target = Target(pending["room"], item["id"], item.get("name", ""))
                intent = replace(intent, player=item["id"], room=pending["room"])
            else:
                target = Target(item["id"])
        elif form and intent is None:
            if form.get("configRevision") != actor.config_revision:
                self.state.clear_form(actor.key)
                return Reply("handled_error", ERRORS["CONFIG_CHANGED"])
            if form["type"] == "player":
                intent, target = replace(form["intent"], player=text.strip()), form["target"]
            else:
                try:
                    value = (
                        form["schema"]["default"]
                        if text == "默认" and "default" in form["schema"]
                        else parse_parameter(text, form["schema"])
                    )
                except QueryError:
                    return Reply("handled_error", ERRORS["PARAMETER_INVALID"])
                form["parameters"][form["field"]] = value
                intent, target, frozen = form["intent"], form["target"], form
        else:
            target = context
        if intent is None:
            return Reply("pass")
        if intent.kind == "cancel":
            self.state.begin(actor.key)
            return Reply("matched", "已取消选择；已经提交的任务不会因此停止。")
        config = self.config(actor)
        default = config["defaultRoom"] if config else self.default_room
        if target is None:
            target = Target("")
        if intent.room:
            target = target if target.room == intent.room else Target(intent.room)
        if not target.room:
            target = Target(default or (actor.rooms[0] if len(actor.rooms) == 1 else ""))
        return await self.execute(actor, intent, target, frozen=frozen)

    async def execute(self, actor, intent, target=None, *, frozen=None):
        if intent.kind != "capability" and not allowed(actor, intent.kind):
            return Reply("denied", ERRORS["PERMISSION_DENIED"])
        seq = self.state.begin(actor.key)
        config = self.config(actor)
        timeout = (
            (config["inventoryTimeout"] if intent.kind == "inventory" else config["queryTimeout"])
            if config
            else (30 if intent.kind == "inventory" else 8)
        )
        try:
            async with asyncio.timeout(timeout):
                return await self._perform(
                    actor,
                    intent,
                    target or Target(intent.room),
                    seq,
                    time.monotonic() + timeout,
                    frozen,
                )
        except asyncio.CancelledError:
            raise
        except (TimeoutError, QueryError) as error:
            self.state.suspend(actor.key, seq)
            code = error.code if isinstance(error, QueryError) else "QUERY_TIMEOUT"
            return Reply(
                "handled_error",
                ERRORS.get(
                    code, "请求未完成（" + fmt.safe(code) + "）。本次没有转交模型或重复执行。"
                ),
            )
        except Exception:
            self.state.suspend(actor.key, seq)
            return Reply(
                "handled_error", "面板数据暂时无法识别，请运行机器人自检；本次没有重复执行。"
            )

    def choose(self, actor, intent, rows, room, seq, title, mode="target", form=None):
        if not self.state.choose(actor.key, intent, rows, room, seq, mode, actor.page_size):
            return Reply("matched")
        pending = self.state.choices[actor.key]
        pending["configRevision"] = actor.config_revision
        if form:
            pending["form"] = form
        self.state._put(self.state.choices, actor.key, pending)
        self.state.suspend(actor.key, seq)
        return Reply("needs_selection", options(pending, title), choice=True, sequence=seq)

    async def _menu(self, actor, intent, seq, deadline):
        rows = [
            {"id": c.kind, "name": c.label, "kind": c.kind}
            for c in menu_commands(actor)
            if c.kind != "job" or self.actions
        ]
        known = {
            "room.status",
            "room.players",
            "room.network",
            "player.resolve",
            "player.inventory",
            "room.mods",
            "room.backups.settings",
            "room.backup.create",
            "room.start",
            "room.stop",
            "room.restart",
            "player.kick",
        }
        note = ""
        if self.directory:
            try:
                async with asyncio.timeout(min(2, max(0.01, deadline - time.monotonic()))):
                    items = await self.directory.items(actor, deadline)
                for c in items:
                    if c["id"] in known or not permitted(actor, c):
                        continue
                    name = (
                        "api_runBotCapability"
                        if c["access"] == "write"
                        else "api_queryBotCapability"
                    )
                    try:
                        await self.gateway.authorize(
                            actor,
                            name,
                            {
                                "path": {
                                    "roomId": actor.rooms[0] if actor.rooms else "",
                                    "capabilityId": c["id"],
                                }
                            },
                        )
                    except QueryError:
                        continue
                    rows.append(
                        {
                            "id": c["id"],
                            "name": c["title"],
                            "kind": "capability",
                            "capability": c["id"],
                        }
                    )
            except (QueryError, TimeoutError):
                note = "\n扩展目录暂不可用；固定指令仍可使用。"
        config = self.config(actor)
        if config:
            lookup = {r["id"]: r for r in rows}
            ordered = []
            for key in config["menuItems"]:
                if key == "*":
                    ordered.extend(r for r in rows if r not in ordered)
                elif key in lookup and lookup[key] not in ordered:
                    ordered.append(lookup[key])
            rows = ordered
        result = self.choose(actor, intent, rows, "", seq, "可用查询与管理", "menu")
        guidance = (
            "其他问题可直接 @机器人询问，也可使用 AI: 问题。"
            if not config or config["mode"] == "mixed"
            else "复杂问题使用 AI: 问题。"
        )
        result.text += note + "\n也可直接说：有哪些服、1服谁在线、查背包 昵称。" + guidance
        return result

    async def _perform(self, actor, intent, target, seq, deadline, frozen=None):
        if intent.kind == "help":
            return await self._menu(actor, intent, seq, deadline)
        if intent.kind == "stop_tracking" and self.actions:
            return Reply("matched", self.actions.stop_tracking(actor))
        rooms = await self.queries.rooms(actor, deadline)
        if intent.kind == "rooms":
            return (
                self.choose(actor, Intent("status"), rooms, "", seq, "可访问房间")
                if rooms
                else Reply("matched", "当前没有可访问的房间。")
            )
        if intent.kind == "status" and intent.all_rooms:
            if not rooms:
                return Reply("matched", "当前没有可访问的房间。")
            # Each room recipe already uses two reads, matching the per-user
            # gateway limit. Do not turn a two-room overview into USER_BUSY.
            semaphore = asyncio.Semaphore(1)

            async def summary(room):
                async with semaphore:
                    try:
                        text, _ = await self.queries.run(
                            actor, "status", Target(room["id"]), None, deadline
                        )
                        text = fmt.safe(room.get("name")) + "\n" + text
                        return (
                            text
                            if len(text) <= 480
                            else text[:430] + "\n（概览已节选，请按房间名查询详情。）"
                        )
                    except (QueryError, TimeoutError):
                        return (
                            fmt.safe(room.get("name"))
                            + "\n状态暂未读取成功，不能视为停服或无人在线。"
                        )

            parts = await asyncio.gather(*(summary(r) for r in rooms[:6]))
            text = "\n\n────────────\n".join(parts)
            if len(rooms) > 6:
                text += "\n仅展示前 6 个房间，请按房间名查询其他房间。"
            return Reply("matched", text)
        if intent.room_name:
            matches = [r for r in rooms if r.get("name") == intent.room_name]
            if len(matches) != 1:
                return Reply("handled_error", "没有唯一匹配的房间，请使用房间列表选择。")
            target = Target(matches[0]["id"])
        if target.room and not any(r["id"] == target.room for r in rooms):
            raise QueryError("ROOM_DENIED")
        if not target.room:
            if len(rooms) == 1:
                target = Target(rooms[0]["id"])
            elif rooms:
                return self.choose(actor, intent, rooms, "", seq, "请选择房间")
            else:
                return Reply("matched", "当前没有可访问的房间。")
        room = next(r for r in rooms if r["id"] == target.room)
        title = fmt.safe(room.get("name"))
        if intent.kind == "capability":
            return await self._dynamic(actor, intent, target, seq, deadline, title, frozen)
        player = None
        if intent.kind in {"inventory", "profile", "network", "kick"}:
            query = intent.player
            config = self.config(actor)
            bindings = config["bindings"] if config else self.bindings
            if query in {"我", "自己"}:
                query = bindings.get(actor.user, "")
                if not query:
                    return Reply("needs_selection", "尚无已验证的 QQ→玩家绑定，请使用游戏昵称。")
            elif query in {"他", "他的"}:
                query = ""
            if query or target.player:
                data = await self.queries.resolve(
                    actor, target.room, query or target.player, deadline
                )
                if data.get("status") in {"unavailable", "not_found"}:
                    return Reply(
                        "handled_error",
                        title + "：玩家目录中没有确认的匹配记录；不代表玩家从未进入。",
                    )
                items = data.get("items", [])
                if (
                    data.get("status") == "ambiguous"
                    or intent.kind == "kick"
                    and data.get("match") not in {"exact_id", "exact_name"}
                ):
                    return self.choose(actor, intent, items, target.room, seq, "请确认玩家")
                if data.get("status") != "resolved" or len(items) != 1:
                    raise QueryError("MCP_RESPONSE_INVALID")
                player = items[0]
                target = Target(target.room, player["id"], player.get("name", ""))
            elif intent.kind != "network" or intent.player == "他":
                self.state.form(
                    actor.key,
                    {
                        "type": "player",
                        "intent": intent,
                        "target": target,
                        "configRevision": actor.config_revision,
                    },
                )
                return Reply("needs_selection", "请 @机器人提供玩家昵称或 KU ID；取消退出。")
        if intent.kind == "job" and self.actions:
            text = await self.actions.latest(actor, target.room, deadline)
        elif intent.kind in {"backup", "restart", "start", "stop", "kick"} and self.actions:
            cap = {"backup": "room.backup.create", "kick": "player.kick"}.get(
                intent.kind, "room." + intent.kind
            )
            if cap not in actor.write_allowlist:
                raise QueryError("PERMISSION_DENIED")
            text = await self.actions.submit(actor, intent, target, deadline)
        else:
            text, target = await self.queries.run(actor, intent.kind, target, player, deadline)
        config = self.config(actor)
        template = (
            config["templates"].get(intent.kind, "{title}\n{body}") if config else "{title}\n{body}"
        )
        if intent.kind in {"backup", "restart", "start", "stop", "kick", "job"}:
            result = title + "\n" + text
        else:
            result = render(template, title=title, body=text, room=title, player=target.name)
            if text not in result:
                result = result[:500] + "\n" + text
        # Required evidence remains outside customizable templates.
        if config and intent.kind not in {"backup", "restart", "start", "stop", "kick", "job"}:
            result += "\n数据来源：面板；仅限本次可见记录，缺失不代表 0。"
        if config and config["style"] == "detailed":
            result += "\n房间 ID：" + fmt.safe(target.room)
            if target.player:
                result += " · 玩家 ID：" + fmt.safe(target.player)
        self.state.remember(actor.key, seq, target, result)
        return Reply("matched", result, target)

    async def _dynamic(self, actor, intent, target, seq, deadline, title, frozen):
        if not self.directory:
            raise QueryError("CAPABILITY_MISSING")
        items = await self.directory.items(actor, deadline)
        item = next((c for c in items if c["id"] == intent.capability), None)
        if not item or not permitted(actor, item):
            raise QueryError("PERMISSION_DENIED")
        if frozen and frozen["item"]["schemaRevision"] != item["schemaRevision"]:
            raise QueryError("DIRECTORY_CHANGED")
        parameters = dict(frozen["parameters"] if frozen else intent.parameters or {})
        schema = item["parameters"]
        for key, hint in item.get("parameterHints", {}).items():
            if (
                hint == "player"
                and key in parameters
                and not str(parameters[key]).startswith("KU_")
            ):
                resolved = await self.queries.resolve(actor, target.room, parameters[key], deadline)
                if resolved.get("status") == "ambiguous":
                    form = {
                        "type": "parameter",
                        "intent": intent,
                        "target": target,
                        "item": item,
                        "parameters": parameters,
                        "field": key,
                        "schema": schema["properties"][key],
                        "configRevision": actor.config_revision,
                    }
                    return self.choose(
                        actor,
                        intent,
                        resolved.get("items", []),
                        target.room,
                        seq,
                        "请确认玩家",
                        "world",
                        form,
                    )
                if resolved.get("status") != "resolved" or len(resolved.get("items", [])) != 1:
                    return Reply("handled_error", "没有确认的唯一玩家，请提供更完整昵称或 KU ID。")
                parameters[key] = resolved["items"][0]["id"]
        for key, prop in schema.get("properties", {}).items():
            if key in parameters or key not in schema.get("required", []) and "default" not in prop:
                continue
            form = {
                "type": "parameter",
                "intent": intent,
                "target": target,
                "item": item,
                "parameters": parameters,
                "field": key,
                "schema": prop,
                "configRevision": actor.config_revision,
            }
            if item.get("parameterHints", {}).get(key) == "world":
                worlds = await self.gateway.call(
                    actor, "api_listRoomWorlds", {"path": {"roomId": target.room}}, deadline
                )
                return self.choose(
                    actor,
                    intent,
                    worlds.get("items", []),
                    target.room,
                    seq,
                    "请选择世界",
                    "world",
                    form,
                )
            if "default" in prop and item["access"] == "read":
                parameters[key] = prop["default"]
                continue
            self.state.form(actor.key, form)
            hint = "；可选：" + json_values(prop["enum"]) if "enum" in prop else ""
            if "default" in prop:
                hint += "；默认 " + fmt.safe(prop["default"]) + "，回复 默认 使用"
            return Reply(
                "needs_selection",
                fmt.safe(item["title"])
                + "：请提供 "
                + key
                + "（"
                + prop["type"]
                + "）"
                + hint
                + "。取消退出。",
            )
        if item["access"] == "write":
            if not self.actions:
                raise QueryError("CAPABILITY_MISSING")
            text = await self.actions.submit_capability(actor, item, target, parameters, deadline)
            result = title + "\n" + text
        else:
            data = await self.directory.execute(actor, item, target.room, parameters, deadline)
            body, footer = business(data)
            config = self.config(actor)
            template = (
                config["templates"].get(item["id"], "{title}\n{body}")
                if config
                else "{title}\n{body}"
            )
            result = (
                render(
                    template, title=title + " · " + fmt.safe(item["title"]), body=body, room=title
                )
                + "\n"
                + footer
            )
        self.state.remember(actor.key, seq, target, result)
        return Reply("matched", result, target)


def json_values(values):
    return " / ".join(fmt.safe(v) for v in values[:12])
