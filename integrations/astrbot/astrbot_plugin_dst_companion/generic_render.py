"""Compact fallback for schema-driven replies without a dedicated card."""

LABELS = {"name": "名称", "title": "名称", "status": "状态", "message": "说明", "error": "错误",
          "roomId": "房间", "worldId": "世界", "playerId": "玩家", "count": "数量", "total": "总数",
          "items": "记录", "enabled": "开关", "complete": "是否完整", "progress": "进度",
          "sampledAt": "采样时间", "observedAt": "观测时间", "createdAt": "创建时间",
          "from": "开始时间", "to": "结束时间", "nextCursor": "后续记录游标", "hasMore": "还有后续记录"}
STATES = {"queued": "排队中", "pending": "等待中", "running": "处理中", "succeeded": "执行完成",
          "failed": "失败", "uncertain": "结果未确认", "partial": "部分完成", "sent": "已发送，待确认",
          "accepted": "已提交", "canceled": "已取消", "unavailable": "暂不可用", "timed_out": "超时"}


def game_release_confirmation(arguments, phrase, preview=None):
    body = arguments.get("body") if isinstance(arguments, dict) else None
    body = body if isinstance(body, dict) else {}
    policy = body.get("policy") if isinstance(body.get("policy"), dict) else {}
    lines = ["🎮 确认更新游戏"]
    if isinstance(preview, dict):
        scope = f"最近预检：{preview['roomCount']} 个房间" if type(preview.get("roomCount")) is int else "最近预检：所选安装的关联房间"
        if type(preview.get("worldCount")) is int:
            scope += f" · {preview['worldCount']} 个世界"
        lines.append(scope)
        for room in preview.get("rooms", [])[:8]:
            if isinstance(room, dict) and isinstance(room.get("name"), str):
                lines.append("· " + room["name"])
        if preview.get("targetBuild"):
            lines.append("目标 Build：" + str(preview["targetBuild"]))
    else:
        lines.append("范围：所选安装及其关联房间。")
        if body.get("desiredVersion"):
            lines.append("目标 Build：" + str(body["desiredVersion"]))
    delay = policy.get("stopDelaySeconds")
    lines.append("停服前：" + ("立即停止" if delay == 0 else f"公告并等待 {delay} 秒" if type(delay) is int else "按房间现有通知设置"))
    lines.append("备份：" + ("按你的选择跳过" if policy.get("skipBackup") is True else "更新前自动备份；失败时再由你决定"))
    lines.append("更新后：" + ("按你的选择保持世界停止" if policy.get("restartRunning") is False else "自动恢复原先运行的世界"))
    lines.append("提交时会再次检查版本和影响范围。")
    lines.append("尚未执行。回复“" + phrase + "”开始更新，或回复“取消”。")
    return "\n".join(lines)


def game_release_submission(payload):
    state = payload.get("state")
    result = payload.get("result")
    result = result if isinstance(result, dict) else {}
    data = result.get("data") if isinstance(result.get("data"), dict) else {}
    job_id = data.get("id") if isinstance(data.get("id"), str) else ""
    if state in {"accepted", "queued", "pending", "running", "succeeded"}:
        lines = ["🎮 游戏更新任务已提交", "正在按步骤处理停服、备份、更新与恢复；提交回执不代表更新完成。"]
        if job_id:
            lines.append("任务编号：" + job_id)
        elif payload.get("taskRef"):
            lines.append("查询编号：" + str(payload["taskRef"]))
        return "\n".join(lines)
    if state in {"uncertain", "executing"}:
        return "游戏更新结果尚未确认，请查询原任务，不要重复提交。\n查询编号：" + str(payload.get("taskRef") or "未提供")
    if state == "failed":
        error = result.get("error")
        reason = error.get("message") if isinstance(error, dict) else None
        detail = reason if isinstance(reason, str) and reason else payload.get("summary")
        return "游戏更新未提交：" + (detail[:200] if isinstance(detail, str) and detail else "请查看原任务回执。")
    return None


