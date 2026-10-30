import math
import re

CHARACTERS = dict(
    zip(
        (
            "wilson",
            "willow",
            "wolfgang",
            "wendy",
            "wx78",
            "wickerbottom",
            "woodie",
            "wes",
            "waxwell",
            "wathgrithr",
            "webber",
            "winona",
            "warly",
            "wortox",
            "wormwood",
            "wurt",
            "walter",
            "wanda",
        ),
        (
            "威尔逊",
            "薇洛",
            "沃尔夫冈",
            "温蒂",
            "WX-78",
            "薇克巴顿",
            "伍迪",
            "韦斯",
            "麦斯威尔",
            "薇格弗德",
            "韦伯",
            "薇诺娜",
            "沃利",
            "沃拓克斯",
            "沃姆伍德",
            "沃特",
            "沃尔特",
            "旺达",
        ),
        strict=True,
    )
)


def world_name(value):
    return {"Master": "地面（Master）", "Caves": "洞穴（Caves）"}.get(value, safe(value))


def safe(value):
    # Plain text goes through structured message components, never CQ parsing.
    return re.sub(r"[\x00-\x1f\x7f]", " ", str(value or "未命名")).replace("[CQ:", "［CQ:")[:100]


def presence(player):
    if player.get("presenceStatus") != "live":
        return "状态未同步"
    if not player.get("online"):
        return "离线"
    if not player.get("worldConfirmed"):
        return "在线 · 所在世界未确认"
    return "在线 · " + world_name(player.get("worldName"))


def player_line(p):
    prefab = str(p.get("prefab") or "").strip()
    return f"{safe(p.get('name'))} · {CHARACTERS.get(prefab.lower(), safe(prefab or '角色未知'))} · {presence(p)}"


def players(data):
    rows = data.get("items", [])
    online = [p for p in rows if p.get("online") and p.get("presenceStatus") == "live"]
    lines = [player_line(p) for p in online[:10]]
    header = f"在线 {data['online']} 人" if type(data.get("online")) is int else "在线人数未确认"
    if data.get("staleOnline", 0):
        header += f"；另有 {data['staleOnline']} 人状态待同步"
    if not online:
        lines = ["当前页无已确认在线玩家。"]
    if len(online) > 10:
        lines.append("仅展示前 10 位在线玩家；请按昵称查询其他玩家。")
    if data.get("total", 0) > len(rows):
        lines.append("名单已分页，仅展示当前页。")
    return "\n".join([header, *lines])


def network(rows):
    lines = []
    for p in rows[:10]:
        score = p.get("netScore")
        perf = p.get("performance")
        fields = p.get("fields", {})
        score_state = fields.get("netScore", {})
        score_text = str(score) if type(score) is int else "未采集"
        stale = "（历史/未同步）" if score_state.get("status") != "live" else ""
        lines.append(
            f"{safe(p.get('name'))}：netScore {score_text}{stale}；客户端性能档位 {perf if type(perf) is int else '未采集'}"
        )
    return (
        "\n".join(lines or ["没有已确认的玩家样本。"])
        + "\nnetScore 是原生连接档位，不是 Ping；目前未取得真实 RTT（ms）和客户端 FPS。"
    )


