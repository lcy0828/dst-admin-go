"""Fixed, bounded text templates. No expressions, I/O or authorization changes."""

import copy
import hashlib
import json
import re
import string
from dataclasses import dataclass
from functools import lru_cache
from types import MappingProxyType

from .display import GAME_GLYPHS

MAX_TEMPLATE = 2000
MAX_BYTES = 64 * 1024
COMMON = frozenset({"room_name", "room_full_name", "time"})
QUERY = COMMON | {"title", "body", "room", "player"}
PRESENCE = COMMON | {
    "player_name",
    "character",
    "world_name",
    "player_age",
    "online",
    "max_players",
    "world_day",
    "season",
    "details",
}


@dataclass(frozen=True)
class Definition:
    label: str
    default: str
    fields: frozenset
    required: str = ""


DEFINITIONS = {
    "chat_line": Definition(
        "聊天条目", "{player_name}：{message}", COMMON | {"player_name", "message"}, "message"
    ),
    "chat_batch": Definition(
        "聊天播报", "💬 {room_name} · 游戏聊天\n{lines}", COMMON | {"lines"}, "lines"
    ),
    "player_join": Definition(
        "进服播报",
        "🚪 玩家加入 · {room_name}\n━━━━━━━━━━━━\n👤 {player_name} · {character} · {world_name}\n{details}",
        PRESENCE,
        "details",
    ),
    "player_leave": Definition(
        "离服播报",
        "👋 玩家离开 · {room_name}\n━━━━━━━━━━━━\n👤 {player_name} · {character} · {world_name}\n{details}",
        PRESENCE,
        "details",
    ),
    "server_start": Definition(
        "开服播报", "🟢 世界已启动\n{room_name} · {world_name}", COMMON | {"world_name", "reason"}
    ),
    "server_stop": Definition(
        "停服播报", "⚫ 世界已停止\n{room_name} · {world_name}", COMMON | {"world_name", "reason"}
    ),
    "server_error": Definition(
        "异常停服",
        "⚠️ 世界异常退出\n{room_name} · {world_name}\n{reason}",
        COMMON | {"world_name", "reason"},
        "reason",
    ),
}
for _key, _label in (
    ("status", "房间概况"),
    ("players", "在线玩家"),
    ("profile", "玩家资料"),
    ("inventory", "玩家背包"),
    ("mods", "模组列表"),
    ("network", "连接质量"),
    ("query", "扩展查询"),
    ("action", "管理回执"),
):
    DEFINITIONS[_key] = Definition(_label, "{title}\n{body}", QUERY, "body")


def template_id(value):
    if value in DEFINITIONS:
        return value
    return next((key for key, item in DEFINITIONS.items() if item.label == value), None)


@lru_cache(maxsize=256)
def compile_template(kind, source):
    if kind not in DEFINITIONS:
        raise ValueError("未知模板类型")
    if not isinstance(source, str) or not source.strip() or len(source) > MAX_TEMPLATE:
        raise ValueError("模板正文须为 1～2000 个字符")
    try:
        source.encode("utf-8")
    except UnicodeEncodeError:
        raise ValueError("模板包含无效 Unicode") from None
    if "[cq:" in source.lower() or any(ord(c) < 32 and c not in "\n\t" for c in source):
        raise ValueError("模板不支持 CQ 指令或控制字符")
    fields = set()
    count = 0
    for _, field, spec, conversion in string.Formatter().parse(source):
        if field is None:
            continue
        if field not in DEFINITIONS[kind].fields or spec or conversion:
            raise ValueError("不支持的占位符：" + str(field)[:80])
        count += 1
        if count > 32 or field == DEFINITIONS[kind].required and field in fields:
            raise ValueError("占位符最多 32 处；正文与证据字段只允许插入一次")
        fields.add(field)
    required = DEFINITIONS[kind].required
    if required and required not in fields:
        raise ValueError("必须保留 {" + required + "}，以免隐藏正文或执行证据")
    return source


def validate_templates(rows):
    if not isinstance(rows, list) or len(rows) > len(DEFINITIONS):
        raise ValueError("模板必须是有界的列表")
    result = {}
    for row in rows:
        if not isinstance(row, dict) or set(row) - {"type", "body", "__template_key"}:
            raise ValueError("模板项包含未知字段")
        kind = row.get("type")
        if not isinstance(kind, str) or row.get("__template_key", "message") not in {
            "message",
            kind,
        }:
            raise ValueError("未知表单模板类型")
        if not isinstance(kind, str) or kind not in DEFINITIONS or kind in result:
            raise ValueError("未知或重复的模板类型")
        result[kind] = compile_template(kind, row.get("body"))
    if len(json.dumps(rows, ensure_ascii=False).encode("utf-8")) > MAX_BYTES:
        raise ValueError("模板总量超过 64 KiB")
    return result


def rows_from(values):
    return [{"__template_key": key, "type": key, "body": value} for key, value in values.items()]


def revision(rows):
    return hashlib.sha256(json.dumps(rows, sort_keys=True, ensure_ascii=True).encode()).hexdigest()


def stored_templates(rows):
    """Old invalid templates fall back individually; no query/feed is disabled."""
    try:
        return validate_templates(rows), False
    except (ValueError, TypeError):
        result = {}
        try:
            if len(json.dumps(rows, ensure_ascii=False).encode()) > MAX_BYTES:
                return result, True
        except UnicodeEncodeError:
            return result, True
        if isinstance(rows, list) and len(rows) <= len(DEFINITIONS):
            duplicates = set()
            for row in rows:
                try:
                    item = validate_templates([row])
                    key = next(iter(item))
                    if key in result or key in duplicates:
                        result.pop(key, None)
                        duplicates.add(key)
                    else:
                        result.update(item)
                except (ValueError, TypeError):
                    pass
        return result, True


