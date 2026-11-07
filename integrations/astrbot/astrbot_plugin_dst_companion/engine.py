"""Deterministic orchestration. Commands and model wrappers share execute()."""

import asyncio
import time
from dataclasses import dataclass, replace

from . import formatters as fmt
from .capabilities import permitted
from .commands import allowed, menu_commands
from .display import room_heading
from .display import room_label as short_room_label
from .display import text as display_text
from .gateway import QueryError
from .parameter_ui import answer as parameter_answer
from .parameter_ui import describe as parameter_description
from .parameter_ui import prompt as parameter_prompt
from .presentation import business, options, render
from .queries import Queries
from .routing import READ_KINDS, Intent, form_reply, is_cancel_reply, parse
from .state import Target


@dataclass
class Reply:
    outcome: str
    text: str = ""
    target: Target | None = None
    choice: bool = False
    sequence: int = 0
    data: dict | None = None


ERRORS = {
    "PERMISSION_DENIED": "当前身份或会话未开放此操作。",
    "NATIVE_PERMISSION_DENIED": "AstrBot 原生工具权限未开放，请管理员检查工具权限。",
    "ROOM_DENIED": "此会话未开放该房间。",
    "QUERY_TIMEOUT": "读取超时，没有确认结果，也没有重新执行。",
    "PANEL_BUSY": "面板查询繁忙，请稍后重试。",
    "USER_BUSY": "你已有两项查询在处理，请稍等。",
    "RESULT_UNCERTAIN": "操作结果尚未确认，请查询原任务，不要重复提交。",
    "CAPABILITY_MISSING": "面板缺少该能力，请检查版本和 MCP 连接。",
    "CLIENT_UPDATE_REQUIRED": "这项操作需要更新插件，其他查询可继续使用；也可先在面板操作。",
    "PARAMETER_INVALID": "参数类型或范围不正确，请按提示重新输入，或取消。",
    "DIRECTORY_CHANGED": "能力目录已改变，请重新打开菜单。",
    "CONFIG_CHANGED": "会话配置已改变，请重新选择。",
    "MCP_RESPONSE_INVALID": "面板返回的数据格式无法识别，本次读取失败，请检查连接或版本。",
    "WORLD_NOT_RUNNING": "该世界未运行，无法读取当前游戏数据。",
    "PANEL_DISCONNECTED": "面板连接已断开，本次没有取得查询结果。",
}


