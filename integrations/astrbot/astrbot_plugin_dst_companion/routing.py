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
        r"^(?:请|麻烦|帮我|给我)(?:再)?(?:看看|看下)?\s*", "", text, count=1
    )
    text = re.sub(r"^(?:看看|看下)\s*", "", text, count=1)
    return re.sub(r"(?:吧|一下|谢谢)$", "", text).strip()


READ_KINDS = frozenset(
    {"rooms", "status", "players", "profile", "inventory", "network", "mods", "help", "backups", "recent", "activity", "trajectory", "combat"}
)
READ_CAPABILITIES = frozenset(
    {
        "room.locations",
        "room.performance",
        "room.map",
        "world.world_info",
        "world.online_players",
        "world.entity_count",
        "world.player_modes",
    }
)
READ_PREFIX = re.compile(
    r"^(?:查询一下|查看一下|查一下|看一下|查询|查看|查下|看看|看下|查)\s*(.+)$"
)


def is_read_intent(intent):
    return bool(
        intent
        and (
            intent.kind in READ_KINDS
            or intent.kind == "capability"
            and intent.capability in READ_CAPABILITIES
        )
    )


def performance_window(text):
    if re.search(r"(?:最近|近|过去)?\s*(?:1|一)\s*个?小时(?:内)?|\b1h\b", text, re.I):
        return "1h"
    if re.search(r"(?:最近|近|过去)?\s*(?:6|六)\s*个?小时(?:内)?|\b6h\b", text, re.I):
        return "6h"
    if re.search(
        r"(?:最近|近|过去)?\s*(?:7|七)\s*天(?:内)?|(?:最近|近|过去)?\s*(?:1|一)?\s*周(?:内)?|本周|这周|\b7d\b",
        text,
        re.I,
    ):
        return "7d"
    if re.search(
        r"(?:最近|近|过去)?\s*(?:1|一)\s*天(?:内)?|(?:最近|近|过去)?\s*24\s*个?小时(?:内)?|今天|今日|\b24h\b",
        text,
        re.I,
    ):
        return "24h"
    return ""


def _performance_intent(text, room, room_name):
    body = re.sub(r"(?:怎么样|如何|情况|报表|指标)?$", "", text.strip()).strip()
    if not body or COMPOUND.search(body) or re.search(r"网络|延迟|ping|直说|[？?]", body, re.I):
        return None
    exact = {
        "性能",
        "性能报表",
        "服务器性能",
        "服务端性能",
        "历史性能",
        "性能指标",
        "CPU内存",
        "cpu内存",
        "服务器负载",
        "运行指标",
    }
    if text in exact or body in exact or re.search(
        r"cpu|内存|性能报表|性能指标|服务器性能|服务端性能|服务器负载|历史性能|(?:最近|今天|今日).*(?:性能|负载)",
        text,
        re.I,
    ):
        window = performance_window(text)
        return Intent(
            "capability",
            room=room,
            room_name=room_name,
            capability="room.performance",
            parameters={"window": window} if window else {},
        )
    return None


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


def capability_terms(items):
    """Names users say for published capabilities (registry aliases first)."""
    terms = set()
    for item in items or []:
        if not isinstance(item, dict):
            continue
        values = [item.get("title", "")]
        for key in ("aliases", "searchTerms"):
            if isinstance(item.get(key), list):
                values.extend(item[key][:64])
        for row in item.get("actions", []) or []:
            if isinstance(row, dict):
                values.append(row.get("title", ""))
        terms.update(v for v in values if isinstance(v, str) and 2 <= len(v) <= 16)
    return terms


def interrupts_form(text, form, quote, items):
    """True when a reply to a pending value prompt is really a new request.

    A slot only consumes text that answers it. "查看地图" during a pending
    nickname prompt is a new request, never a nickname. Quoting the prompt
    itself always answers the prompt.
    """
    if not form or form.get("type") == "confirmation":
        return False
    if quote and quote == form.get("messageId"):
        return False
    value = normalize(text)
    raw = text.strip()
    if not value or re.search(r"[？?]", raw) or READ_PREFIX.fullmatch(value) or READ_PREFIX.fullmatch(raw):
        return True
    from .state import player_prompt

    if not player_prompt(form):
        return False
    return any(term in value for term in capability_terms(items))


def read_request_intent(text, aliases):
    """Recognize fixed read requests independently of display-card eligibility."""
    value = re.sub(r"^(?:AI(?:\s+|[：:])|问AI(?:\s+|[：:]))\s*", "", text.strip(), flags=re.I)
    intent = parse(value, aliases)
    return intent if is_read_intent(intent) else None