@lru_cache(maxsize=128)
def stored_snapshot(encoded):
    values, invalid = stored_templates(json.loads(encoded))
    return MappingProxyType(values), invalid


def safe_value(value, limit=100):
    # Preserve line breaks, but neutralize transport markup and game-only glyphs.
    if not isinstance(value, (str, int, float)) or isinstance(value, bool):
        return "未确认"
    value = re.sub(r"[\x00-\x08\x0b-\x1f\x7f]", " ", str(value))
    value = GAME_GLYPHS.sub("", value).replace("[CQ:", "［CQ:")
    if len(value) > limit:
        if limit < 500:
            value = value[: limit - 1] + "…"
        else:
            tail = min(650, limit // 3)
            marker = "\n（正文过长，已节选；末尾证据保留）\n"
            value = value[: limit - tail - len(marker)] + marker + value[-tail:]
    return value


def render_text(kind, source, values):
    definition = DEFINITIONS[kind]
    clean = {
        key: safe_value(
            values.get(key, "未确认"),
            2700
            if key == definition.required
            else (300 if key in {"title", "room", "room_name", "room_full_name"} else 100),
        )
        for key in definition.fields
    }
    body = clean.get(definition.required, "")
    # Allocate expansion budget to decoration. Never chop original receipt/evidence.
    limit = 2400 if kind == "chat_line" else 3000
    try:
        compile_template(kind, source)
        rendered = source.format_map(clean).strip()
        if definition.required and body and body not in rendered:
            raise ValueError("missing evidence")
        if len(rendered) > limit:
            raise ValueError("output budget")
        return rendered
    except (ValueError, KeyError, IndexError):
        return definition.default.format_map(clean).strip()


class MessageTemplates:
    def __init__(self, config, enrollment):
        self.config, self.enrollment = config, enrollment
        self.refresh()

    def refresh(self):
        self.global_rows = copy.deepcopy(self.config.get("message_templates", []))
        self.global_values, self.invalid_global = stored_templates(self.global_rows)

    def rows(self, session, global_scope=False):
        if global_scope:
            return copy.deepcopy(self.global_rows)
        return copy.deepcopy(self.enrollment.records.get(session, {}).get("messageTemplates", []))

    def selected(self, session, kind, *, draft=None, global_scope=False, legacy=None):
        local, invalid = stored_snapshot(
            json.dumps(self.rows(session), ensure_ascii=True, sort_keys=True)
        )
        if global_scope:
            local = {}
        global_values = self.global_values
        if draft is not None:
            if global_scope:
                global_values = validate_templates(draft)
                local = {}  # Global preview is independent of this group's override.
            else:
                local = validate_templates(draft)
        if kind in local:
            return local[kind], "本群覆盖"
        if kind in global_values:
            return global_values[kind], "全局覆盖"
        if legacy:
            try:
                compile_template(kind, legacy)
                return legacy, "旧版查询模板"
            except (ValueError, TypeError):
                pass
        return DEFINITIONS[kind].default, "内置默认" + (
            "（无效覆盖已回退）" if invalid or self.invalid_global else ""
        )

    def render(self, session, kind, values, *, legacy=None):
        kind = kind if kind in DEFINITIONS else "query"
        source, _ = self.selected(session, kind, legacy=legacy)
        return render_text(kind, source, values)


def sample_values(kind):
    # Entirely synthetic; no model, game command, directory lookup or real player.
    values = {
        "room_name": "示例服",
        "room_full_name": "示例营地「示例服」",
        "time": "10/31 12:00:00（北京时间）",
        "room": "示例服",
        "player": "示例玩家",
        "player_name": "示例玩家",
        "character": "温蒂",
        "world_name": "地面",
        "player_age": "20",
        "online": "1",
        "max_players": "12",
        "world_day": "386",
        "season": "🌱 春",
        "details": "角色年龄：20 天\n👥 在线 1/12 人 · 第 386 天 🌱",
        "message": "准备去打龙蝇啦",
        "lines": "示例玩家：准备去打龙蝇啦\n示例队友：稍等我一下",
        "reason": "示例异常：进程意外退出，原因待排查",
        "title": "🏕️ 示例服 · " + DEFINITIONS[kind].label,
        "body": "🌍 世界状态\n🟢 地面 · 运行中\n  第 386 天 · 🌱 春 · 剩余 20 天\n\n👥 在线 1 / 12 人\n· 示例玩家 · 温蒂 · 地面\n\n📦 模组 · 已配置 0 个",
    }
    if kind == "inventory":
        values["body"] = (
            "🎒 背包 · 存档快照\n🛡️ 装备\n· 步行手杖 ×1\n📦 物品栏\n· 肉丸 ×2\n· 示例模组容器 ×1\n  └ 蓝宝石 ×3\n存档时间：2026-10-31 11:30:00（北京时间）\n仅代表最近落盘记录；不代表当前在线状态。"
        )
    elif kind == "action":
        values["body"] = (
            "物品：肉丸 · 请求 3 个\n结果尚未确认\n来源：示例回执\n不要重复提交，请查询原请求。"
        )
    elif kind == "players":
        values["body"] = "👥 在线 1 / 12 人\n· 示例玩家 · 温蒂 · 地面"
    elif kind in {"mods", "profile", "network", "query"}:
        values["body"] = "示例数据 · 部分字段未确认\n未确认信息不视为 0。"
    return values