def game_release_preview(plan):
    """Keep update decisions visible without dumping Runtime capabilities."""
    if not isinstance(plan, dict) or not {"ready", "updateRequired", "installations"} <= plan.keys():
        return None
    installations = plan.get("installations")
    if not isinstance(installations, list):
        return None
    rooms = {}
    targets = []
    known_worlds = True
    running_worlds = 0
    plan_blockers = plan.get("blockers") if isinstance(plan.get("blockers"), list) else []
    blockers = [row.get("message") for row in plan_blockers
                if isinstance(row, dict) and isinstance(row.get("message"), str)]
    for installation in installations:
        if not isinstance(installation, dict):
            continue
        shards = installation.get("shards")
        if not isinstance(shards, list):
            known_worlds = False
            shards = []
        worlds = []
        for shard in shards:
            if not isinstance(shard, dict):
                continue
            room_id = shard.get("roomId")
            room_name = shard.get("roomName") or "房间名称未提供"
            name = shard.get("worldName") or ("地面" if shard.get("isMaster") else "世界名称未提供")
            running = shard.get("wasRunning") if type(shard.get("wasRunning")) is bool else None
            if running is True:
                running_worlds += 1
            world = {"roomName": room_name, "name": name, "wasRunning": running}
            worlds.append(world)
            if isinstance(room_id, str):
                rooms.setdefault(room_id, {"name": room_name, "worlds": []})["worlds"].append(world)
        installation_blockers = installation.get("blockers") if isinstance(installation.get("blockers"), list) else []
        blockers.extend(row.get("message") for row in installation_blockers
                        if isinstance(row, dict) and isinstance(row.get("message"), str))
        targets.append({
            "name": installation.get("targetName") or installation.get("targetId"),
            "targetId": installation.get("targetId"),
            "installationId": installation.get("installationId"),
            "currentBuild": installation.get("steamBuild") or installation.get("currentVersion"),
            "targetBuild": installation.get("desiredVersion"),
            "upToDate": installation.get("upToDate"),
            "worlds": worlds[:16],
            "omittedWorlds": max(0, len(worlds) - 16),
        })
    affected = plan.get("affectedRoomIds")
    room_count = len(affected) if isinstance(affected, list) else None
    world_count = sum(len(row["worlds"]) + row["omittedWorlds"] for row in targets) if known_worlds else None
    policy = plan.get("policy") if isinstance(plan.get("policy"), dict) else {}
    compact = {
        "ready": plan.get("ready"),
        "updateRequired": plan.get("updateRequired"),
        "targetBuild": plan.get("desiredVersion"),
        "roomCount": room_count,
        "worldCount": world_count,
        "runningWorldCount": running_worlds if known_worlds else None,
        "rooms": list(rooms.values())[:16],
        "targets": targets[:8],
        "blockers": blockers[:8],
        "policy": {key: policy[key] for key in ("stopDelaySeconds", "skipBackup", "restartRunning") if key in policy},
        "selectedInstallations": plan.get("selectedInstallations"),
        "omittedTargets": max(0, len(targets) - 8),
        "omittedBlockers": max(0, len(blockers) - 8),
    }
    status = "可以更新" if plan["ready"] and plan["updateRequired"] else (
        "已是目标版本" if plan["ready"] and not plan["updateRequired"] else "暂不能更新"
    )
    lines = ["🎮 游戏更新检查", status]
    if plan.get("desiredVersion"):
        lines.append("目标 Build：" + str(plan["desiredVersion"]))
    if room_count is not None:
        scope = f"影响 {room_count} 个房间"
        if world_count is not None:
            scope += f" · {world_count} 个世界（{running_worlds} 个运行中）"
        lines.append(scope)
    for row in targets[:8]:
        lines.append("· " + str(row["name"] or "运行目标未提供") + "：" +
                     str(row["currentBuild"] or "版本未确认") + " → " +
                     str(row["targetBuild"] or "目标未确认"))
    for room in list(rooms.values())[:8]:
        names = [str(world["name"]) for world in room["worlds"][:8]]
        lines.append("· " + str(room["name"]) + "：" + "、".join(names))
    for message in blockers[:5]:
        lines.append("需处理：" + message[:160])
    if len(targets) > 8 or len(rooms) > 8 or len(blockers) > 5:
        lines.append("还有未展示的目标或提示，请在面板查看完整计划。")
    lines.append("本次仅检查版本与影响范围，尚未停服、备份或更新。")
    return compact, "\n".join(lines)


def facts(value, max_rows=32):
    lines, omitted = [], False
    def visit(item, label="", depth=0):
        nonlocal omitted
        if len(lines) >= max_rows or depth > 4:
            omitted = True
            return
        if isinstance(item, dict):
            for key, child in item.items():
                name = LABELS.get(key, str(key))
                if key == "status" and isinstance(child, str):
                    child = STATES.get(child, child)
                visit(child, (label + " · " if label else "") + name, depth+1)
        elif isinstance(item, list):
            if not item:
                lines.append((label + "：" if label else "") + "本次没有返回记录")
            for i, child in enumerate(item):
                visit(child, (label + " " if label else "记录 ") + str(i+1), depth+1)
        else:
            shown = "未提供" if item is None else "是" if item is True else "否" if item is False else str(item)
            lines.append("· " + (label + "：" if label else "") + shown[:300])
            omitted = omitted or len(shown) > 300
    visit(value)
    text = "\n".join(lines)
    if len(text)>3000:
        text, omitted = text[:3000], True
    if omitted:
        text += "\n内容较多，未完整展示；可缩小范围或分页查询。"
    return text
