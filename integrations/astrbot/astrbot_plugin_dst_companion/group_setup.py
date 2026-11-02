"""Group permission settings, authorized using QQ identity, never the model."""

import copy
import re

from .configuration import fingerprint
from .group_permissions import enabled, grants, validate_rules
from .sessions import group_origin

ROLE_NAMES = {"成员": "member", "普通成员": "member", "管理员": "admin", "群主": "owner"}
ACTION_NAMES = {
    "复活玩家": "player.resurrect",
    "发物品": "player.give_item",
    "踢人": "player.kick",
    "启动房间": "room.start",
    "停止房间": "room.stop",
    "重启房间": "room.restart",
    "立即备份": "room.backup.create",
    "自动备份": "room.backups.configure",
    "保存世界": "room.save",
}


def permission_request(text):
    """Exact direct requests only; discussions/questions continue to the model."""
    text = text.strip().rstrip("。！!")
    role = r"(?:本群)?(普通成员|成员|管理员|群主)"
    match = re.fullmatch(
        r"(?:请|麻烦)?\s*(?:给|为)\s*" + role
        + r"\s*(添加|开放|开启|授予|赋予|开通|取消|关闭|收回)\s*(?:所有|全部)(?:的)?权限(?:吧)?",
        text,
    )
    if match:
        action = "关" if match[2] in {"取消", "关闭", "收回"} else "开"
        return match[1] + " 全部 " + action
    match = re.fullmatch(
        r"(?:请|麻烦)?\s*(?:把)?\s*" + role
        + r"(?:的)?\s*(?:所有|全部)权限\s*(打开|开启|开通|关闭|取消)(?:吧)?",
        text,
    )
    if match:
        return match[1] + " 全部 " + ("关" if match[2] in {"关闭", "取消"} else "开")
    return ""


async def migrate_private_groups(p):
    """An explicit private-connection migration keeps delivery state continuous."""
    await p.ensure_enrollment()
    source = p.connections.source()
    origin = source.split("|", 1)[-1]
    async with p.migration_lock, p.notifications.lock, p.enrollment.lock:
        records = copy.deepcopy(p.enrollment.records)
        changed = []
        for session, record in records.items():
            previous = record.get("source", "")
            if previous != source and previous.split("|", 1)[-1] == origin:
                record["source"] = source
                record.setdefault("assignedRooms", record.get("rooms", []))
                changed.append(session)
        if not changed:
            return
        # Stop the old consumers before touching their durable cursors/outbox.
        await p.notifications.stop_feeds()
        p.notifications.engines.clear()
        p.notifications.ready.clear()
        async with p.notifications.storage_lock:
            saved = copy.deepcopy(p.notifications.saved)
            for session in changed:
                value = saved.get(session)
                if value and value.get("source", "").split("|", 1)[-1] == origin:
                    value["source"] = source
            # Publish delivery state first. An interruption before enrollment
            # publication leaves the group paused and can be safely resumed.
            await p.put_kv_data("broadcast_state_v2", saved)
            p.notifications.saved = saved
        await p.enrollment.save("sessions_v3", records)
        p.enrollment.records = records
        p.catalog.until = 0


