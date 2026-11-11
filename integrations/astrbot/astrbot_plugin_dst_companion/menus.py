"""Small chat menus over the authorized, dynamically discovered catalog."""

import re

from .display import text


COMMON = frozenset({"rooms", "status", "players", "profile", "inventory"})
ROOT_GROUPS = {
    "activity": "活动与连接",
    "management": "游戏管理",
    "mods": "模组与更新",
    "backups": "备份与任务",
    "diagnostics": "面板与运行诊断",
}
MANAGEMENT_GROUPS = {
    "management.players": "玩家状态与模式",
    "management.items": "物品与装备",
    "management.entities": "生物与附近环境",
    "management.locations": "地图与传送",
    "management.world": "世界与时间",
    "management.room": "房间运行与存档",
}
GROUPS = {**ROOT_GROUPS, **MANAGEMENT_GROUPS}


def request(value):
    """Local navigation/search only; never interpret a search as an action."""
    value = value.strip()
    if value in {"返回主菜单", "返回菜单", "主菜单", "全部功能"}:
        return "", ""
    if value == "返回游戏管理":
        return "management", ""
    for key, title in GROUPS.items():
        if value in {title, title + "菜单"}:
            return key, ""
    match = re.fullmatch(r"(?:查找功能|搜索功能|找功能)\s+(.{1,64})", value)
    return ("", match[1].strip()) if match else None


def group(row):
    kind, ident = row["kind"], row["id"].casefold()
    category = row.get("category", "")
    if kind in {"network", "recent", "activity", "trajectory", "combat"} or category in {"network", "profile", "inventory", "players"}:
        return "activity"
    if kind in {"backups", "backup", "job"} or category == "backups" or "backup" in ident:
        return "backups"
    if kind == "mods" or category == "mods" or any(term in ident for term in ("update", "release", "install")):
        return "mods"
    if kind in {"start", "stop", "restart", "kick"} or row.get("access") == "write":
        return "management"
    return "diagnostics"


def home(rows):
    # Keep established common-query numbers; additions go into their category.
    result = [r for r in rows if r["kind"] in COMMON]
    for key, title in ROOT_GROUPS.items():
        members = [r for r in rows if r["kind"] not in COMMON and group(r) == key]
        if not members:
            continue
        preview = " / ".join(text(r["name"], 16) for r in members[:3])
        result.append({"id": "menu:" + key, "name": title, "kind": "help",
                       "capability": "menu:" + key,
                       "menuDescription": preview + f"（{len(members)} 项）"})
    return result


def management_group(row):
    """Use catalog targets/parameters and domain names, not per-operation maps."""
    ident = row["id"].casefold()
    names = set(row.get("parameterNames", []))
    if row["kind"] in {"start", "stop", "restart"} or ident.startswith("room."):
        return "management.room"
    if any(term in ident for term in ("teleport", "relocate", "location", "migrate")):
        return "management.locations"
    if any(term in ident for term in ("entity", "entities", "nearby", "creature")) or "radius" in names:
        return "management.entities"
    if any(term in ident for term in ("give", "inventory", "equipment")) or names & {"prefab", "recipe", "item"}:
        return "management.items"
    if row.get("playerTarget") or "player" in ident or row["kind"] == "kick":
        return "management.players"
    if {"x", "z"} <= names:
        return "management.locations"
    return "management.world"


def management_home(rows):
    result = []
    for key, title in MANAGEMENT_GROUPS.items():
        members = [r for r in rows if management_group(r) == key]
        if members:
            preview = " / ".join(text(r["name"], 16) for r in members[:3])
            result.append({"id": "menu:" + key, "name": title, "kind": "help",
                           "capability": "menu:" + key,
                           "menuDescription": preview + f"（{len(members)} 项）"})
    return result


def matching(rows, query):
    terms = query.casefold().split()
    return [r for r in rows if all(t in " ".join(
        [r["name"], r["id"], *r.get("searchTerms", [])]
    ).casefold() for t in terms)]


def render(pending):
    size, page = pending["pageSize"], pending["page"]
    pages = max(1, (len(pending["candidates"]) + size - 1) // size)
    title = pending.get("title", "可用查询与管理")
    lines = ["🏕️ " + title + (f"（{page + 1}/{pages} 页）" if pages > 1 else ""), "━━━━━━━━━━━━"]
    for index, row in enumerate(pending["candidates"][page * size:(page + 1) * size], 1):
        lines.append(f"{index}. " + text(row["name"], 80))
        if row.get("menuDescription"):
            lines.append("   " + row["menuDescription"])
    if pages > 1:
        lines.append("上一页 / 下一页")
    if pending.get("menuHome"):
        lines.append("直接说：有哪些服、谁在线、查背包 昵称。")
    else:
        lines.append("回复 游戏管理 返回上级；返回菜单 回到首页。" if pending.get("menuParent") else "回复 返回菜单 回到首页。")
    lines.extend(["找功能：搜索功能 关键词（如 地图、复活、更新）。",
                  "2 分钟内直接回复序号；取消退出。"])
    lines.append(pending.get("menuGuidance", ""))
    if pending.get("menuNote"):
        lines.append(pending["menuNote"])
    return "\n".join(line for line in lines if line)
