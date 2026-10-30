import re


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
    return "在线 · " + safe(player.get("worldName"))


def player_line(p):
    return f"{safe(p.get('name'))} · {safe(p.get('prefab') or '角色未知')} · {presence(p)}"


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

    # Keep the actual inventory structure; never imply unsupported Mod-private
    # containers were inspected or that a partial result is an empty inventory.
    def walk(value, depth=0):
        if depth > 8 or len(lines) >= 16:
            return
        if isinstance(value, list):
            for child in value:
                walk(child, depth + 1)
        elif isinstance(value, dict):
            if value.get("prefab"):
                amount = value.get("stackSize", value.get("count"))
                lines.append(
                    safe(value.get("name") or value["prefab"])
                    + (f" ×{amount}" if type(amount) is int else "")
                )
            for key in (
                "items",
                "slots",
                "equipment",
                "inventory",
                "containers",
                "contents",
                "heldItem",
                "activeItem",
                "overflow",
                "item",
            ):
                if key in value:
                    walk(value[key], depth + 1)

    walk(data)
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
        line = f"{safe(world.get('name'))}：{label}"
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
        elif state == "running":
            line += " · 采集待同步"
        if snapshot.get("observedAt"):
            line += "\n  采样：" + safe(snapshot["observedAt"])
        lines.append(line)
    return "\n".join(lines or ["暂无世界记录。"])


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
