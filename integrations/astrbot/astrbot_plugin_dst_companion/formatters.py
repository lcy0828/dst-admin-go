import math

from .display import local_time, text, time_text
from .item_names import item_name

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
    return {"Master": "地面", "Caves": "洞穴"}.get(value, safe(value))


def safe(value):
    # Plain text goes through structured message components, never CQ parsing.
    return text(value) or "未命名"


def presence(player):
    if player.get("presenceStatus") != "live":
        return "状态未同步"
    if not player.get("online"):
        return "离线"
    if not player.get("worldConfirmed"):
        return "在线 · 所在世界未确认"
    return "在线 · " + world_name(player.get("worldName"))


def player_line(p, *, compact=False):
    prefab = str(p.get("prefab") or "").strip()
    location = (
        world_name(p.get("worldName"))
        if compact
        and p.get("worldConfirmed")
        and p.get("online")
        and p.get("presenceStatus") == "live"
        else presence(p)
    )
    return f"{safe(p.get('name'))} · {CHARACTERS.get(prefab.lower(), safe(prefab or '角色未知'))} · {location}"


def players(data, maximum=None, *, limit=10):
    rows = data.get("items", [])
    online = [p for p in rows if p.get("online") and p.get("presenceStatus") == "live"]
    lines = ["· " + player_line(p, compact=True) for p in online[:limit]]
    header = f"在线 {data['online']} 人" if type(data.get("online")) is int else "在线人数未确认"
    if type(maximum) is int and maximum > 0 and type(data.get("online")) is int:
        header = f"在线 {data['online']} / {maximum} 人"
    if data.get("staleOnline", 0):
        header += f"；另有 {data['staleOnline']} 人状态待同步"
    if not online:
        lines = ["当前页无已确认在线玩家。"]
    if len(online) > limit:
        lines.append(f"另有 {len(online) - limit} 位在线玩家未展开；可查询在线玩家列表。")
    if data.get("total", 0) > len(rows):
        lines.append("名单已分页，仅展示当前页。")
    return "\n".join(["👥 " + header, *lines])


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
    coverage = {"complete": "支持范围内完整", "partial": "部分内容", "unavailable": "暂不可用"}.get(
        data.get("status"), "覆盖情况未确认"
    )
    lines = [f"🎒 背包 · {source} · {coverage}"]

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
        text = safe(item_name(value))
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
                if key == "perishable" and metric.get("unit") == "seconds" and raw >= 0:
                    seconds = round(raw)
                    days, rest = divmod(seconds, 86400)
                    hours, rest = divmod(rest, 3600)
                    minutes, seconds = divmod(rest, 60)
                    duration = "".join(
                        f"{amount}{unit}"
                        for amount, unit in (
                            (days, "天"),
                            (hours, "小时"),
                            (minutes, "分"),
                            (seconds, "秒"),
                        )
                        if amount
                    )
                    duration = duration or "0秒"
                    details.append("剩余保鲜 " + duration)
                else:
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
            lines.append("\n" + ("🛡️" if key == "equipment" else "📦") + "【" + label + "】")
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
        lines.append(
            ("存档时间：" if saved else "采样时间：") + time_text(stamp, year=True) + "（北京时间）"
        )
    elif saved:
        lines.append("存档时间未确认；本次读取时间不代表存档内容的新鲜度。")
    if data.get("status") != "complete":
        lines.append("部分内容未能读取，缺失处不代表没有物品。")
    return "\n".join(lines)


def status(worlds, states, *, include_sample=True):
    by_id = {s.get("worldId"): s for s in states.get("items", [])}
    lines = []
    for world in worlds.get("items", [])[:12]:
        snapshot = by_id.get(world.get("id"), {})
        state = snapshot.get("runtimeState") or world.get("status", "unknown")
        fresh = snapshot.get("freshness") == "live" and not snapshot.get("stale")
        paused = state == "running" and (
            snapshot.get("paused") is True or snapshot.get("freshness") == "paused"
        )
        label, icon = {
            "running": ("运行中", "🟢" if fresh else "🟡"),
            "stopped": ("已停止", "⚫"),
            "starting": ("启动中", "🟡"),
            "stopping": ("停止中", "🟡"),
            "failed": ("运行异常", "🔴"),
            "crashed": ("异常退出", "🔴"),
        }.get(state, ("状态未确认", "⚪"))
        if paused:
            label, icon = "已暂停", "⏸️"
        line = f"{icon} {world_name(world.get('name'))} · {label}"
        details = []
        if (fresh or paused and not snapshot.get("stale")) and state == "running":
            if type(snapshot.get("cycles")) is int:
                details.append(f"第 {snapshot['cycles'] + 1} 天")
            if snapshot.get("season"):
                details.append(
                    {
                        "autumn": "🍂 秋",
                        "winter": "❄️ 冬",
                        "spring": "🌱 春",
                        "summer": "☀️ 夏",
                    }.get(snapshot["season"], safe(snapshot["season"]))
                )
            if type(snapshot.get("remainingDaysInSeason")) is int:
                details.append(f"剩余 {snapshot['remainingDaysInSeason']} 天")
        elif state == "running":
            line += " · 采集待同步"
            if snapshot.get("observedAt"):
                details.append("最近采样 " + time_text(snapshot["observedAt"]))
        if details:
            line += "\n  " + " · ".join(details)
        lines.append(line)
    if len(worlds.get("items", [])) > 12:
        lines.append("仅展示前 12 个世界；其余世界状态未展开。")
    result = "🌍 世界状态\n" + "\n".join(lines or ["暂无世界记录。"])
    if include_sample:
        result += "\n" + sample_time(worlds, states)
    return result


def sample_time(worlds, states):
    ids = {world.get("id") for world in worlds.get("items", [])[:12]}
    stamps = [
        local_time(s.get("observedAt")) for s in states.get("items", []) if s.get("worldId") in ids
    ]
    stamps = [stamp for stamp in stamps if stamp]
    if not stamps:
        return "🕒 世界采样时间未确认"
    return "🕒 最近世界采样：" + max(stamps).strftime("%m/%d %H:%M:%S") + "（北京时间）"


def mods(data, *, limit=12):
    rows = data.get("items")
    if not isinstance(rows, list):
        return "📦 模组 · 列表未返回，数量未知"
    total = data.get("total")
    confirmed = type(total) is int and total >= len(rows)
    lines = [
        f"📦 模组 · 已配置 {total} 个"
        if confirmed
        else f"📦 模组 · 当前列表 {len(rows)} 个，总数未确认"
    ]
    if not confirmed and not rows:
        lines = ["📦 模组 · 数量未确认"]
    if (
        rows
        and confirmed
        and total == len(rows)
        and all(type(m.get("enabled")) is bool for m in rows)
    ):
        enabled = sum(m["enabled"] for m in rows)
        lines.append(f"启用 {enabled} 个 · 停用 {total - enabled} 个")
    lines.extend("· " + safe(m.get("name") or m.get("id")) for m in rows[:limit])
    if len(rows) > limit or confirmed and total > len(rows):
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