async def handle(plugin, event, command, argument, reply):
    if command not in {"权限", "权限模式"}:
        return False
    p = plugin
    if command == "权限模式":
        if not event.is_admin():
            await reply("权限模式由机器人维护者设置；群主无需成为 AstrBot 管理员。")
        elif argument not in {"群角色", "旧版"} or p.config_editor.save is None:
            await reply(
                "使用 /dst 权限模式 群角色，统一使用面板连接并按 QQ 群身份授权；旧版保留原规则。"
            )
        else:
            if argument == "群角色" and p.private_panel.configured:
                await p.private_panel.refresh()
                if not p.private_panel.full_access:
                    await reply("当前密钥未提供完整面板权限，请维护者更换插件专用密钥。设置未更改。")
                    return True
            candidate = copy.deepcopy(dict(p.config))
            candidate["permission_mode"] = "group" if argument == "群角色" else "legacy"
            validate_rules(candidate.get("group_permissions", {}))
            await p.config_editor.apply(
                candidate, fingerprint(dict(p.config)), candidate.get("allowed_sessions", [])
            )
            if argument == "群角色" and p.private_panel.configured:
                await migrate_private_groups(p)
            await reply(
                "已切换为" + argument + "权限。原群和房间范围保留；使用 /dst 权限 查看实际授权。"
            )
        return True
    if not enabled(p.config):
        await reply("当前使用旧版权限。机器人维护者可发送 /dst 权限模式 群角色。")
        return True
    await p.ensure_enrollment()
    session = group_origin(event.unified_msg_origin)
    if not p.allowed(str(event.unified_msg_origin)):
        await reply("本群尚未开放查询，请机器人维护者先分配房间并启用本群。")
        return True
    role = await p.group_roles.refresh(event, force=True)
    if not p.allowed(str(event.unified_msg_origin)):
        await reply("本群授权已变更，请重新查看 /dst 权限。设置未更改。")
        return True
    record = p.enrollment.records.get(session, {})
    if not argument:
        rule, _ = grants(p.config, record, "owner" if role == "maintainer" else role)
        names = {v: k for k, v in ACTION_NAMES.items()}
        await reply(
            "当前身份："
            + {
                "member": "普通成员",
                "admin": "本群管理员",
                "owner": "本群群主",
                "maintainer": "机器人维护者",
            }[role]
            + "\n开放操作："
            + ("、".join(names.get(v, v) for v in rule["write"]) or "查询")
            + "\n工作台："
            + {"off": "仅指定操作", "standard": "常用操作", "all": "含危险操作"}[rule["workbench"]]
            + "\n群主可说：给管理员添加所有权限，或 /dst 权限 管理员 重启房间 开/关。"
        )
        return True
    if role not in {"owner", "maintainer"}:
        await reply("本群权限由群主或机器人维护者设置。")
        return True
    parts = argument.split()
    if len(parts) != 3 or parts[0] not in ROLE_NAMES or parts[2] not in {"开", "关"}:
        await reply("可说：给管理员添加所有权限。也可使用 /dst 权限 管理员 全部 开/关，或将“全部”换成“重启房间”等操作；身份支持成员、管理员、群主。")
        return True
    capability = ACTION_NAMES.get(parts[1], parts[1])
    target = ROLE_NAMES[parts[0]]
    rule, _ = grants(p.config, record, target)
    ceiling, _ = grants(p.config, {}, "owner")
    if parts[1] in {"全部", "所有", "全部权限", "所有权限"}:
        candidate = copy.deepcopy(record)
        candidate.setdefault("rolePermissions", {})[target] = (
            {"read": list(ceiling["read"]), "write": list(ceiling["write"]),
             "workbench": ceiling["workbench"], "deny": []}
            if parts[2] == "开"
            else {"read": [], "write": [], "workbench": "off", "deny": []}
        )
        applied, _ = grants(p.config, candidate, target)
        await p.enrollment.update(session, candidate, expected=fingerprint(record))
        if parts[2] == "开":
            names = {v: k for k, v in ACTION_NAMES.items()}
            await reply(
                "已向本群" + parts[0] + "开放当前可授予的全部权限。\n管理操作："
                + ("、".join(names.get(v, v) for v in applied["write"]) or "无")
                + "\n工作台：" + {"off": "仅指定操作", "standard": "常用操作", "all": "含危险操作"}[applied["workbench"]]
                + "。\n仅对本群开放的房间生效；维护者全局禁用的操作仍不可用，面板连接和插件维护权限不变。"
            )
        else:
            await reply("已关闭本群" + parts[0] + "的查询和管理权限；其他身份和群保持不变。")
        return True
    if capability not in ceiling["write"] and parts[2] == "开":
        await reply("此操作未在全局群权限中开放，请机器人维护者先设置。")
        return True
    writes, deny = set(rule["write"]), set(rule["deny"])
    if parts[2] == "开":
        writes.add(capability)
        deny.discard(capability)
    else:
        writes.discard(capability)
        deny.add(capability)
    candidate = copy.deepcopy(record)
    candidate.setdefault("rolePermissions", {})[target] = {
        **rule,
        "write": sorted(writes),
        "deny": sorted(deny),
    }
    grants(p.config, candidate, target)
    await p.enrollment.update(session, candidate, expected=fingerprint(record))
    await reply(
        "已" + ("允许" if parts[2] == "开" else "关闭") + parts[0] + "使用「" + parts[1] + "」。"
    )
    return True
