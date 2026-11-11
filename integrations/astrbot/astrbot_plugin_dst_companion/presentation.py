"""Bounded user-facing results; uncertain execution stays explicit."""

import string

from . import formatters as fmt
from .display import time_text

FIELDS = frozenset({"title", "body", "room", "player"})


def render(template, *, title="", body="", room="", player=""):
    values = {"title": title, "body": body, "room": room, "player": player}
    try:
        if not isinstance(template, str) or len(template) > 2000:
            raise ValueError("invalid template")
        for _, field, spec, conversion in string.Formatter().parse(template):
            if field is not None and (field not in FIELDS or spec or conversion):
                raise ValueError("unsupported placeholder")
        # Body holds already formatted evidence, never objects/attributes.
        return template.format_map(values)[:2400]
    except (ValueError, KeyError, IndexError):
        return (title + "\n" + body)[:2400]


def _mib(value):
    if type(value) not in (int, float) or value <= 0:
        return ""
    mib = value / (1024 * 1024)
    return f"{mib / 1024:.2f} GiB" if mib >= 1024 else f"{mib:.0f} MiB"


def _performance_body(data):
    lines = ["📊 性能报表"]
    start, end = data.get("from"), data.get("to")
    if isinstance(start, str) and isinstance(end, str) and start and end:
        lines.append("范围：" + time_text(start) + " — " + time_text(end))
    worlds = [w for w in (data.get("worlds") or []) if isinstance(w, dict)]
    items = [r for r in (data.get("items") or []) if isinstance(r, dict)]
    if not worlds and not items:
        lines.append("该时间段暂无性能采样记录（可能尚未开启采样或世界未运行）。")
        return "\n".join(lines)
    by_world = {}
    for row in items:
        wid = str(row.get("worldId") or "")
        by_world.setdefault(wid, []).append(row)
    if not worlds:
        for wid, rows in by_world.items():
            worlds.append({"worldId": wid, "worldName": rows[-1].get("worldName") or wid})
    for w in worlds[:8]:
        wid = str(w.get("worldId") or "")
        name = fmt.world_name(w.get("worldName") or wid or "世界")
        rows = by_world.get(wid, [])
        parts = []
        avg_score = w.get("averageScore")
        min_score = w.get("minimumScore")
        if type(avg_score) in (int, float):
            score_text = f"模拟评分 平均 {avg_score:.1f}"
            if type(min_score) in (int, float):
                score_text += f"（最低 {min_score:.1f}）"
            parts.append(score_text)
        cpus = [r["cpuPercent"] for r in rows if type(r.get("cpuPercent")) in (int, float)]
        peak_cpu = w.get("peakCPUPercent")
        if type(peak_cpu) not in (int, float) and cpus:
            peak_cpu = max(cpus)
        if cpus and type(peak_cpu) in (int, float):
            parts.append(f"CPU 平均 {sum(cpus) / len(cpus):.1f}% / 峰值 {peak_cpu:.1f}%")
        elif type(peak_cpu) in (int, float):
            parts.append(f"CPU 峰值 {peak_cpu:.1f}%")
        rss_rows = [r["rssBytes"] for r in rows if type(r.get("rssBytes")) in (int, float) and r["rssBytes"] > 0]
        peak_rss = w.get("peakRSSBytes")
        if (type(peak_rss) not in (int, float) or peak_rss <= 0) and rss_rows:
            peak_rss = max(rss_rows)
        if rss_rows and type(peak_rss) in (int, float) and peak_rss > 0:
            parts.append(f"内存 当前 {_mib(rss_rows[-1])} / 峰值 {_mib(peak_rss)}")
        elif type(peak_rss) in (int, float) and peak_rss > 0:
            parts.append(f"内存峰值 {_mib(peak_rss)}")
        cov = w.get("coverage")
        if type(cov) in (int, float):
            parts.append(f"覆盖率 {round(cov * 100)}%")
        lines.append("· " + name + "：" + (" · ".join(parts) if parts else "暂无有效采样（暂停或未运行期间不计分）"))
    return "\n".join(lines)


