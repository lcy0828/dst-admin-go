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
    confirm_required: bool = False


# Grammar ambiguity guard, NOT authorization or a Lua safety classifier.
DISCUSSION = re.compile(
    r"不要|别(?:查|重启|备份|踢)|不(?:要|用|想)|如果|假如|能不能实现|怎么实现|如何实现|为什么|分析|代码|```|[;；\n]"
)
COMPOUND = re.compile(
    r"然后|并且|以及|再帮|能否|是不是|功能|怎么|是否|和.*(?:背包|网络)|[，,]"
    r"|(?:再|之后|同时)(?:查询|查看|查|重启|停止|启动|备份|给|生成|刷)|比较|对比|分别|和.+谁"
)
QUANTITY = r"(?:-?[0-9]{1,4}|[一二两三四五六七八九]?十[一二三四五六七八九]?|[一二两三四五六七八九])"


def quantity(value):
    if re.fullmatch(r"-?[0-9]+", value):
        return int(value)
    digits = dict(zip("一二三四五六七八九", range(1, 10), strict=True))
    digits["两"] = 2
    if "十" in value:
        tens, units = value.split("十")
        return digits.get(tens, 1) * 10 + digits.get(units, 0)
    return digits[value]


def explicit_ai(text):
    return re.match(r"^(?:AI(?:\s+|[：:]|分析)|问AI(?:\s+|[：:]))", text.strip(), re.I) is not None


def normalize(text):
    text = text.strip().rstrip("。！？!?～~")
    text = re.sub(
        r"^(?:请|麻烦|帮我|给我)(?:再)?(?:看看|看下|查一下|查询一下)?\s*", "", text, count=1
    )
    text = re.sub(r"^(?:看看|看下)\s*", "", text, count=1)
    return re.sub(r"(?:吧|一下|谢谢)$", "", text).strip()


READ_KINDS = frozenset(
    {"rooms", "status", "players", "profile", "inventory", "network", "mods", "help", "backups"}
)
READ_PREFIX = re.compile(
    r"^(?:查询一下|查看一下|查一下|看一下|查询|查看|查下|看看|看下|查)\s*(.+)$"
)


def is_cancel_reply(text):
    """Whole replies to an active prompt, not matches inside other requests."""
    value = text.strip().rstrip("。！!～~")
    return value in {
        "取消", "返回", "取消选择", "算了", "不查了", "先不查了", "不用了", "先不用了",
        "不选了", "不需要了", "算了不查了", "算了，不查了", "算了,不查了",
    }


def form_reply(text):
    """A new conversational request must not be consumed as a nickname/value."""
    value = re.sub(r"^(?:我说的是|我指的是|名字是|昵称是)\s*", "", text.strip())
    if DISCUSSION.search(value) or COMPOUND.search(value) or re.search(r"先不|暂不|[？?]", value):
        return None
    return value


def read_request_intent(text, aliases):
    """Recognize fixed read requests independently of display-card eligibility."""
    value = re.sub(r"^(?:AI(?:\s+|[：:])|问AI(?:\s+|[：:]))\s*", "", text.strip(), flags=re.I)
    intent = parse(value, aliases)
    return intent if intent and intent.kind in READ_KINDS else None


def parse(text: str, aliases: dict[str, str]) -> Intent | None:
    if len(text) > 256 or DISCUSSION.search(text) or explicit_ai(text):
        return None
    value = normalize(text)
    match = READ_PREFIX.fullmatch(value)
    if match:
        # Prefer exact menu labels over interpreting “玩家信息” as a room name.
        candidate = _parse(match[1], aliases)
        if candidate and candidate.kind in READ_KINDS:
            return candidate
    intent = _parse(value, aliases)
    if intent is not None:
        return intent
    # Recognise conversational endings only after an exact parse failed. Never
    # trim a supplied player name or turn a fuzzy sentence into a write action.
    plain = re.sub(r"[呀啊呢吗嘛哦呗啦]{1,3}$", "", value).rstrip()
    if plain != value:
        candidate = parse(plain, aliases)
        if candidate and candidate.kind in READ_KINDS:
            return candidate
    return None


def _parse(text: str, aliases: dict[str, str]) -> Intent | None:
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

    elif room or room_name:
        text = normalize(text)

    if text in {"有哪些地点", "地点列表", "查看地点", "已保存地点"}:
        return Intent("capability", room=room, room_name=room_name, capability="room.locations", parameters={})
    if not COMPOUND.search(text):
        match = re.fullmatch(r"(?:把|将)?\s*(.{1,80}?)\s*(?:的)?当前位置(?:保存|记)为\s*(.{1,48})", text)
        if match:
            return Intent("capability", room=room, room_name=room_name, capability="location.save", parameters={"playerId": match[1].strip(), "name": match[2].strip()})
        match = re.fullmatch(r"(?:把|将)\s*(.{1,80}?)\s*传送到\s*(.{1,48})", text)
        if match:
            return Intent("capability", room=room, room_name=room_name, capability="player.teleport", parameters={"playerId": match[1].strip(), "locationName": match[2].strip()})
    match = READ_PREFIX.fullmatch(text)
    if match:
        candidate = _parse_command(match[1], aliases, room, room_name)
        if candidate and candidate.kind in READ_KINDS:
            return candidate
    return _parse_command(text, aliases, room, room_name)


