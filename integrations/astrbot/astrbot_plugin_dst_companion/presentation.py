"""Safe bounded templates; evidence/status footers cannot be configured away."""

import json
import string

from . import formatters as fmt

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


def business(result):
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
    body = (
        json.dumps(data, ensure_ascii=False, indent=2)
        if data is not None
        else "没有确认的数据，不能视为 0 或未发生。"
    )
    if len(body) > 1500:
        body = body[:1500] + "\n（仅展示部分内容）"
    error = result.get("error")
    if isinstance(error, dict):
        body += "\n" + fmt.safe(error.get("message") or error.get("code"))
    footer = labels.get(status, "结果尚未确认")
    footer += " · " + ("完整结果" if result.get("complete") is True else "覆盖不完整")
    footer += "\n来源：" + fmt.safe(result.get("source") or "未确认")
    if result.get("observedAt"):
        footer += " · " + fmt.safe(result["observedAt"])
    if result.get("cached"):
        footer += "（缓存）"
    if result.get("requestId"):
        footer += "\n请求：" + fmt.safe(result["requestId"])
    if result.get("jobId"):
        footer += " · 任务：" + fmt.safe(result["jobId"])
    if status in {"pending", "running", "uncertain"}:
        footer += "\n请回读原请求；已下发不表示完成，没有重新提交。"
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
