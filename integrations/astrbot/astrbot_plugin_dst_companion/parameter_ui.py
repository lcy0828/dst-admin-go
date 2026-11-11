"""Translate bounded human answers into the panel's declared parameter schema."""

import re

from .capabilities import parse_parameter, validate
from .display import text
from .routing import QUANTITY, quantity

LABELS = {"enabled": "开关", "count": "数量", "quantity": "数量", "item": "物品名称",
          "entity": "生物名称", "prefab": "物品或生物名称", "radius": "作用范围（格）",
          "mode": "模式", "message": "消息内容", "reason": "原因", "duration": "时长",
          "multiplier": "倍率", "name": "名称", "confirmation": "确认名称", "backup": "重置前备份",
          "locationName": "目的地"}
SWITCHES = {"开启": True, "打开": True, "启用": True, "开": True, "是": True,
            "关闭": False, "关掉": False, "停用": False, "关": False, "否": False}


def choices(item, key, schema):
    # These are human labels for enum values already declared by the panel.
    # No alias can introduce another value, operation or capability.
    result = {}
    for row in item.get("actions", []):
        if not isinstance(row, dict):
            continue
        parameters = row.get("parameters")
        if not isinstance(parameters, dict):
            continue
        value = parameters.get(key)
        title = row.get("title")
        if isinstance(value, str) and value in schema.get("enum", []) and isinstance(title, str):
            result[title] = value
    return result


def answer(value, schema, *, item, key):
    if schema.get("type") == "boolean" and value in SWITCHES:
        return validate(SWITCHES[value], schema)
    if schema.get("type") == "integer":
        number = value
        if key in {"count", "quantity"}:
            number = re.sub(r"(?:个|只|份|件)$", "", number).strip()
        if re.fullmatch(QUANTITY, number):
            return validate(quantity(number), schema)
    if schema.get("type") == "string":
        value = choices(item, key, schema).get(value, value)
    return parse_parameter(value, schema)


def prompt(item, key, schema):
    label = schema.get("title") or LABELS.get(key) or "“" + key + "”参数"
    if schema.get("type") == "boolean":
        return "请回复“开启”或“关闭”。回复取消退出。"
    if schema.get("type") in {"integer", "number"}:
        bounds = []
        if "minimum" in schema:
            bounds.append("最少 " + str(schema["minimum"]))
        if "maximum" in schema:
            bounds.append("最多 " + str(schema["maximum"]))
        return "请输入" + label + ("（" + "，".join(bounds) + "）" if bounds else "") + "。回复取消退出。"
    labels = choices(item, key, schema)
    values = schema.get("enum", [])
    names = [next((title for title, value in labels.items() if value == option), str(option)) for option in values[:12]]
    options = "，可选：" + " / ".join(names) if names else ""
    return "请提供" + label + options + "。回复取消退出。"


def invalid_prompt(item, key, schema):
    return "这次尚未提交。" + prompt(item, key, schema)


def describe(item, parameters, target):
    lines = []
    props = item.get("parameters", {}).get("properties", {})
    for key, value in parameters.items():
        if key in {"phase", "preview_id", "confirmed", "confirmation", "feedback"}:
            continue
        if key == "backup" and item.get("id") == "room.reset":
            continue  # The confirmation prompt states the backup choice once.
        schema = props.get(key, {})
        hint = item.get("parameterHints", {}).get(key)
        if hint == "player":
            label = "玩家"
            shown = target.name if target.player == value and target.name else value
        elif hint == "world":
            label = "世界"
            shown = {"Master": "地面", "Caves": "洞穴"}.get(value, value) if isinstance(value, str) else value
        else:
            label = schema.get("title") or LABELS.get(key) or "“" + key + "”参数"
            shown = "开启" if value is True else "关闭" if value is False else next(
                (name for name, option in choices(item, key, schema).items() if option == value), value)
        lines.append(text(label) + "：" + text(shown))
    return "\n".join(lines)
