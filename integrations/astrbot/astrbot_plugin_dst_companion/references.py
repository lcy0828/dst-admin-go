"""Resolve read-only replies from sent receipts, never from copied chat text."""

import asyncio
import re
import time

from .broadcast import clean, timestamp
from .sessions import group_origin
from .state import Target


def needs_reference(intent):
    return bool(
        intent
        and intent.kind in {"inventory", "profile", "network"}
        and intent.player in {"", "他", "他的"}
    )


def presence_identity(text):
    if not isinstance(text, str) or len(text) > 1600:
        return None
    lines = text.splitlines()
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
            for row in reversed(saved.get("state", {}).get("references", [])[-128:]):
                related = row.get("related", {})
                if (
                    row.get("messageId") == quote
                    and row.get("selfId") == bot
                    and 0 <= time.time() - row.get("at", 0) <= 86400
                    and related.get("roomId") in actor.rooms
                    and related.get("playerId")
                ):
                    return Target(related["roomId"], related["playerId"], related.get("name", ""))
        # Older versions did not persist receipts. Only an adapter-resolved
        # reply by this bot may use the compatibility path. Fetch the original
        # to verify its group as Reply itself contains no original group ID.
        event = actor.event
        from astrbot.api import message_components as C

        replies = [part for part in event.get_messages() if isinstance(part, C.Reply)]
        if (
            saved.get("source") != source
            or len(replies) != 1
            or str(replies[0].id) != quote
            or str(replies[0].sender_id) != bot
            or not presence_identity(replies[0].message_str)
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
                or not 0 <= time.time() - timestamp(original.get("time")) <= 86400
            ):
                return None
            parts = original.get("message")
            if (
                not isinstance(parts, list)
                or not 1 <= len(parts) <= 8
                or any(not isinstance(part, dict) or part.get("type") != "text" for part in parts)
            ):
                return None
            identity = presence_identity(
                "".join(part.get("data", {}).get("text", "") for part in parts)
            )
            if not identity:
                return None
            label, name = identity
            rooms = await p.engine.queries.rooms(actor, time.monotonic() + 3)
            matches = [r for r in rooms if clean(r.get("name")) == label]
            if len(matches) == 1:
                if not any(
                    row.get("status") == "sent"
                    and str(row.get("key", "")).startswith(matches[0]["id"] + "/")
                    # QQ timestamps have whole-second resolution.
                    and -2 <= timestamp(original["time"]) - row.get("at", 0) <= 20
                    for row in saved.get("state", {}).get("deliveries", [])[-100:]
                ):
                    return None
                # This is a name, not an asserted KU: Engine must resolve it in
                # the current authorized directory and ask on ambiguity.
                return Target(matches[0]["id"], name, name)
        except Exception:
            return None
        return None
