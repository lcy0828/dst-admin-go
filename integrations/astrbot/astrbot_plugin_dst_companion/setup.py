"""Administrator setup in the current session; no model or copied room IDs."""

import copy

from .broadcast import READ_TOOLS
from .configuration import fingerprint
from .formatters import safe
from .gateway import QueryError
from .sessions import group_origin


class Setup:
    def __init__(self, plugin):
        self.plugin = plugin

    async def handle(self, event):
        p = self.plugin
        parts = p.input_unscoped(event)
        from .group_setup import permission_request

        if not parts and p.config.get("enabled", False) and p.allowed(str(event.unified_msg_origin)):
            try:
                addressed = p.input(event)
            except (QueryError, ValueError, TypeError, KeyError):
                addressed = None
            if addressed and addressed[1] and not addressed[3]:
                request = permission_request(addressed[0])
                if request:
                    parts = "权限 " + request
        if not parts:
            return False
        request = permission_request(parts)
        if request:
            parts = "权限 " + request
        tokens = parts.split(maxsplit=1)
        command = tokens[0]
        argument = tokens[1].strip() if len(tokens) > 1 else ""
        command = {"启动本群": "启用本群"}.get(command, command)
        if command not in {
            "帮助",
            "模板",
            "启用本群",
            "禁用本群",
            "设置",
            "使用连接",
            "订阅房间",
            "播报",
            "聊天播报",
            "权限",
            "权限模式",
            "检查插件更新", "安装插件更新", "取消插件下载", "插件更新状态", "恢复上一版插件",
        }:
            return False
        event.stop_event()  # Claim before any asynchronous read or persistence.
        from astrbot.api.event import MessageChain

        async def reply(text):
            try:
                await event.send(MessageChain().message(text))
            except Exception:
                p.logger.warning("设置回复投递未确认，未重发；可用 /dst 设置 核对保存结果。")

        if command in {"检查插件更新", "安装插件更新", "取消插件下载", "插件更新状态", "恢复上一版插件"}:
            await p.updates.handle(event, command, reply)
            return True
        if command == "帮助":
            await reply(
                "DST 助手设置（由插件直接处理，不调用模型）\n"
                "管理员在本群发送：\n/dst 启用本群\n"
                "/dst 启用本群 聊天播报 开（也支持“启动本群”）\n"
                "/dst 设置 · /dst 模板 · /dst 聊天播报 开/关 · /dst 禁用本群\n"
                "启用后可 @机器人 发送 菜单、有哪些服、1服信息、查背包 昵称。\n"
                "其他问题直接 @机器人说即可，助手会自动理解并选择可用功能。\n"
                "只有插件确认保存后才算启用；模型回复不能作为设置生效依据。"
            )
            return True
        from .group_permissions import enabled as group_permissions_enabled
        from .group_setup import handle as group_setup

        if command in {"权限", "权限模式"}:
            try:
                return await group_setup(p, event, command, argument, reply)
            except (QueryError, ValueError, OSError, TimeoutError):
                await reply("权限设置未完成，请检查群身份、全局权限和配置保存状态。")
                return True
        group_mode = group_permissions_enabled(p.config)
        if group_mode and not event.is_admin():
            await p.ensure_enrollment()
            session = group_origin(event.unified_msg_origin)
            old = p.enrollment.records.get(session, {})
            try:
                role = await p.group_roles.refresh(event, force=True)
            except QueryError:
                await reply("暂时无法确认 QQ 群身份，请稍后重试。")
                return True
            if (old.get("source") != p.connections.source() or not old.get("rooms")
                    or not p.session_native_allowed(session)):
                await reply("请机器人维护者先为本群分配房间并启用；之后群主可自行管理本群。")
                return True
            if (role != "owner" and command != "设置"
                    or command == "使用连接"
                    or command == "模板" and argument.split()[:1] == ["全局"]):
                await reply("此设置由群主或机器人维护者管理；面板连接和全局模板仅维护者可修改。")
                return True
        elif not event.is_admin():
            await reply("此设置仅允许 AstrBot 管理员操作。")
            return True
        if command == "模板":
            await p.template_editor.handle(event, argument, reply)
            return True
        enable_flags = {}
        if command == "启用本群" and argument:
            values = argument.split()
            if (
                len(values) != 2
                or values[0] not in {"播报", "聊天播报"}
                or values[1] not in {"开", "关"}
            ):
                await reply("使用 /dst 启用本群，或 /dst 启用本群 聊天播报 开/关。设置未更改。")
                return True
            enabled = values[1] == "开"
            enable_flags = (
                {"chat": enabled}
                if values[0] == "聊天播报"
                else {
                    "presence": enabled,
                    "server": enabled,
                    **({"chat": False} if not enabled else {}),
                }
            )
        if command in {"设置", "禁用本群"} and argument:
            await reply("使用 /dst " + command + "，无需附加参数。设置未更改。")
            return True
        session = group_origin(event.unified_msg_origin)
        if (
            event.is_private_chat()
            or len(session.split(":")) != 3
            or ":GroupMessage:" not in session
        ):
            if command != "使用连接":
                await reply("请在要启用的群中发送 /dst 启用本群，插件会自动识别群和房间。")
                return True
        try:
            if command == "使用连接":
                if argument not in p.connections.names():
                    await reply(
                        "请选择已连接的 DST MCP："
                        + "、".join(safe(v) for v in p.connections.names())
                    )
                    return True
                if p.config_editor.save is None:
                    raise QueryError("CONFIG_SAVE_UNAVAILABLE")
                candidate = copy.deepcopy(dict(p.config))
                candidate["mcp_server"] = argument
                await p.config_editor.apply(
                    candidate, fingerprint(dict(p.config)), candidate.get("allowed_sessions", [])
                )
                p.catalog.until = 0
                await p.notifications.reconcile()
                await reply(
                    "已选择 MCP 连接；在目标群发送 /dst 启用本群 即可接入。原群不会自动绑定到新面板。"
                )
                return True
            await p.ensure_enrollment()
            old = p.enrollment.records.get(session, {})
            if command == "禁用本群":
                await p.enrollment.update(session, {**old, "enabled": False})
                await p.notifications.reconcile()
                await reply("已关闭本群的查询与播报。")
                return True
            if not p.config.get("enabled", True):
                await reply("插件总开关已关闭，请先在插件设置中启用。")
                return True
            if not p.session_native_allowed(session):
                await reply(
                    "本群不在 AstrBot 会话白名单中。管理员豁免只允许你发消息，"
                    "不会开放全群查询或主动播报。请在本机器人使用的配置 → 访问控制 → "
                    "白名单中加入当前群，再重试。设置未更改。"
                )
                return True
            await p.catalog.refresh()
            from .migration import migrate_broadcast

            await migrate_broadcast(p)
            old = p.enrollment.records.get(session, {})
            source = p.connections.source()
            if command != "启用本群" and (not old.get("enabled") or old.get("source") != source):
                await reply("本群尚未接入当前面板，请先发送 /dst 启用本群。")
                return True
            ceiling = p.settings.effective(session)["rooms"]
            rooms = [r for r in p.catalog.rows if not ceiling or r["id"] in ceiling]
            assigned = old.get("assignedRooms", old.get("rooms", []))
            if group_mode and not event.is_admin() and assigned != "all":
                rooms = [r for r in rooms if r["id"] in assigned]
            if not rooms:
                await reply("面板未返回可开放房间，请检查密钥的房间权限。")
                return True
            record = copy.deepcopy(old) if old.get("source") == source else {}
            if group_mode and record and "assignedRooms" not in record:
                record["assignedRooms"] = record.get("rooms", [])
            if command == "启用本群":
                record = {
                    **record,
                    "enabled": True,
                    "query": True,
                    "source": source,
                    "rooms": record.get("rooms", list(ceiling) or "all"),
                    "notices": record.get(
                        "notices", {"presence": True, "server": True, "chat": False}
                    ),
                }
                record["notices"].update(enable_flags)
                if group_mode and event.is_admin():
                    record["assignedRooms"] = list(ceiling) or "all"
            elif command == "订阅房间":
                if argument == "全部":
                    record["rooms"] = ([r["id"] for r in rooms] if group_mode and not event.is_admin()
                                       else list(ceiling) or "all")
                else:
                    matches = [r for r in rooms if r["name"] == argument]
                    if len(matches) != 1:
                        await reply(
                            "请使用唯一的房间名称："
                            + "、".join(safe(r["name"]) for r in rooms)
                            + "；或 /dst 订阅房间 全部。"
                        )
                        return True
                    record["rooms"] = [matches[0]["id"]]
            elif command in {"播报", "聊天播报"}:
                if argument not in {"开", "关"}:
                    await reply("使用 /dst " + command + " 开 或 关。")
                    return True
                flags = record.setdefault(
                    "notices", {"presence": True, "server": True, "chat": False}
                )
                if command == "聊天播报":
                    flags["chat"] = argument == "开"
                else:
                    flags.update(presence=argument == "开", server=argument == "开")
                    if argument == "关":
                        flags["chat"] = False
            if command != "设置":
                await p.enrollment.update(session, record)
                p.context.dst_companion_v1 = p
                await p.notifications.reconcile()
            effective = p.effective(session)
            caller = p.actor(event)
            management = (
                "\n你的游戏管理：已授权 · 固定写能力 " + str(len(caller.write_allowlist)) + " 项"
                if caller and caller.role in {"game-manage", "panel-manage"}
                else ("\n你的游戏管理：按本群身份与操作权限开放，可用 /dst 权限 查看。" if group_mode
                      else "\n你的游戏管理：未授权（需管理连接、管理员映射与固定操作授权同时就绪）")
            )
            names = [r["name"] for r in rooms if r["id"] in effective["rooms"]]
            notice = (
                p.notifications.delivery_errors.get(
                    session, p.notifications.session_errors.get(session, p.notifications.status)
                )
                if p.connections.broadcast(READ_TOOLS)
                else "暂不可用：当前同源 MCP 未提供播报读取权限，查询可正常使用"
            )
            await reply(
                ("已启用本群。" if command == "启用本群" else "本群设置：")
                + "\n房间："
                + "、".join(safe(n) for n in names)
                + ("（自动同步面板授权）" if record.get("rooms") == "all" else "")
                + "\n播报："
                + (notice if any(record.get("notices", {}).values()) else "关闭")
                + "\n聊天播报开关："
                + ("开" if record.get("notices", {}).get("chat") else "关")
                + management
                + (
                    f"\n聊天仅播报启用后的新公屏消息，约 {p.config.get('advanced', {}).get('chatPollInterval', 3)} 秒检查一次；连续消息会合并，不补发历史。"
                    if record.get("notices", {}).get("chat")
                    else ""
                )
                + "\n已开放常用只读查询（含玩家和背包）；管理仍需单独授权。\n可用：/dst 订阅房间 名称 · /dst 播报 开/关 · /dst 聊天播报 开/关 · /dst 禁用本群"
            )
        except QueryError as error:
            if error.code == "MCP_SELECTION_REQUIRED":
                await reply(
                    "发现多个 DST MCP，请先选择：/dst 使用连接 连接名\n可选："
                    + "、".join(safe(n) for n in p.connections.names())
                )
            elif error.code == "PANEL_DISCONNECTED":
                await reply(
                    "未发现已连接的 DST MCP。请先在 AstrBot 添加面板 /mcp?view=bot 及访问密钥；已有连接请等待其就绪后重试。"
                )
            else:
                await reply("设置未完成：" + error.code + "。未开放新权限，请检查连接后重试。")
        except Exception:
            await reply("设置保存或读取未确认，本次未确认启用；请用 /dst 设置 核对。")
        return True