def parse(text: str, aliases: dict[str, str]) -> Intent | None:
    from .menus import request as menu_request

    navigation = menu_request(text) if len(text) <= 256 else None
    if navigation is not None:
        group, query = navigation
        return Intent("help", capability="menu:" + group if group else "",
                      parameters={"search": query} if query else None)
    if len(text) > 256 or DISCUSSION.search(text) or explicit_ai(text):
        return None
    value = normalize(text)
    match = READ_PREFIX.fullmatch(value)
    if match:
        # Prefer exact menu labels over interpreting “玩家信息” as a room name.
        candidate = _parse(match[1], aliases)
        if is_read_intent(candidate):
            return candidate
    # Prefer a complete, target-free query with a conversational ending over
    # treating that ending as a nickname ("查人呀"). Explicitly separated names
    # and names with their own suffix remain literal ("查人 呀", "查人 小呀").
    plain = re.sub(r"[呀啊呢吗嘛哦呗啦]{1,3}$", "", value).rstrip()
    candidate = None
    if plain != value:
        candidate = parse(plain, aliases)
        if (is_read_intent(candidate) and not candidate.player
                and not re.search(r"\s+[呀啊呢吗嘛哦呗啦]{1,3}$", value)):
            return candidate
    intent = _parse(value, aliases)
    if intent is not None:
        return intent
    # Recognise conversational endings only after an exact parse failed. Never
    # trim a supplied player name or turn a fuzzy sentence into a write action.
    if is_read_intent(candidate):
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
        located = re.match(r"^在\s*" + re.escape(alias) + r"(?:里|中)?\s*(?=为|给|把|将)", text)
        if located:
            room, text = aliases[alias], text[located.end():].strip()
            break
        if text.startswith(alias):
            room, text = aliases[alias], text[len(alias) :].strip()
            text = re.sub(r"^(?:现在|的)\s*", "", text)
            break
    if not room and re.match(r"^在.+?(?:为|给|把|将)", text):
        # Let the model resolve an unknown room clause. It must not become
        # part of a player's nickname in an otherwise valid short command.
        return None
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
        if match is None:
            match = re.fullmatch(r"传送\s*(.{1,80}?)\s*到\s*(.{1,48})", text)
        if match:
            return Intent("capability", room=room, room_name=room_name, capability="player.teleport", parameters={"playerId": match[1].strip(), "locationName": match[2].strip()})
    match = READ_PREFIX.fullmatch(text)
    if match:
        candidate = _parse_command(match[1], aliases, room, room_name)
        if is_read_intent(candidate):
            return candidate
    return _parse_command(text, aliases, room, room_name)


