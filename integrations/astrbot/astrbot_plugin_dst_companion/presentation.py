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


def query_body(result, data):
    """Render known fields only. New capabilities do not dump protocol objects."""
    if data is None:
        return "没有确认的数据，请稍后查看原查询。"
    if not isinstance(data, dict):
        return "查询已返回，请在面板查看详细结果。"
    capability = result.get("capabilityId", "")
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
        body = "物品：" + name + " · 请求 " + fmt.safe(data.get("requested")) + " 个"
        if data.get("granted") is not None:
            body += "\n本次回执确认发放：" + str(data["granted"]) + " 个"
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
            body = title + "已执行。\n游戏内的实际效果尚未确认，请查看玩家或世界的当前状态。"
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

    if len(body) > 1500:
        body = body[:1500] + "\n（仅展示部分内容）"
    error = result.get("error")
    if isinstance(error, dict):
        body += "\n" + fmt.safe(error.get("message") or error.get("code"))
    footer = labels.get(status, "结果尚未确认")
    if result.get("resultKind") == "workbench-run" and status == "succeeded":
        footer = "指令执行完成 · 游戏内效果待确认"
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
        lines.append(text)
    if pages > 1:
        lines.append("上一页 / 下一页")
    lines.append("可直接回复序号、上一页、下一页；也可引用本条或 @机器人选择。取消退出。")
    return "\n".join(lines)