def _map_body(data):
    lines = ["🗺️ 世界地图"]
    rows = [r for r in (data.get("items") or []) if isinstance(r, dict)]
    if not rows:
        lines.append("当前房间尚未生成地图快照。")
        if data.get("rendererAvailable") is False:
            lines.append("当前节点未安装地图渲染器。")
        else:
            lines.append("可在管理面板「世界地图」中生成最新地图。")
        return "\n".join(lines)
    status_labels = {"succeeded": "已生成", "running": "生成中", "failed": "生成失败"}
    seen = set()
    for row in rows[:8]:
        wid = str(row.get("worldId") or row.get("sessionLabel") or "世界")
        if wid in seen:
            continue
        seen.add(wid)
        name = fmt.world_name(wid)
        status = status_labels.get(row.get("status"), fmt.safe(row.get("status") or "未知"))
        parts = [status]
        w, h = row.get("width"), row.get("height")
        if type(w) is int and type(h) is int and w > 0 and h > 0:
            parts.append(f"{w}×{h}")
        features = row.get("featureCount")
        if type(features) is int and features >= 0:
            parts.append(f"标记 {features} 处")
        stamp = row.get("finishedAt") or row.get("createdAt")
        if isinstance(stamp, str) and stamp:
            parts.append(time_text(stamp))
        lines.append("· " + name + "：" + " · ".join(parts))
    lines.append("高清图层与坐标可在管理面板「世界地图」查看。")
    return "\n".join(lines)


def query_body(result, data):
    """Render known fields only. New capabilities do not dump protocol objects."""
    if data is None:
        return "没有确认的数据，请稍后查看原查询。"
    if not isinstance(data, dict):
        return "查询已返回，请在面板查看详细结果。"
    capability = result.get("capabilityId", "")
    if capability == "room.performance":
        return _performance_body(data)
    if capability == "room.map":
        return _map_body(data)
    if capability == "room.locations":
        rows = data.get("items", [])
        if not rows: return "当前世界还没有保存地点。"
        return "📍 已保存地点\n" + "\n".join("· " + fmt.safe(row.get("name")) + " · X " + fmt.safe(row.get("x")) + " / Z " + fmt.safe(row.get("z")) for row in rows[:30] if isinstance(row, dict)) + ("\n还有其他地点，请缩小查询范围。" if data.get("hasMore") or len(rows)>30 else "")
    if capability == "location.save":
        return "📍 已保存地点：" + fmt.safe(data.get("name")) + "\nX " + fmt.safe(data.get("x")) + " / Z " + fmt.safe(data.get("z"))
    if capability == "player.teleport":
        from .teleport import describe
        return describe(data)
    if capability == "player.inventory":
        return fmt.inventory(data)
    if capability in {"room.players", "player.resolve"}:
        return fmt.players(data)
    if capability == "room.network":
        return fmt.network(data.get("items", []))
    if capability == "room.backups.configure" and type(data.get("effectiveEnabled")) is bool:
        return "自动备份：" + ("已开启" if data["effectiveEnabled"] else "已关闭") + (
            "\n下次备份：" + fmt.safe(data["nextRunLocal"]) if data.get("nextRunLocal") else ""
        )
    if capability == "world.player_modes" and isinstance(data.get("players"), list):
        labels = {"enabled": "已开启", "disabled": "已关闭", "unknown": "无法确认"}
        return "玩家模式 · 当前读取\n" + "\n".join(
            "· " + fmt.safe(row.get("id")) + "：免伤 " + labels.get(row.get("invincible"), "无法确认")
            + " · 制作 " + labels.get(row.get("creative"), "无法确认")
            for row in data["players"][:20] if isinstance(row, dict)) + "\n当前效果可能来自面板或其他模组。"
    if result.get("resultKind") == "query-run":
        if type(data.get("count")) is int and data.get("prefab"):
            return fmt.safe(data["prefab"]) + "：" + str(data["count"]) + " 个"
        if type(data.get("lowerBound")) is int:
            return fmt.safe(data.get("prefab")) + "：至少 " + str(data["lowerBound"]) + " 个（仅完成部分扫描）"
        if isinstance(data.get("players"), list):
            rows = data["players"][:20]
            return "在线玩家" + (" " + str(data["count"]) + " 人" if type(data.get("count")) is int else "") + "\n" + "\n".join("· " + fmt.safe(row.get("name")) for row in rows if isinstance(row, dict))
        labels = {"cycles": "已度过天数", "temperature": "温度（°C）", "season": "季节", "phase": "时段", "israining": "下雨", "issnowing": "下雪"}
        values = {"autumn": "秋", "winter": "冬", "spring": "春", "summer": "夏", "day": "白天", "dusk": "黄昏", "night": "夜晚"}
        lines = []
        for key, label in labels.items():
            value = data.get(key)
            if type(value) in (str, int, float, bool):
                shown = "是" if value is True else "否" if value is False else values.get(value, str(value))
                lines.append(label + "：" + fmt.safe(shown))
        if lines:
            return "\n".join(lines)
    for key in ("message", "text"):
        if isinstance(data.get(key), str) and data[key].strip():
            return fmt.text(data[key], 1400)
    for key in ("answer", "value", "count"):
        if type(data.get(key)) in (int, float, str, bool):
            return "结果：" + fmt.safe(data[key])
    return "已取得结果，请在面板查看详情。"


