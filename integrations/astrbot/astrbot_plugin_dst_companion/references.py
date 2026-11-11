"""Resolve player selections from sent receipts, never from copied chat text.

A reference selects an identity; it never grants permission to perform an action.
"""

import asyncio
import re
import time

from .broadcast import clean, timestamp
from .sessions import group_origin
from .display import REFERENCE_LIMIT, REFERENCE_TTL, room_label
from .state import Target

PLAYER_REFERENCES = frozenset({"", "他", "她", "他的", "她的", "该玩家", "这个玩家", "此玩家"})


def needs_reference(intent):
    if not intent:
        return False
    if intent.kind in {"inventory", "profile", "network", "kick", "activity", "trajectory", "combat"}:
        return intent.player in PLAYER_REFERENCES
    if intent.kind == "capability" and intent.capability.startswith(("player.", "workbench.", "location.")):
        parameters = intent.parameters or {}
        return isinstance(parameters, dict) and all(
            isinstance(parameters.get(key, ""), str) and parameters.get(key, "") in PLAYER_REFERENCES
            for key in ("playerId", "player_id")
        )
    return False


def presence_identity(text):
    if not isinstance(text, str) or len(text) > 1600:
        return None
    lines = text.splitlines()
    if 3 <= len(lines) <= 6 and lines[1] == "━━━━━━━━━━━━":
        header = re.fullmatch(r"(?:🚪 玩家加入|👋 玩家离开) · (.+)", lines[0])
        player = re.fullmatch(r"👤 (.+) · [^·\n]+ · [^·\n]+", lines[2])
        if header and player and 0 < len(player[1]) < 80:
            return header[1], player[1]
        return None
    if (
        not 3 <= len(lines) <= 5
        or lines[0] not in {"🚪 有人来营地啦喵～", "👋 有人先走啦喵～"}
        or lines[1] != "────────────"
    ):
        return None
    match = re.fullmatch(r"(.+?) · 〖([^〖〗\n]+)〗·[^·\n]+·[^·\n]+", lines[2])
    # The renderer truncates at 80 characters: a truncated nickname is not an
    # identity. New receipts contain KU IDs and do not have this limitation.
    if not match or len(match[2]) >= 80:
        return None
    return match[1], match[2]


def legacy_notice_identity(text):
    """Only fixed historical templates; their original delivery is checked too."""
    presence = presence_identity(text)
    if presence:
        return *presence, "presence"
    if not isinstance(text, str) or len(text) > 1600:
        return None
    lines = text.splitlines()
    if (len(lines) != 5 or lines[1] != "━━━━━━━━━━━━" or lines[2] != "已观测"
            or not re.fullmatch(r"🕒 \d{2}/\d{2} \d{2}:\d{2}:\d{2}(?:（延迟 \d+ 秒送达）)?", lines[4])):
        return None
    header = re.fullmatch(r"📣 (.+) · (玩家死亡|玩家复活)", lines[0])
    body = re.fullmatch(r"[^·\n]+ · (.+)", lines[3])
    if not header or not body:
        return None
    pattern = (r"(.+)(?:已死亡。|被[^被\n。]{1,32}击败。)" if header[2] == "玩家死亡"
               else r"(.+)已从幽灵状态复活。")
    player = re.fullmatch(pattern, body[1])
    # The producer bounds names to 128 codepoints; an edge-length value could
    # be truncated. Missing names use this placeholder and are not identities.
    if not player or not 0 < len(player[1]) < 128 or player[1] == "一位玩家":
        return None
    return header[1], player[1], "event"


def delivered_notice(state, room, original_time, family):
    events = set(state.get("rooms", {}).get(room, {}).get("notice_seen", [])[-1024:])
    for row in state.get("deliveries", [])[-100:]:
        if row.get("status") != "sent" or not -2 <= original_time - row.get("at", 0) <= 20:
            continue
        key = str(row.get("key", ""))
        if family == "presence" and key.startswith(room + "/"):
            return True
        if family == "event" and key.startswith("notice/") and key[7:] in events:
            return True
    return False


