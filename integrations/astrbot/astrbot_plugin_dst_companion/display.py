"""Portable plain-text labels and explicit local times for QQ messages."""

import re
from datetime import datetime, timedelta, timezone

BEIJING = timezone(timedelta(hours=8))
GAME_GLYPHS = re.compile(r"[\ue000-\uf8ff\U000f0000-\U000ffffd\U00100000-\U0010fffd]")


def chat_text(value):
    """Convert presentation markup only; keep evidence, identifiers and URLs intact."""
    value = str(value or "")
    value = re.sub(r"(?m)^[ \t]*```(?:[\w.+-]+)?[ \t]*$\n?", "", value)
    value = re.sub(r"```([^\n]+?)```", r"\1", value)
    value = re.sub(r"(?m)^ {0,3}#{1,6}\s+", "", value)
    value = re.sub(r"\[([^\]\n]+)\]\((https?://[^\s)]+)\)", r"\1（\2）", value)
    value = re.sub(r"`([^`\n]+)`", r"\1", value)
    for _ in range(2):
        value = re.sub(r"\*\*([^\n]+?)\*\*", r"\1", value)
        value = re.sub(r"(?<!\w)__(?=\S)([^\n]+?)(?<=\S)__(?!\w)", r"\1", value)
    value = re.sub(r"(?<![\w*])\*([^\s*](?:[^*\n]*?[^\s*])?)\*(?![\w*])", r"\1", value)
    return value.strip()


def text(value, limit=100):
    value = "" if value is None else str(value)
    value = re.sub(r"[\x00-\x1f\x7f]", " ", value)
    return GAME_GLYPHS.sub("", value).replace("[CQ:", "［CQ:").strip()[:limit]


def room_label(value):
    value = text(value)
    match = re.search(r"「([^」]{1,15}服)」", value)
    label = match[1] if match else value
    return (label if len(label) <= 18 else label[:17] + "…") or "未命名房间"


def room_heading(room, rooms, kind=""):
    label = room_label(room.get("name"))
    peers = [r for r in rooms if room_label(r.get("name")) == label]
    if len(peers) > 1:
        # Preserve distinct full labels when short names collide. Opaque IDs
        # are only needed if even the portable full names are identical.
        label = text(room.get("name")) or "未命名房间"
        if sum(text(r.get("name")) == text(room.get("name")) for r in peers) > 1:
            label += " · " + text(room.get("id"), 128)
    title = "🏕️ " + label
    if kind == "status":
        title += " · 房间概况\n━━━━━━━━━━━━"
    return title, label


def local_time(value):
    if not isinstance(value, str) or len(value) > 64:
        return None
    try:
        stamp = datetime.fromisoformat(value.replace("Z", "+00:00"))
        if stamp.tzinfo is None or stamp.year < 2000:
            return None
        return stamp.astimezone(BEIJING)
    except (ValueError, OverflowError):
        return None


def time_text(value, *, year=False):
    stamp = local_time(value)
    return (
        stamp.strftime("%Y-%m-%d %H:%M:%S" if year else "%m/%d %H:%M:%S") if stamp else "时间未确认"
    )