DIRECT_CAPABILITIES = {
    "restart": "room.restart",
    "start": "room.start",
    "stop": "room.stop",
    "backup": "room.backup.create",
    "kick": "player.kick",
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
        message_renderer=None,
    ):
        self.gateway, self.state = gateway, state
        self.aliases, self.default_room, self.bindings = aliases or {}, default_room, bindings or {}
        self.actions, self.directory, self.settings = actions, directory, settings
        self.queries = Queries(gateway)
        self.message_renderer = message_renderer

    def decorate(self, actor, kind, title, body, room="", player="", full_room="", legacy=None):
        values = {
            "title": title,
            "body": body,
            "room": room,
            "player": player,
            "room_name": room,
            "room_full_name": full_room or room,
            "time": "时间请见各数据采样记录",
        }
        if self.message_renderer:
            return self.message_renderer(actor.scope[-1], kind, values, legacy=legacy)
        result = render(
            legacy or "{title}\n{body}", title=title, body=body, room=room, player=player
        )
        # Both the legacy and new paths must preserve the entire business body.
        return result if body in result else title + "\n" + body

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

    async def handle(self, actor, text, *, woken=False, quote="", message_id="", reference=None):
        if not woken and not self.state.handles(actor.key, text.strip(), quote, woken):
            return Reply("pass")
        # Duplicate deliveries must not consume another person's/current choice.
        if self.state.duplicate(actor.scope, message_id):
            return Reply("matched")
        pending = self.state.choice_matches(actor.key, quote) or self.state.pending_form(actor.key)
        intent = Intent("cancel") if pending and is_cancel_reply(text) else self.parse(actor, text)
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
        # An unknown explicit reply must not silently select the last player.
        context = self.state.quoted(actor.scope, quote) if quote else self.state.target(actor.key)
        if context is not None and context.player:
            context = replace(context, source="quote" if quote else "focus")
        if reference is not None:
            from .references import needs_reference

            if needs_reference(intent) and (not intent.room or intent.room == reference.room):
                context = replace(reference, source="quote")
        if intent and intent.kind == "network" and not intent.player and (quote or reference):
            # An explicit reply asks about that player. An unverified reply
            # needs a name; it must not silently become a room-wide query.
            intent = replace(intent, player=context.player if context and context.player else "他")
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
                if form["item"].get("parameterHints", {}).get(form["field"]) == "player":
                    form["target"] = Target(form["target"].room, item["id"], item.get("name", ""))
                intent, target, frozen = form["intent"], form["target"], form
            elif pending["room"]:
                target = Target(pending["room"], item["id"], item.get("name", ""))
                intent = replace(intent, player=item["id"], room=pending["room"])
            else:
                target = Target(item["id"])
        elif form and intent is None:
            from .decision import confirmation_reply, looks_like_confirmation
            confirming = form["type"] == "confirmation" and looks_like_confirmation(text)
            relayed = form.get("relayed") and not form.get("messageId")
            if confirming and quote and quote != form.get("messageId") and not relayed:
                # A known prompt exists and the user quoted something else
                # (a notice, another prompt): never confirm through it.
                return Reply("handled_error", "请引用当前这条确认提示，或不引用直接回复；引用其他消息不会确认或更换已准备好的操作。尚未执行。")
            # A prompt relayed by the model has no recorded message id; quoting
            # it while confirming answers this user's own pending operation.
            if quote and quote != form.get("messageId") and reference is None and not confirming:
                return Reply("pass")
            if form.get("configRevision") != actor.config_revision:
                self.state.clear_form(actor.key)
                return Reply("handled_error", ERRORS["CONFIG_CHANGED"])
            if confirming:
                overrides = confirmation_reply(text, form, form.get("roomName", ""))
                if overrides is None:
                    expected = form.get("phrase") or "确认"
                    return Reply("needs_selection", "尚未执行。请回复「" + expected + "」执行这项操作，或回复 取消。")
                form["parameters"].update(overrides)
                form["userConfirmed"] = True
                intent, target, frozen = form["intent"], form["target"], form
                answer = text
            else:
                answer = form_reply(text)
            if answer is None:
                self.state.begin(actor.key)
                return Reply("pass")
            if form["type"] == "confirmation":
                if not form.get("userConfirmed"):
                    expected = form.get("phrase") or "确认"
                    return Reply("needs_selection", "请回复「" + expected + "」执行这项操作，或回复 取消。")
            elif form["type"] == "player":
                intent, target = replace(form["intent"], player=reference.player if reference else answer), reference or form["target"]
            else:
                try:
                    value = (
                        form["schema"]["default"]
                        if text == "默认" and "default" in form["schema"]
                        else parameter_answer(reference.player if reference and form["item"].get("parameterHints", {}).get(form["field"]) == "player" else answer,
                                              form["schema"], item=form["item"], key=form["field"])
                    )
                except QueryError:
                    return Reply("handled_error", ERRORS["PARAMETER_INVALID"])
                form["parameters"][form["field"]] = value
                if reference and form["item"].get("parameterHints", {}).get(form["field"]) == "player":
                    form["target"] = reference
                intent, target, frozen = form["intent"], form["target"], form
        else:
            target = context
        if (
            intent is None
            and woken
            and self.directory
            and actor.role in {"game-manage", "panel-manage"}
        ):
            from .routing import COMPOUND, DISCUSSION, explicit_ai, normalize

            value = normalize(text)
            if len(value) <= 128 and not (
                COMPOUND.search(value) or DISCUSSION.search(value) or explicit_ai(value)
            ):
                try:
                    items = await self.directory.items(actor, time.monotonic() + 4)
                    matches = [
                        item
                        for item in items
                        if item.get("resultKind") == "workbench-run"
                        and item.get("title") == value
                        and permitted(actor, item)
                    ]
                    from .interactions import direct_request
                    direct=direct_request(value,items,actor)
                    if direct:
                        intent=Intent("capability",capability=direct[0],parameters=direct[1])
                    elif len(matches) == 1:
                        intent = Intent("capability", capability=matches[0]["id"])
                    elif not matches:
                        from .discovery import actions as discover_actions
                        candidates = []
                        for item in items:
                            if not permitted(actor, item):
                                continue
                            for action in discover_actions(item):
                                if not permitted(actor,item,action["parameters"]):
                                    continue
                                for term in dict.fromkeys([action["title"], *action["searchTerms"]]):
                                    if value == term or value.startswith(term + " "):
                                        params = dict(action["parameters"])
                                        nickname = value[len(term):].strip()
                                        hints = [k for k,h in item.get("parameterHints", {}).items() if h == "player"]
                                        if nickname:
                                            if len(hints) != 1: continue
                                            params[hints[0]] = nickname
                                        candidates.append((item, action, params))
                                        break
                        if len(candidates) == 1:
                            item, action, params = candidates[0]
                            intent = Intent("capability", capability=item["id"], parameters=params)
                except QueryError:
                    pass
        if intent is None:
            return Reply("pass")
        if intent.kind == "cancel":
            if form and quote and quote != form.get("messageId") and reference is None:
                return Reply("pass")
            self.state.begin(actor.key)
            if form and form.get("teleportPreview"):
                preview = form["teleportPreview"]
                try:
                    await self.gateway.call(actor, "api_cancelRoomTeleport", {
                        "path": {"roomId": form["target"].room, "previewId": preview["id"]}, "body": {}
                    }, time.monotonic() + 6, write=True)
                except QueryError:
                    pass  # Local confirmation is revoked; the remote permit expires.
                return Reply("matched", "已取消本次确认；已提交的操作仍需查看原结果。")
            from .private_connection import LEGACY_MOVEMENT
            if form and form.get("item", {}).get("id") in LEGACY_MOVEMENT and form.get("parameters", {}).get("preview_id"):
                try:
                    await self.directory.execute(actor, form["item"], form["target"].room,
                        {**form["parameters"], "phase":"cancel"}, time.monotonic()+6)
                except QueryError:
                    pass
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
        if intent.kind in DIRECT_CAPABILITIES and self.directory:
            # Shortcut words run the same published capability as AI tools, so
            # permission, confirmation and receipts never depend on phrasing.
            intent = replace(intent, kind="capability", capability=DIRECT_CAPABILITIES[intent.kind],
                             parameters={"playerId": intent.player} if intent.kind == "kick" and intent.player else {})
        seq = self.state.begin(actor.key)
        config = self.config(actor)
        timeout = (
            (config["inventoryTimeout"] if intent.kind == "inventory" else config["queryTimeout"])
            if config
            else (30 if intent.kind == "inventory" else 8)
        )
        if (
            intent.kind == "capability"
            and intent.capability
            in {
                "player.give_item",
                "player.spawn_entity",
                "player.resurrect",
                "player.teleport",
                "location.save",
            }
            or intent.kind == "capability"
            and intent.capability.startswith("workbench.")
        ):
            # Shard confirmation and the original write receipt share one bounded
            # deadline. A short read timeout must not cancel a valid item grant.
            timeout = config["inventoryTimeout"] if config else 30
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
                data=getattr(error, "receipt", None),
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
            "其他问题直接 @机器人提问，会自动理解并选择可用功能。"
            if not config or config["mode"] == "mixed"
            else "当前只开启了快捷查询；需要自由提问时，请管理员开启混合模式。"
        )
        result.text += note + "\n也可直接说：有哪些服、谁在线、查背包 昵称。" + guidance
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
                            actor,
                            "status",
                            Target(room["id"]),
                            None,
                            deadline,
                            room=room,
                            compact=True,
                        )
                        title, label = room_heading(room, rooms, "status")
                        text = self.decorate(
                            actor, "status", title, text, label, full_room=room.get("name", "")
                        )
                        return (
                            text
                            if len(text) <= 480
                            else text[:430] + "\n（概览已节选，请按房间名查询详情。）"
                        )
                    except (QueryError, TimeoutError):
                        return (
                            room_heading(room, rooms)[0]
                            + "\n状态暂未读取成功，不能视为停服或无人在线。"
                        )

            parts = await asyncio.gather(*(summary(r) for r in rooms[:6]))
            text = "\n\n────────────\n".join(parts)
            if len(rooms) > 6:
                text += "\n仅展示前 6 个房间，请按房间名查询其他房间。"
            return Reply("matched", text)
        if intent.room_name:
            matches = [
                r for r in rooms if intent.room_name in {r.get("name"), display_text(r.get("name"))}
            ]
            if not matches:
                matches = [r for r in rooms if short_room_label(r.get("name")) == intent.room_name]
            if len(matches) != 1:
                if rooms and intent.kind in READ_KINDS:
                    # A selection explicitly resolves the request, without
                    # inventing ordinal aliases or querying a remembered room.
                    explanation = "对应多个房间" if matches else "未匹配"
                    return self.choose(
                        actor,
                        replace(intent, room="", room_name=""),
                        matches or rooms,
                        "",
                        seq,
                        "房间名称“" + fmt.safe(intent.room_name) + "”" + explanation
                        + "，请选择要查询的房间",
                    )
                return Reply("handled_error", "没有唯一匹配的房间，请使用房间列表选择。")
            if target.room != matches[0]["id"]:
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
        title, room_label = room_heading(room, rooms, intent.kind)
        if intent.kind == "capability":
            return await self._dynamic(actor, intent, target, seq, deadline, title, frozen,
                                       room_name=str(room.get("name", "")))
        if intent.kind == "network" and not intent.player:
            # General connection questions describe the room's online players,
            # even after an earlier profile/inventory query selected someone.
            target = Target(target.room)
        player = None
        if intent.kind in {"inventory", "profile", "network", "kick"}:
            from .references import PLAYER_REFERENCES

            missing_binding = False
            query = intent.player
            config = self.config(actor)
            bindings = config["bindings"] if config else self.bindings
            if query in {"我", "自己"}:
                query = bindings.get(actor.user, "")
                if not query:
                    # Unknown self identity must ask again, even if a previous
                    # query remembered another player. Reuse the normal form.
                    target = Target(target.room)
                    intent = replace(intent, player="他")
                    missing_binding = True
            elif query in PLAYER_REFERENCES:
                query = ""
            if query or target.player:
                data = await self.queries.resolve(
                    actor, target.room, query or target.player, deadline
                )
                if data.get("status") in {"unavailable", "not_found"}:
                    self.state.form(actor.key, {"type": "player", "intent": replace(intent, player=""),
                        "target": Target(target.room), "configRevision": actor.config_revision}, sequence=seq)
                    return Reply(
                        "needs_selection",
                        title + ("：玩家信息暂时无法读取。" if data.get("status") == "unavailable"
                                 else "：没有找到对应的玩家；不代表玩家从未进入。")
                        + "请回复游戏昵称或 KU ID 重试，或回复取消。",
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
            elif intent.kind != "network" or intent.player in PLAYER_REFERENCES - {""}:
                self.state.form(
                    actor.key,
                    {
                        "type": "player",
                        "intent": intent,
                        "target": target,
                        "configRevision": actor.config_revision,
                    },
                    sequence=seq,
                )
                return Reply(
                    "needs_selection",
                    ("尚无已验证的 QQ→玩家绑定，请使用游戏昵称。\n" if missing_binding else "")
                    + "👤 要查看哪位玩家？\n2 分钟内可直接回复玩家昵称或 KU ID；也可引用玩家进退服通知后查询。取消退出。"
                    if intent.kind in {"profile", "inventory", "network"}
                    else "请 @机器人提供玩家昵称或 KU ID；取消退出。",
                )
        if intent.kind == "job" and self.actions:
            text = await self.actions.latest(actor, target.room, deadline)
        elif intent.kind in {"backup", "restart", "start", "stop", "kick"} and self.actions:
            cap = {"backup": "room.backup.create", "kick": "player.kick"}.get(
                intent.kind, "room." + intent.kind
            )
            if not actor.may_write(cap):
                raise QueryError("PERMISSION_DENIED")
            text = await self.actions.submit(actor, intent, target, deadline)
        else:
            text, target = await self.queries.run(
                actor, intent.kind, target, player, deadline, room=room
            )
        config = self.config(actor)
        template = (
            config["templates"].get(intent.kind, "{title}\n{body}") if config else "{title}\n{body}"
        )
        kind = (
            "action"
            if intent.kind in {"backup", "restart", "start", "stop", "kick", "job"}
            else intent.kind
        )
        result = self.decorate(
            actor,
            kind,
            title,
            text,
            room_label,
            target.name,
            full_room=room.get("name", ""),
            legacy=template if kind != "action" else None,
        )
        if config and config["style"] == "detailed":
            result += "\n房间 ID：" + fmt.safe(target.room)
            if target.player:
                result += " · 玩家 ID：" + fmt.safe(target.player)
        # A room-wide list with just one online player is not a personal
        # selection. It must not become the implicit target of a later write.
        remembered = Target(target.room) if intent.kind == "players" or intent.kind == "network" and not intent.player else target
        self.state.remember(actor.key, seq, remembered, result)
        return Reply("matched", result, target)

    async def _dynamic(self, actor, intent, target, seq, deadline, title, frozen, room_name=""):
        evidence = None
        if not self.directory:
            raise QueryError("CAPABILITY_MISSING")
        item = await self.directory.get(actor, intent.capability, deadline)
        # Dedicated and workbench entries must represent the same effect.
        # Falling back to the workbench must never turn full god mode into
        # ordinary damage immunity merely because permissions differ.
        if not frozen and intent.capability == "workbench.set_player_mode":
            values = intent.parameters or {}
            simple = {"super_god": "player.god_mode", "creative": "player.creative_mode"}.get(values.get("mode"))
            dedicated = await self.directory.get(actor, simple, deadline) if simple else None
            if dedicated and not permitted(actor, dedicated):
                dedicated = None
            if dedicated and set(values) <= {"player_id", "mode", "enabled"}:
                mapped = {"enabled": values["enabled"]} if "enabled" in values else {}
                if "player_id" in values:
                    mapped["playerId"] = values["player_id"]
                intent = replace(intent, capability=simple, parameters=mapped)
                item = dedicated
        if not item or not permitted(actor, item):
            raise QueryError("PERMISSION_DENIED")
        if frozen and frozen.get("previewUntil", float("inf")) <= self.state.clock():
            self.state.clear_form(actor.key)
            return Reply("handled_error", "预览已过期，请重新预览并确认范围。尚未执行。")
        if frozen and frozen["item"]["schemaRevision"] != item["schemaRevision"]:
            raise QueryError("DIRECTORY_CHANGED")
        if frozen and frozen.get("teleportPreview") and frozen.get("userConfirmed") is True:
            text = await self.actions.submit_teleport(actor, item, target, frozen["teleportPreview"], deadline)
            return Reply("matched", text, target)
        parameters = dict(frozen["parameters"] if frozen else intent.parameters or {})
        import copy
        schema = copy.deepcopy(item["parameters"])
        for row in item.get("actions",[]):
            if not permitted(actor,item,row.get("parameters",{})):
                for field,value in row.get("parameters",{}).items():
                    prop=schema.get("properties",{}).get(field,{})
                    if value in prop.get("enum",[]): prop["enum"].remove(value)
        from .interactions import effective_risk, interaction, preview_seconds
        selector_missing=any(row.get("parameters") and any(key not in parameters for key in row["parameters"]) for row in item.get("actions",[]) if isinstance(row,dict))
        if not permitted(actor, item, parameters) and not selector_missing:
            raise QueryError("PERMISSION_DENIED")
        critical = effective_risk(item,parameters) == "critical" and item["access"] == "write"
        from .decision import confirmation, confirmation_prompt, typed_phrase
        policy = confirmation(item, parameters) if item["access"] == "write" else "none"
        flow = interaction(item, parameters)
        from .private_connection import LEGACY_MOVEMENT
        movement = item["id"] in LEGACY_MOVEMENT and "phase" in schema.get("properties", {})
        if movement and not (frozen and frozen.get("userConfirmed") is True):
            # Phase/preview handles supplied by a model do not authorize an
            # existing plan. Always obtain this user's own frozen precheck.
            parameters.pop("preview_id", None)
            parameters["phase"] = "preview"
        movement_preview = movement and parameters.get("phase") == "preview"
        if flow and not (frozen and frozen.get("userConfirmed") is True):
            parameters.pop("preview_id", None)
            parameters["phase"] = "preview"
        nearby_preview = bool(flow and parameters.get("phase") == "preview")
        if critical or flow:
            parameters.pop("confirmed", None)
        from .references import PLAYER_REFERENCES
        player_fields = [key for key, hint in item.get("parameterHints", {}).items() if hint == "player"]
        for key in player_fields:
            value = parameters.get(key, "")
            if isinstance(value, str) and value in PLAYER_REFERENCES:
                if target.player and len(player_fields) == 1:
                    parameters[key] = target.player
                    target = replace(target, source=target.source or "focus")
                else:
                    parameters.pop(key, None)
            elif isinstance(value, str) and value in {"我", "自己"}:
                config = self.config(actor)
                binding = (config["bindings"] if config else self.bindings).get(actor.user, "")
                if binding:
                    parameters[key] = binding
                else:
                    parameters.pop(key, None)
                    target = Target(target.room)
        for key, hint in item.get("parameterHints", {}).items():
            if (
                hint == "player"
                and key in parameters
                and not str(parameters[key]).startswith("KU_")
            ):
                resolved = await self.queries.resolve(actor, target.room, parameters[key], deadline)
                if (intent.capability == "player.resurrect" and resolved.get("status") == "not_found"
                        and isinstance(parameters[key], str) and parameters[key].startswith("一下")
                        and parameters[key][2:].strip()):
                    # Preserve an actual nickname beginning with 一下; only a
                    # definite miss permits interpreting it as politeness.
                    resolved = await self.queries.resolve(actor, target.room, parameters[key][2:].strip(), deadline)
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
                    self.state.form(actor.key, {"type": "parameter", "intent": intent,
                        "target": Target(target.room), "item": item, "parameters": {k: v for k, v in parameters.items() if k != key},
                        "field": key, "schema": schema["properties"][key], "configRevision": actor.config_revision}, sequence=seq)
                    return Reply("needs_selection", ("玩家信息暂时无法读取。" if resolved.get("status") == "unavailable"
                        else "没有确认的唯一玩家。") + "请回复更完整的游戏昵称或 KU ID，或回复取消。")
                parameters[key] = resolved["items"][0]["id"]
                if len(player_fields) == 1:
                    target = Target(target.room, parameters[key], resolved["items"][0].get("name", ""), source="explicit")
            elif hint == "player" and key in parameters and len(player_fields) == 1:
                target = Target(
                    target.room,
                    parameters[key],
                    target.name if target.player == parameters[key] else "",
                    source=target.source if target.player == parameters[key] else "explicit",
                )
        for key, prop in schema.get("properties", {}).items():
            if (movement or flow) and key in {"phase", "preview_id"}:
                continue
            if (item["id"] == "workbench.act_nearby_entities" and parameters.get("action") != "till" and key == "layout") or (nearby_preview and key in {"phase", "preview_id"}):
                continue
            if (critical or flow) and key == "confirmed":
                continue
            if key == "confirmation" and policy != "none":
                continue
            if key in parameters or key not in schema.get("required", []) and "default" not in prop:
                continue
            if key not in schema.get("required", []) and "default" in prop:
                from .capabilities import validate

                validate(prop["default"], prop)
                parameters[key] = prop["default"]
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
                rows = worlds.get("items", [])
                slot = (item.get("slotRoles") or {}).get(key) or {}
                if (slot.get("ask") == "never" or item["id"] == "workbench.skip_days") and not item["id"].startswith("world."):
                    masters = [r for r in rows if isinstance(r, dict) and r.get("role") == "master" and r.get("id")]
                    chosen = masters[0] if len(masters) == 1 else (
                        rows[0] if len(rows) == 1 and isinstance(rows[0], dict) and rows[0].get("id") else None
                    )
                    if chosen:
                        parameters[key] = chosen["id"]
                        continue
                return self.choose(
                    actor,
                    intent,
                    rows,
                    target.room,
                    seq,
                    "请选择世界",
                    "world",
                    form,
                )
            slot = (item.get("slotRoles") or {}).get(key) or {}
            if "default" in prop and (item["access"] == "read" or slot.get("ask") == "never"):
                parameters[key] = prop["default"]
                continue
            self.state.form(actor.key, form, sequence=seq)
            if item.get("parameterHints", {}).get(key) == "player":
                return Reply(
                    "needs_selection",
                    fmt.safe(item["title"]) + "：请回复玩家昵称或 KU ID。\n"
                    "也可直接说“" + fmt.safe(item["title"]) + " 玩家昵称”；回复 取消 退出。",
                )
            return Reply(
                "needs_selection",
                fmt.safe(item["title"])
                + "：" + parameter_prompt(item, key, prop),
            )
        # The Runtime still freezes the target for a reversible player mode.
        # The bot can submit that preview in this turn when policy needs no reply.
        auto_mode = bool(
            flow and policy == "none" and not critical
            and item["id"] == "workbench.set_player_mode"
            and parameters.get("mode") in schema.get("properties", {}).get("mode", {}).get("enum", [])
            and type(parameters.get("enabled")) is bool and target.player
            and (parameters.get("mode") != "clear_all" or parameters["enabled"] is False)
        )
        needs_confirmation = policy != "none" or (flow and not auto_mode) or item["id"] in LEGACY_MOVEMENT
        if needs_confirmation and not nearby_preview and not movement_preview and item["id"] != "player.teleport":
            if not frozen or frozen.get("userConfirmed") is not True:
                form_confirm = "typed" if policy == "typed" else "once"
                self.state.form(
                    actor.key,
                    {
                        "type": "confirmation",
                        "confirm": form_confirm,
                        "phrase": typed_phrase(item) if form_confirm == "typed" else "确认",
                        "roomName": room_name,
                        "intent": intent,
                        "target": target,
                        "item": item,
                        "parameters": parameters,
                        "configRevision": actor.config_revision,
                    },
                    sequence=seq,
                )
                lines = ["⚠️ " + fmt.safe(item["title"]) + " · " + title,
                         parameter_description(item, parameters, target),
                         confirmation_prompt(item, parameters, form_confirm)]
                return Reply("needs_selection", "\n".join(line for line in lines if line))
            if critical or flow:
                parameters["confirmed"] = True
            if "confirmation" in schema.get("properties", {}) and "confirmation" not in parameters and room_name:
                # Room-name confirmation is the panel's proof of a deliberate
                # change; the user already confirmed this exact room above.
                parameters["confirmation"] = room_name
        if item["access"] == "write":
            if not self.actions:
                raise QueryError("CAPABILITY_MISSING")
            preview_started = self.state.clock()
            journal_step = "mode-preview" if auto_mode else ""
            text = await self.actions.submit_capability(actor, item, target, parameters, deadline, journal_step=journal_step)
            evidence = await self.actions.submitted_result(actor, journal_step=journal_step)
            move_result = evidence.get("data", {}).get("movement") if isinstance(evidence, dict) else None
            if movement_preview and isinstance(move_result, dict) and move_result.get("state") == "preview" and evidence.get("complete") is True:
                remaining_seconds = max(0, min(60, move_result.get("expiresAt", 0) - move_result.get("observedAt", 0)) - (self.state.clock()-preview_started))
                if remaining_seconds <= 0:
                    return Reply("handled_error", "本次传送检查已过期，请重新发起。尚未传送。")
                execution = {**parameters, "phase":"execute", "preview_id":move_result["previewId"]}
                self.state.form(actor.key, {"type":"confirmation", "intent":intent, "target":target, "item":item,
                    "parameters":execution, "configRevision":actor.config_revision, "previewUntil":self.state.clock()+remaining_seconds}, sequence=seq)
                from .teleport import describe_movement
                return Reply("needs_selection", describe_movement(move_result)+"\n请在 60 秒内回复 确认 或 取消。", target, data=evidence)
            if item["id"] == "player.teleport" and isinstance(evidence, dict):
                from .teleport import describe, remaining
                preview = evidence.get("data", {})
                if preview.get("state") == "prepared" and remaining(preview)>0:
                    self.state.form(actor.key, {"type":"confirmation", "intent":intent, "target":target, "item":item,
                        "parameters":parameters, "configRevision":actor.config_revision, "teleportPreview":preview,
                        "previewUntil":self.state.clock()+remaining(preview)}, sequence=seq)
                    return Reply("needs_selection", describe(preview)+"\n请在 60 秒内回复 确认 或 取消。附近仍可能有危险。", target, data=evidence)
                return Reply("handled_error", describe(preview), target, data=evidence)
            batch = evidence.get("data", {}).get("batch") if isinstance(evidence, dict) else None
            if auto_mode and not (
                isinstance(batch, dict) and batch.get("state") == "preview"
                and isinstance(batch.get("previewId"), str)
                and evidence.get("status") == "succeeded" and evidence.get("complete") is True
            ):
                return Reply("handled_error", "预览结果尚未确认，玩家模式尚未修改。请查询任务进度后再决定是否重试。",
                             target, data=evidence)
            if (nearby_preview and isinstance(batch, dict) and batch.get("state") == "preview"
                    and isinstance(batch.get("previewId"), str) and evidence.get("complete") is True):
                if flow and batch.get("version",1)!=flow["resultVersion"]:
                    raise QueryError("CLIENT_UPDATE_REQUIRED")
                seconds=preview_seconds(batch,self.state.clock()-preview_started)
                if seconds<=0:
                    return Reply("handled_error","预览已过期，请重新检查。尚未执行。")
                if batch.get("matched")==0:
                    return Reply("matched",text,target,data=evidence)
                execution = {**parameters, "phase": "execute", "preview_id": batch["previewId"]}
                if auto_mode:
                    if batch.get("matched") != 1 or batch.get("detailsComplete") is not True:
                        return Reply("handled_error", "目标范围未能确认，尚未修改游戏。", target, data=evidence)
                    execution["confirmed"] = True
                    text = await self.actions.submit_capability(actor, item, target, execution, deadline, journal_step="mode-execute")
                    evidence = await self.actions.submitted_result(actor, journal_step="mode-execute")
                    if not isinstance(evidence, dict):
                        self.state.suspend(actor.key, seq)
                        return Reply("handled_error", "模式操作已提交，但结果尚未确认。请查询任务进度；不要重复提交。",
                                     target)
                else:
                    self.state.form(actor.key, {"type": "confirmation", "intent": intent, "target": target,
                        "item": item, "parameters": execution, "configRevision": actor.config_revision, "previewUntil": preview_started + preview_seconds(batch, 0)}, sequence=seq)
                    return Reply("needs_selection", text + "\n请在 30 秒内直接回复 确认，或回复 取消。尚未修改游戏。", target, data=evidence)
            line = target_line(item, target)
            text = line + "\n" + text if line else text
            result = self.decorate(actor, "action", title, text, room=title, player=target.name)
        else:
            data = await self.directory.execute(actor, item, target.room, parameters, deadline)
            evidence = data
            body, footer = business(data)
            config = self.config(actor)
            template = (
                config["templates"].get(item["id"], "{title}\n{body}")
                if config
                else "{title}\n{body}"
            )
            result = (
                self.decorate(
                    actor,
                    "query",
                    title + " · " + fmt.safe(item["title"]),
                    body,
                    room=title,
                    legacy=template,
                )
                + "\n"
                + footer
            )
        if item["access"] == "write" and isinstance(evidence, dict) and (
            evidence.get("status") != "succeeded" or evidence.get("complete") is not True
        ):
            self.state.suspend(actor.key, seq)
        else:
            self.state.remember(actor.key, seq, target, result)
        return Reply("matched", result, target, data=evidence)


TARGET_SOURCES = {
    "quote": "引用的消息",
    "focus": "沿用你刚才操作的玩家",
    "only_online": "当前唯一在线玩家",
}


def target_line(item, target):
    """Every player write names who it acted on, and why that player."""
    hints = item.get("parameterHints", {}) or {}
    if "player" not in hints.values() or not target.player:
        return ""
    name = target.name or target.player
    source = TARGET_SOURCES.get(target.source)
    return "对象：" + fmt.safe(name) + ("（" + source + "）" if source else "")


def json_values(values):
    return " / ".join(fmt.safe(v) for v in values[:12])