class References:
    def __init__(self, plugin):
        self.plugin = plugin
        self.limit = asyncio.Semaphore(1)

    async def resolve(self, actor, quote, intent):
        if not quote or not needs_reference(intent):
            return None
        p = self.plugin
        platform, bot, session = actor.scope
        group = group_origin(session)
        try:
            source = p.connections.source()
        except Exception:
            return None
        target = p.state.quoted(actor.scope, quote, source=source) or p.state.quoted(
            (platform, bot, group), quote, source=source
        )
        if target:
            return target if target.room in actor.rooms and target.player else None
        saved = p.notifications.saved.get(group, {})
        if saved.get("source") == source:
            for row in reversed(saved.get("state", {}).get("references", [])[-REFERENCE_LIMIT:]):
                related = row.get("related", {})
                if (
                    row.get("messageId") == quote
                    and row.get("selfId") == bot
                    and 0 <= time.time() - row.get("at", 0) <= REFERENCE_TTL
                ):
                    if related.get("roomId") in actor.rooms and related.get("playerId"):
                        return Target(related["roomId"], related["playerId"], related.get("name", ""))
                    return None
        # Older versions did not persist receipts. Only an adapter-resolved
        # reply by this bot may use the compatibility path. Fetch the original
        # to verify its group as Reply itself contains no original group ID.
        event = actor.event
        from astrbot.api import message_components as C

        replies = [part for part in event.get_messages() if isinstance(part, C.Reply)] if event and hasattr(event, "get_messages") else []
        if (
            len(replies) == 1
            and str(replies[0].id) == quote
            and str(replies[0].sender_id) == bot
            and hasattr(p.state, "quoted_text")
        ):
            by_text = p.state.quoted_text(actor.scope, replies[0].message_str, source=source) or p.state.quoted_text(
                (platform, bot, group), replies[0].message_str, source=source
            )
            if by_text:
                return by_text if by_text.room in actor.rooms and by_text.player else None
        if (
            saved.get("source") != source
            or len(replies) != 1
            or str(replies[0].id) != quote
            or str(replies[0].sender_id) != bot
            or event.get_platform_name() != "aiocqhttp"
            or event.is_private_chat()
            or not quote.lstrip("-").isdigit()
            or len(quote) > 24
        ):
            return None
        try:
            async with asyncio.timeout(3), self.limit:
                original = await event.bot.call_action(
                    "get_msg", message_id=int(quote), self_id=bot
                )
            if (
                not isinstance(original, dict)
                or str(original.get("message_id")) != quote
                or original.get("message_type") != "group"
                or str(original.get("group_id")) != str(event.get_group_id())
                or str(original.get("sender", {}).get("user_id")) != bot
                or not 0 <= time.time() - timestamp(original.get("time")) <= REFERENCE_TTL
            ):
                return None
            parts = original.get("message")
            if (
                not isinstance(parts, list)
                or not 1 <= len(parts) <= 8
                or any(not isinstance(part, dict) or part.get("type") != "text" for part in parts)
            ):
                return None
            identity = legacy_notice_identity(
                "".join(part.get("data", {}).get("text", "") for part in parts)
            )
            if not identity:
                return None
            label, name, family = identity
            rooms = await p.engine.queries.rooms(actor, time.monotonic() + 3)
            # Current notices use short room labels; older ones used the full
            # name. Reproduce the broadcaster's collision suffix exactly.
            def labels(room):
                short = room_label(room.get("name"))
                if sum(room_label(r.get("name")) == short for r in rooms) > 1:
                    short += " · " + clean(room.get("id"), 128)
                return {clean(room.get("name")), short}

            matches = [r for r in rooms if r.get("id") in actor.rooms and label in labels(r)]
            if len(matches) == 1:
                # Generic event deliveries use notice/<event ID>. Their room
                # is proven by the persisted feed cursor's seen-event journal.
                if not delivered_notice(saved.get("state", {}), matches[0]["id"],
                                        timestamp(original["time"]), family):
                    return None
                # This is a name, not an asserted KU: Engine must resolve it in
                # the current authorized directory and ask on ambiguity.
                return Target(matches[0]["id"], name, name)
        except Exception:
            return None
        return None
