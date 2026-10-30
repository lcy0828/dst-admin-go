"""Bounded whole-message grammar. Parsing never performs I/O or invokes a model."""

import re
from dataclasses import dataclass

from .commands import BY_PHRASE


@dataclass(frozen=True)
class Intent:
    kind: str
    player: str = ""
    room: str = ""
    room_name: str = ""
    capability: str = ""
    parameters: object = None
    all_rooms: bool = False


# Grammar ambiguity guard, NOT authorization or a Lua safety classifier.
DISCUSSION = re.compile(
    r"不要|别(?:查|重启|备份|踢)|不(?:要|用|想)|如果|假如|能不能实现|怎么实现|如何实现|为什么|分析|代码|```|[;；\n]"
)
COMPOUND = re.compile(r"然后|并且|以及|再帮|能否|是不是|功能|怎么|是否|和.*(?:背包|网络)")


def explicit_ai(text):
    return re.match(r"^(?:AI(?:\s+|[：:]|分析)|问AI(?:\s+|[：:]))", text.strip(), re.I) is not None


def normalize(text):
    text = text.strip().rstrip("。！？!?～~")
    text = re.sub(
        r"^(?:请|麻烦|帮我|给我)(?:再)?(?:看看|看下|查一下|查询一下)?\s*", "", text, count=1
    )
    text = re.sub(r"^(?:看看|看下)\s*", "", text, count=1)
    return re.sub(r"(?:吧|一下|谢谢)$", "", text).strip()


def parse(text: str, aliases: dict[str, str]) -> Intent | None:
    if len(text) > 256 or DISCUSSION.search(text) or explicit_ai(text):
        return None
    text = normalize(text)
    aliases = dict(aliases)
    chinese = {
        "一": "1",
        "二": "2",
        "三": "3",
        "四": "4",
        "五": "5",
        "六": "6",
        "七": "7",
        "八": "8",
        "九": "9",
        "十": "10",
    }
    for word, number in chinese.items():
        if number + "服" in aliases:
            aliases.setdefault(word + "服", aliases[number + "服"])
    room = ""
    room_name = ""
    for alias in sorted(aliases, key=len, reverse=True):
        if text.startswith(alias):
            room, text = aliases[alias], text[len(alias) :].strip()
            text = re.sub(r"^(?:现在|的)\s*", "", text)
            break
    if not room:
        match = re.match(r"^([一二三四五六七八九十]|[1-9][0-9]?)服\s*", text)
        if match:
            number = chinese.get(match[1], match[1])
            room_name, text = number + "服", text[match.end() :].strip()
            text = re.sub(r"^(?:现在|的)\s*", "", text)
    if (room or room_name) and not text:
        text = "信息"
    command = BY_PHRASE.get(text)
    if command:
        return Intent(
            command.kind,
            "我" if "我" in text and command.kind == "profile" else "他" if "他" in text else "",
            room,
            room_name=room_name,
            all_rooms=command.kind == "status"
            and text in {"查房", "查服", "查群"}
            and not (room or room_name),
        )

    # Display names are resolved exactly within the caller's current directory.
    match = re.fullmatch(r"(?:查询|查看|查一下|看看|看下)\s*(.+?)(?:的)?(?:信息|状态|情况)", text)
    if not match:
        match = re.fullmatch(r"(.+?)的(?:信息|状态|情况)", text)
    if match and not room:
        name = match[1].strip()
        if name and not COMPOUND.search(name):
            return Intent(
                "status", room=aliases.get(name, ""), room_name="" if name in aliases else name
            )

    patterns = (
        ("network", r"(?:查网络|查询网络)\s+(.+)"),
        ("inventory", r"(?:查背包|查询背包|查装备|查物资|查物品)\s+(.+)"),
        ("inventory", r"(?:查|查询)?\s*(.+?)(?:的)?背包(?:里有什么)?"),
        ("network", r"(?:查|查询)\s*(.+?)(?:的)?(?:网络|延迟|网络质量)"),
        ("profile", r"(?:查玩家|查询玩家|查人)\s*(.+)"),
        ("profile", r"(.+?)(?:在(?:哪|哪里)|的位置)"),
        ("kick", r"踢出\s*(.+)"),
    )
    for kind, expression in patterns:
        match = re.fullmatch(expression, text)
        if match:
            target = match[1].strip().strip('“”"')
            if not target or COMPOUND.search(target):
                return None
            return Intent(kind, target, room, room_name=room_name)
    # Writes require an exact configured alias, never a remembered fallback
    # after failing to resolve an explicit natural-language room description.
    for kind, verb in (("restart", "重启"), ("start", "启动"), ("stop", "停止")):
        if text.startswith(verb) and text[len(verb) :].strip() in aliases:
            return Intent(kind, room=aliases[text[len(verb) :].strip()])
    return None