def business(result, *, diagnostics=False):
    labels = {
        "pending": "已排队",
        "running": "正在执行",
        "succeeded": "已完成",
        "partial": "仅有部分结果",
        "failed": "执行失败",
        "uncertain": "结果尚未确认",
        "unavailable": "数据暂不可用",
        "rejected": "请求被拒绝",
    }
    status = result.get("status")
    if status == "succeeded" and result.get("complete") is not True:
        status = "partial"
    data = result.get("data")
    if isinstance(data, dict) and "data" in data and result.get("resultKind") == "query-run":
        data = data["data"]
    body = query_body(result, data)
    if result.get("resultKind") == "item-grant" and isinstance(data, dict):
        name = fmt.safe(data.get("name") or data.get("prefab"))
        granted, requested = data.get("granted"), data.get("requested")
        if status == "succeeded" and granted is not None and granted == requested:
            body = "已发放 " + name + " ×" + str(granted) + "\n背包满时会掉在玩家脚下。"
        else:
            body = "物品：" + name + " · 请求 " + fmt.safe(requested) + " 个"
            if granted is not None:
                body += "\n本次确认发放：" + str(granted) + " 个"
            body += "\n" + fmt.safe(data.get("message") or "结果尚未确认，请只回读原请求。")
        if data.get("worldName"):
            body += "\n世界：" + fmt.world_name(data["worldName"])
    if result.get("resultKind") == "entity-spawn" and isinstance(data, dict):
        body = (
            "生物："
            + fmt.safe(data.get("name") or data.get("prefab"))
            + " · 请求 "
            + fmt.safe(data.get("requested"))
            + " 只"
        )
        if data.get("spawned") is not None:
            body += "\n本次回执确认生成：" + str(data["spawned"]) + " 只"
        body += "\n" + fmt.safe(data.get("message") or "结果尚未确认，请只回读原请求。")
        if data.get("worldName"):
            body += "\n世界：" + fmt.world_name(data["worldName"])
    if result.get("resultKind") == "workbench-run" and isinstance(data, dict):
        title = "复活指令" if result.get("capabilityId") == "player.resurrect" else "操作指令"
        if status == "succeeded":
            body = title + "已执行；游戏内的实际效果尚未确认，以随后的玩家状态或通知为准。"
        elif status in {"pending", "running"}:
            body = title + "已提交，正在等待执行结果。"
        elif status == "uncertain":
            body = title + "可能已执行，目前无法确认结果；请勿重复提交。"
        elif status in {"failed", "rejected"}:
            body = title + "未完成，请在面板查看原因。"
        else:
            body = title + "的结果尚未确认，请在面板查看详情。"
        output = data.get("output")
        if diagnostics and isinstance(output, dict):
            body += "\n" + fmt.safe(output.get("text", ""))
        if isinstance(output, dict) and output.get("truncated"):
            body += "\n（仅收到部分输出，无法确认完整效果）"

        batch = data.get("batch")
        movement = data.get("movement")
        if isinstance(movement, dict):
            from .teleport import describe_movement
            body = describe_movement(movement)
        if isinstance(batch, dict) and batch.get("state") in {"preview", "cancelled", "completed", "partial"}:
            if batch["state"] == "preview":
                body = "🔎 操作预览：" + fmt.safe(batch.get("matched")) + " 个符合条件的对象。\n尚未修改游戏；确认后只处理本次预览中的对象。"
            elif batch["state"] == "cancelled":
                body = "已取消本次确认，没有修改游戏。"
            else:
                body = ("操作已结束\n已确认生效：" + fmt.safe(batch.get("verified"))
                        + " · 效果待确认：" + fmt.safe(batch.get("unconfirmed"))
                        + "\n跳过：" + fmt.safe(batch.get("skipped")) + " · 失败：" + fmt.safe(batch.get("failed")))
            if batch.get("version")==2:
                if batch.get('action') in {'set_player_hunger_rate','set_wanda_age'}:
                    label='面板附加饥饿倍率' if batch['action']=='set_player_hunger_rate' else '年龄'
                    for row in batch.get('items',[])[:1]:
                        body+='\n'+label+'：'+fmt.safe(row.get('before'))+' → '+fmt.safe(row.get('after'))
                        if batch['state']=='preview':body+='（预计）'
                        elif row.get('state')!='verified':body+='（结果待确认）'
                if batch["state"]=="preview" and any(row.get("destination")=="inventory" for row in batch.get("items",[])):
                    body+="\n物品交给所选玩家，不处理容器内物品。"
                if any(row.get("destination")=="rebuild" for row in batch.get("items",[])):
                    body+="\n在原位置重建科技站；不能找回烧毁前已丢失的数据。"
                if batch.get("units") and batch["state"]!="preview":
                    body+="\n已确认收到物品："+fmt.safe(batch["units"])+" 个"
                reasons={"INVENTORY_FULL":"背包空间不足","TARGET_CHANGED":"对象状态已变化","TARGET_UNAVAILABLE":"暂时无法核对对象","CPU_BUDGET":"已达到本次处理时限","INVENTORY_UNAVAILABLE":"无法读取背包","MATERIAL_UNAVAILABLE":"无法准备肥料","TRANSITION_PENDING":"变身或年龄调整的结果待确认；暂停时需等待游戏继续","REBUILD_UNAVAILABLE":"暂时无法重建","REBUILD_UNCONFIRMED":"重建结果待确认","EFFECT_UNCONFIRMED":"效果待确认","OUTPUT_UNCONFIRMED":"收获物品去向待确认"}
                counts={}
                for row in batch.get("items",[]):
                    if row.get("reason"):counts[row["reason"]]=counts.get(row["reason"],0)+1
                for reason,count in list(counts.items())[:5]:
                    body+="\n"+reasons.get(reason,"结果待核对")+"："+str(count)+" 项"
                if batch.get("matched")==0:
                    body="附近没有符合条件的对象，没有执行修改。"


    if len(body) > 1500:
        body = body[:1500] + "\n（仅展示部分内容）"
    error = result.get("error")
    if isinstance(error, dict):
        body += "\n" + fmt.safe(error.get("message") or error.get("code"))
    footer = labels.get(status, "结果尚未确认")
    if result.get("resultKind") == "workbench-run" and status == "succeeded":
        footer = "已执行"
        if isinstance(data, dict) and isinstance(data.get("batch"), dict):
            footer = {"preview": "预览有效期 30 秒", "cancelled": "已取消", "completed": "效果已确认" if data["batch"].get("matched") else "没有执行修改"}.get(data["batch"].get("state"), "部分完成，请勿重复提交")
        if isinstance(data, dict) and isinstance(data.get("movement"), dict):
            footer = {"preview":"检查有效期 60 秒", "cancelled":"已取消", "confirmed":"位置已确认"}.get(data["movement"].get("state"),"部分完成，请勿重复提交")
    if status in {"partial", "uncertain", "unavailable"} or result.get("complete") is not True:
        footer += " · 记录可能不完整"
    if result.get("observedAt"):
        footer += "\n时间：" + time_text(result["observedAt"]) + "（北京时间）"
    if result.get("cached"):
        footer += "（缓存）"
    if status in {"pending", "running", "uncertain"}:
        footer += "\n请回读原请求；正在等待确认，请勿重复提交。"
    if (diagnostics or status in {"partial", "failed", "rejected", "uncertain", "unavailable"}) and result.get("requestId"):
        footer += "\n查询原记录：" + fmt.safe(result["requestId"])
    if diagnostics and result.get("jobId"):
        footer += "\n任务：" + fmt.safe(result["jobId"])
    return body, footer


def options(pending, title="请选择"):
    size, page = pending.get("pageSize", 5), pending.get("page", 0)
    rows = pending["candidates"][page * size : (page + 1) * size]
    pages = max(1, (len(pending["candidates"]) + size - 1) // size)
    lines = [f"{title}（{page + 1}/{pages} 页，2 分钟内回复序号）"]
    for i, row in enumerate(rows, 1):
        text = f"{i}. {fmt.safe(row.get('name'))}"
        if "online" in row:
            text += " · " + fmt.presence(row) + " · " + fmt.safe(row.get("id"))
        if pending.get("mode") in {"location", "landmark"}:
            text += " · " + fmt.world_name(row.get("worldName") or row.get("worldId"))
        if pending.get("mode") == "landmark":
            text += f" · X {row['x']:.1f} / Z {row['z']:.1f}"
        lines.append(text)
    if pages > 1:
        lines.append("上一页 / 下一页")
    lines.append("可直接回复序号、上一页、下一页；也可引用本条或 @机器人选择。取消退出。")
    return "\n".join(lines)