def _parse_command(text, aliases, room, room_name):
    revival = re.fullmatch(r"(?:复活玩家|复活)(?:\s+(.+))?", text)
    if revival and not COMPOUND.search(text):
        player = (revival[1] or "").strip()
        return Intent(
            "capability",
            room=room,
            room_name=room_name,
            capability="player.resurrect",
            parameters={"player_id": player} if player else {},
        )

    spawn = re.fullmatch(
        rf"(?:在)?\s*(.+?)\s*(?:附近|旁边|身边)\s*(?:生成|刷出|刷)\s*({QUANTITY})\s*(?:只|头|个)\s*(.+)",
        text,
    )
    if spawn and not COMPOUND.search(text):
        player, count, entity = spawn.groups()
        return Intent(
            "capability",
            room=room,
            room_name=room_name,
            capability="player.spawn_entity",
            parameters={
                "playerId": player.strip(),
                "entity": entity.strip(),
                "count": quantity(count),
            },
        )
    grant = re.fullmatch(
        rf"给\s*(.+?)\s+(?:发放|发|给予|给)\s*({QUANTITY})\s*(?:个|份|件)\s*(.+)", text
    )
    if grant is None:
        explicit = re.fullmatch(
            rf"给\s*(.+?)\s*(?:发放|发|给予|给)\s*({QUANTITY})\s*(?:个|份|件)\s*(.+)", text
        )
        # Without a verb, whitespace separates identity from quantity. Do not
        # guess whether a numeral belongs to an unseparated player nickname.
        implicit = re.fullmatch(
            rf"(?:给|发给|送给)\s*(.+?)\s+({QUANTITY})\s*(?:个|份|件)\s*(.+)", text
        )
        if explicit and implicit and explicit[1].strip() != implicit[1].strip():
            return None
        grant = explicit or implicit
    if grant and not COMPOUND.search(text):
        player, count, item = grant.groups()
        return Intent(
            "capability",
            room=room,
            room_name=room_name,
            capability="player.give_item",
            parameters={"playerId": player.strip(), "item": item.strip(), "count": quantity(count)},
        )
    if text in {
        "所有服状态",
        "所有房间状态",
        "全部房间状态",
        "所有服务器状态",
        "各服状态",
        "所有服信息",
        "各服信息",
    }:
        return Intent("status", room=room, room_name=room_name, all_rooms=not (room or room_name))
    if text in {
        "有哪些人在玩",
        "现在有哪些人在玩",
        "现在谁在玩",
        "谁在游戏里",
        "在线名单",
        "在线人数",
        "现在在线人数",
    }:
        return Intent("players", room=room, room_name=room_name)
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
    if not match:
        match = re.fullmatch(r"(.+?(?:服务器|房间|营地|服))(?:信息|状态|详情)", text)
    if match and not room:
        name = match[1].strip()
        if name and not COMPOUND.search(name):
            return Intent(
                "status", room=aliases.get(name, ""), room_name="" if name in aliases else name
            )

    patterns = (
        (
            "profile",
            r"(?:玩家信息|玩家资料|个人资料|查玩家信息|查询玩家信息|查看玩家信息|查询玩家资料)\s+(.+)",
        ),
        ("profile", r"(?:查询|查看|查)?\s*(.+?)(?:的)?(?:个人信息|个人资料|玩家资料)"),
        ("inventory", r"(?:玩家背包|玩家装备|物品栏|查看物品栏|查询物品栏)\s+(.+)"),
        ("inventory", r"(?:查询|查看|查)?\s*(.+?)(?:的)?(?:物品栏|装备)(?:里有什么|有什么)?"),
        ("network", r"(?:查网络|查询网络)\s+(.+)"),
        ("inventory", r"(?:查背包|查询背包|查装备|查物资|查物品)\s+(.+)"),
        ("inventory", r"(?:查询|查看|查)?\s*(.+?)(?:的)?背包(?:里有什么)?"),
        ("network", r"(?:查询|查看|查)\s*(.+?)(?:的)?(?:网络延迟|网络连接质量|网络质量|网络|延迟)"),
        ("profile", r"(?:查玩家|查询玩家|查人)\s*(.+)"),
        ("profile", r"(.+?)(?:现在)?(?:在(?:哪里|哪)(?:个世界)?|的位置|所在世界)"),
        ("kick", r"(?:踢出|踢人)\s*(.+)"),
    )
    for kind, expression in patterns:
        match = re.fullmatch(expression, text)
        if match:
            target = match[1].strip().strip('“”"')
            # CQ-looking text in a name stays literal data, never an At part.
            grammar = re.sub(r"\[CQ:[^\]\r\n]+\]", "", target) if kind in READ_KINDS else target
            if not target or COMPOUND.search(grammar):
                return None
            return Intent(kind, target, room, room_name=room_name)
    # Writes require an exact configured alias, never a remembered fallback
    # after failing to resolve an explicit natural-language room description.
    for kind, verb in (("restart", "重启"), ("start", "启动"), ("stop", "停止"), ("save", "保存")):
        if text.startswith(verb):
            name = text[len(verb):].strip()
            if name in aliases or re.fullmatch(r"(?:[一二三四五六七八九十]|[1-9][0-9]?)服", name):
                values = {"room": aliases.get(name, ""), "room_name": "" if name in aliases else name}
                if kind == "save":
                    return Intent("capability", capability="room.save", **values)
                return Intent(kind, **values)
    return None