def _parse_command(text, aliases, room, room_name):
    # Parse the complete historical read before broad “查询玩家 <nickname>”.
    # A time clause is query context, never a player-directory search string.
    if re.match(r"^(?:查询|查看|查)?(?:总结|梳理|回顾|解释|评估)", text) or re.search(r"(?:活动|轨迹|战斗).*(?:和|及|、).*(?:活动|轨迹|战斗)", text):
        return None
    history_labels = {"活动": "activity", "活动记录": "activity", "最近活动": "activity", "玩家活动": "activity",
                      "做了什么": "activity", "干了什么": "activity", "做了啥": "activity", "干了啥": "activity",
                      "轨迹": "trajectory", "移动轨迹": "trajectory", "战斗": "combat", "战斗记录": "combat", "战斗汇总": "combat"}
    history_suffix = "|".join(sorted(history_labels, key=len, reverse=True))
    activity = re.fullmatch(
        rf"(.*?)(?:的)?(?:最近|过去|近)\s*({QUANTITY})\s*(?:个)?(天|小时|周)(?:内)?(?:的)?"
        rf"({history_suffix})", text
    )
    history = None
    if activity:
        history = (activity[1].strip(), quantity(activity[2]) * {"天": 24, "小时": 1, "周": 168}[activity[3]], activity[4])
    elif text in history_labels:
        history = ("", 24, text)
    else:
        short = re.fullmatch(r"(.+?)(?:的)?(活动记录|最近活动|移动轨迹|战斗记录|战斗汇总)", text)
        prefix = re.fullmatch(r"(?:查)?(活动记录|活动|轨迹|移动轨迹|战斗记录|战斗汇总)\s+(.+)", text)
        if short:
            history = (short[1].strip(), 24, short[2])
        elif prefix:
            history = (prefix[2].strip(), 24, prefix[1])
    if history and not COMPOUND.search(text):
        player, hours, label = history
        all_players = player in {"所有玩家", "全部玩家", "全员", "大家", "所有人"}
        if player in {"", "玩家", "所有玩家", "全部玩家", "全员", "大家", "所有人"}:
            player = ""
        if 1 <= hours <= 90 * 24:
            return Intent(history_labels[label], player=player, room=room, room_name=room_name,
                          parameters={"hours": hours, **({"scope": "all"} if all_players else {})})
        return None
    if text in {"地图", "世界地图", "小地图", "地图概览", "查看地图", "看看地图", "看地图", "地图状态"}:
        return Intent("capability", room=room, room_name=room_name, capability="room.map", parameters={})
    perf = _performance_intent(text, room, room_name)
    if perf is not None:
        return perf
    if not COMPOUND.search(text):
        skip = re.fullmatch(
            rf"(?:跳过|快进|推进|跳)\s*({QUANTITY})\s*天|跳天(?:\s*({QUANTITY}))?", text
        )
        if skip:
            days = quantity(skip[1] or skip[2] or "1")
            if 1 <= days <= 200:
                return Intent(
                    "capability",
                    room=room,
                    room_name=room_name,
                    capability="workbench.skip_days",
                    parameters={"days": days},
                )
        reset = re.fullmatch(
            r"(?:(?:先备份再)?(?:重置|重新生成)(?:全部|整个|所有)?(?:世界|存档|地图|房间)|删档重开|重开世界)"
            r"(?:\s*(不备份|不用备份|无需备份|不要备份|直接重置|先备份|备份))?",
            text,
        )
        if reset:
            no_backup = reset[1] in {"不备份", "不用备份", "无需备份", "不要备份", "直接重置"}
            return Intent(
                "capability",
                room=room,
                room_name=room_name,
                capability="room.reset",
                parameters={"backup": False} if no_backup else {},
            )
    if text in {"再次复活", "再复活", "再复活一次", "再次复活玩家"}:
        return Intent("capability", room=room, room_name=room_name, capability="player.resurrect", parameters={})
    # The short verb does not require an ASCII space before a player name.
    # Keep names beginning with “玩家” intact unless the long command is
    # separated from its target (or has no target yet).
    revival = re.fullmatch(r"(?:复活玩家(?:\s+|$)|复活\s*)(.*)", text)
    if revival and text not in {"复活玩家呀", "复活玩家啊", "复活玩家呢", "复活玩家吗"} and not COMPOUND.search(text):
        player = (revival[1] or "").strip()
        return Intent(
            "capability",
            room=room,
            room_name=room_name,
            capability="player.resurrect",
            parameters={"player_id": player} if player else {},
        )

    from .interactions import player_mode_request

    mode = player_mode_request(text)
    if mode and not COMPOUND.search(text):
        return Intent("capability", room=room, room_name=room_name,
                      capability="workbench.set_player_mode", parameters=mode)

    spawn = re.fullmatch(
        rf"(?:在)?\s*(.+?)\s*(?:的?位置|附近|旁边|身边)\s*(?:生成|刷出|刷)\s*({QUANTITY})\s*(?:只|头|个)\s*(.+)",
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
    placeholder_grant = re.fullmatch(
        rf"给\s*玩家\s*(?:(?:发放|发|给予|给)\s*)?({QUANTITY})\s*(?:个|份|件)\s*(.+)", text
    )
    if placeholder_grant and not COMPOUND.search(text):
        return Intent("capability", room=room, room_name=room_name, capability="player.give_item",
                      parameters={"item": placeholder_grant[2].strip(), "count": quantity(placeholder_grant[1])})
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
    if grant is None:
        # A marked, nonnumeric ASCII nickname gives an unambiguous boundary
        # for compact forms such as 给玩家Example100个木头. Numeric names and
        # Chinese numeral boundaries still require whitespace or a verb.
        grant = re.fullmatch(r"给\s*(玩家[A-Za-z_][A-Za-z_-]{0,63})([0-9]{1,4})\s*(?:个|份|件)\s*(.+)", text)
    if grant is None:
        reverse = re.fullmatch(rf"(?:再)?(?:发放|发|送)\s*({QUANTITY})\s*(?:个|份|件)\s*(.+?)\s*给\s*(.+)", text)
        pronoun = re.fullmatch(rf"(?:再)?给\s*(这个玩家|该玩家|他|她)\s*({QUANTITY})\s*(?:个|份|件)\s*(.+)", text)
        targetless = re.fullmatch(rf"(?:再)?(?:发放|发|送)\s*({QUANTITY})\s*(?:个|份|件)\s*(.+)", text)
        body = text[1:] if text.startswith(("再给", "再发", "再送")) else text
        if not COMPOUND.search(body) and (reverse or pronoun or targetless):
            if reverse:
                count, item, player = reverse.groups()
            elif pronoun:
                player, count, item = pronoun.groups()
            else:
                count, item = targetless.groups()
                player = ""
            return Intent("capability", room=room, room_name=room_name, capability="player.give_item",
                          parameters={**({"playerId": player.strip()} if player else {}), "item": item.strip(), "count": quantity(count)})
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