def inventory(data):
    source = {"live": "实时读取", "save": "存档快照"}.get(
        data.get("source"), safe(data.get("source"))
    )
    lines = [f"背包 · {source} · {safe(data.get('status') or '覆盖情况未确认')}"]

    if data.get("worldName"):
        lines.append(world_name(data["worldName"]))
    truncated = False
    count = 0

    def number(value):
        return type(value) in (int, float) and math.isfinite(value)

    def walk(value, depth=0, equipment=False):
        nonlocal count, truncated
        if not isinstance(value, dict) or not value.get("prefab"):
            return
        if depth > 5 or count >= 50 or sum(map(len, lines)) > 2200:
            truncated = True
            return
        count += 1
        amount = value.get("count", value.get("stackSize"))
        text = safe(value.get("name") or value["prefab"])
        if number(amount):
            text += f" ×{amount:g}"
        if equipment:
            slot = str(value.get("slot", "")).split("/")[-1]
            text = (
                {"hands": "手持", "body": "身体", "head": "头部"}.get(slot, safe(slot))
                + "："
                + text
            )
        metrics = value.get("metrics") or {}
        details = []
        for key, label in (
            ("uses", "次数"),
            ("armor", "护甲"),
            ("fuel", "燃料"),
            ("perishable", "新鲜"),
        ):
            metric = metrics.get(key) or {}
            ratio, raw = metric.get("ratio"), metric.get("value")
            if number(ratio) and 0 <= ratio <= 1:
                details.append(f"{label} {ratio * 100:.0f}%")
            elif number(raw):
                details.append(
                    f"{label} {raw:g}" + ("秒" if metric.get("unit") == "seconds" else "")
                )
        if details:
            text += " [" + " · ".join(details) + "]"
        lines.append("  " * depth + ("└ " if depth else "· ") + text)
        for child in (value.get("contents") or [])[:256]:
            walk(child, depth + 1)

    for key, label in (("equipment", "装备"), ("inventory", "物品栏")):
        values = data.get(key)
        if isinstance(values, list):
            lines.append("\n【" + label + "】")
            for value in values[:256]:
                walk(value, equipment=key == "equipment")
            if len(values) > 256:
                truncated = True
    if isinstance(data.get("activeItem"), dict):
        lines.append("\n【鼠标手持】")
        walk(data["activeItem"])
    if count == 0:
        lines.append("未返回可展示物品；是否为空以面板返回的覆盖状态为准。")
    if truncated or data.get("limitsReached"):
        lines.append("（物品较多，仅展示部分；完整内容请在面板查看。）")
    saved = data.get("source") == "save"
    stamp = (
        data.get("savedAt") or data.get("fileModifiedAt")
        if saved
        else data.get("capturedAt") or data.get("observedAt") or data.get("snapshotAt")
    )
    if stamp:
        lines.append(("存档时间：" if saved else "采样时间：") + safe(stamp))
    elif saved:
        lines.append("存档时间未确认；本次读取时间不代表存档内容的新鲜度。")
    lines.append("仅列支持读取的内容；模组私有容器与缺失字段不视为空。")
    return "\n".join(lines)


def status(worlds, states):
    by_id = {s.get("worldId"): s for s in states.get("items", [])}
    lines = []
    for world in worlds.get("items", [])[:12]:
        snapshot = by_id.get(world.get("id"), {})
        state = snapshot.get("runtimeState") or world.get("status", "unknown")
        label = {"running": "运行中", "stopped": "已停止", "starting": "启动中"}.get(
            state, "状态未确认"
        )
        line = f"{world_name(world.get('name'))}：{label}"
        fresh = snapshot.get("freshness") == "live" and not snapshot.get("stale")
        if fresh and state == "running":
            if type(snapshot.get("cycles")) is int:
                line += f" · 第 {snapshot['cycles'] + 1} 天"
            if snapshot.get("season"):
                line += " · " + {
                    "autumn": "秋",
                    "winter": "冬",
                    "spring": "春",
                    "summer": "夏",
                }.get(snapshot["season"], safe(snapshot["season"]))
            if type(snapshot.get("remainingDaysInSeason")) is int:
                line += f" · 剩余 {snapshot['remainingDaysInSeason']} 天"
        elif state == "running":
            line += " · 采集待同步"
        if snapshot.get("observedAt"):
            line += "\n  采样：" + safe(snapshot["observedAt"])
        lines.append(line)
    return "\n".join(lines or ["暂无世界记录。"])


def mods(data):
    rows = data.get("items")
    if not isinstance(rows, list):
        return "模组列表未返回，数量未知。"
    total = data.get("total") if type(data.get("total")) is int else len(rows)
    lines = [f"模组 {total} 个"]
    lines.extend("· " + safe(m.get("name") or m.get("id")) for m in rows[:12])
    if len(rows) > 12 or total > len(rows):
        lines.append("仅展示部分模组，请到面板查看完整列表。")
    return "\n".join(lines)


def backups(data):
    if (
        not isinstance(data.get("items"), list)
        or not isinstance(data.get("groups"), list)
        or not isinstance(data.get("legacyPolicy"), dict)
    ):
        return "自动备份配置不完整，不能确认是否启用。"
    groups = {g.get("id"): g for g in data["groups"]}
    active = []
    unknown = False
    for task in data["items"]:
        group = groups.get(task.get("groupId"))
        if task.get("enabled") is not True:
            continue
        if task.get("groupId") and group is None:
            unknown = True
            continue
        if group is None or group.get("enabled") is True:
            active.append(task)
    enabled = bool(active) or data["legacyPolicy"].get("enabled") is True
    if not enabled and unknown:
        return "存在启用的备份任务，但任务组状态缺失，尚不能确认生效。"
    lines = ["自动备份：" + ("已开启" if enabled else "未开启")]
    for task in active[:5]:
        lines.append(
            safe(task.get("name"))
            + " · "
            + safe(task.get("schedule"))
            + " · "
            + safe(task.get("timezone"))
            + "\n下次："
            + safe(task.get("nextRunAt"))
        )
    if data["legacyPolicy"].get("enabled") is True:
        lines.append("原有自动快照策略已开启。")
    lines.append("已配置计划不等于最近备份已成功。")
    return "\n".join(lines)
