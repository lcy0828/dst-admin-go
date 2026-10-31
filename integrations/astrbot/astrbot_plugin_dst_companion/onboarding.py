"""Bounded, demand-driven room discovery and per-session enrollment."""

import asyncio
import copy
import time

from .configuration import Effective, fingerprint, freeze
from .gateway import QueryError
from .sessions import canonical_records


class RoomCatalog:
    def __init__(self, read, identity, clock=time.monotonic):
        self.read, self.identity, self.clock = read, identity, clock
        self.rows, self.identity_value, self.until = [], "", 0
        self.lock = asyncio.Lock()

    def valid(self):
        return self.identity_value == self.identity() and self.clock() < self.until

    async def refresh(self, force=False):
        async with self.lock:
            if not force and self.valid():
                return self.rows
            identity = self.identity()
            self.until = 0  # Failure never extends old authority.
            result = await self.read("api_listRooms", {})
            items = result.get("items")
            if not isinstance(items, list) or result.get("_aiTruncated") or result.get("hasMore"):
                raise QueryError("ROOM_CATALOG_INCOMPLETE")
            if (
                len(items) > 256
                or type(result.get("total")) is int
                and result["total"] > len(items)
            ):
                raise QueryError("ROOM_CATALOG_INCOMPLETE")
            rows, ids = [], set()
            for item in items:
                room = item.get("id") if isinstance(item, dict) else None
                if not isinstance(room, str) or not room or len(room) > 255 or room in ids:
                    raise QueryError("ROOM_CATALOG_INCOMPLETE")
                ids.add(room)
                rows.append({"id": room, "name": str(item.get("name") or room)[:255]})
            if identity != self.identity():
                raise QueryError("CONFIG_CHANGED")
            self.rows, self.identity_value, self.until = rows, identity, self.clock() + 60
            return self.rows


class Enrollment:
    def __init__(self, save):
        self.save = save
        self.records = {}
        self.lock = asyncio.Lock()
        self.loaded = False

    async def load(self, load):
        value = await load("sessions_v3", {})
        if not isinstance(value, dict) or len(value) > 64:
            raise ValueError("invalid saved sessions")
        for session, record in value.items():
            self.validate(session, record)
        normalized = canonical_records(value)
        if normalized != value:
            await self.save("sessions_v3_before_group_normalization", value)
            await self.save("sessions_v3", normalized)
            value = normalized
        self.records = copy.deepcopy(value)
        self.loaded = True

    async def update(self, session, value):
        async with self.lock:
            self.validate(session, value)
            candidate = copy.deepcopy(self.records)
            if len(candidate) >= 64 and session not in candidate:
                raise QueryError("SESSION_LIMIT")
            candidate[session] = copy.deepcopy(value)
            await self.save("sessions_v3", candidate)
            self.records = candidate  # Do not grant anything before persistence succeeds.

    async def update_templates(self, session, rows, expected):
        from .message_templates import revision, validate_templates

        validate_templates(rows)
        async with self.lock:
            record = self.records.get(session)
            if not record or not record.get("enabled"):
                raise QueryError("SESSION_DISABLED")
            if revision(record.get("messageTemplates", [])) != expected:
                raise QueryError("CONFIG_CONFLICT")
            candidate = copy.deepcopy(self.records)
            candidate[session]["messageTemplates"] = copy.deepcopy(rows)
            committed = await self.save("sessions_v3", candidate)
            if committed is False:
                raise QueryError("CONFIG_SAVE_FAILED")
            self.records = candidate

    @staticmethod
    def validate(session, record):
        if (
            not isinstance(session, str)
            or len(session) > 255
            or len(session.split(":")) != 3
            or not isinstance(record, dict)
            or type(record.get("enabled")) is not bool
        ):
            raise ValueError("invalid session")
        if not record["enabled"]:
            return
        rooms = record.get("rooms")
        if rooms != "all" and (
            not isinstance(rooms, list)
            or len(rooms) > 256
            or any(not isinstance(r, str) or not r or len(r) > 255 for r in rooms)
        ):
            raise ValueError("invalid room scope")
        if (
            type(record.get("query")) is not bool
            or not isinstance(record.get("source"), str)
            or len(record["source"]) > 4096
        ):
            raise ValueError("invalid session source")
        flags = record.get("notices", {})
        if (
            not isinstance(flags, dict)
            or set(flags) - {"presence", "server", "chat"}
            or any(type(v) is not bool for v in flags.values())
        ):
            raise ValueError("invalid notice settings")
        if record.get("noticeOwner", "unified") not in {"unified", "pending"}:
            raise ValueError("invalid notice owner")


def dynamic_effective(base, rows, selection):
    values = {k: thaw(v) for k, v in base.values.items()}
    allowed = {r["id"] for r in rows}
    if selection != "all":
        allowed &= set(selection)
    values["rooms"] = sorted(allowed)
    values["roomAliases"] = {k: v for k, v in values["roomAliases"].items() if v in allowed}
    from collections import Counter

    from .commands import BY_PHRASE

    names = Counter(r["name"] for r in rows if r["id"] in allowed)
    for row in sorted(rows, key=lambda r: r["id"]):
        name = row["name"]
        if (
            row["id"] in allowed
            and names[name] == 1
            and 2 <= len(name) <= 64
            and not name.isdigit()
            and name not in BY_PHRASE
            and len(values["roomAliases"]) < 64
        ):
            values["roomAliases"].setdefault(name, row["id"])
    if values["defaultRoom"] not in allowed:
        values["defaultRoom"] = ""
    # Use the panel's real names. List order is never turned into room aliases.
    return Effective(freeze(values), base.origins, fingerprint(values))


def thaw(value):
    from collections.abc import Mapping

    if isinstance(value, Mapping):
        return {k: thaw(v) for k, v in value.items()}
    if isinstance(value, tuple):
        return [thaw(v) for v in value]
    return copy.deepcopy(value)
