"""Bounded per-user state; message IDs are adapter receipts, never text hashes."""

import json
import time
from collections import OrderedDict
from dataclasses import asdict, dataclass, is_dataclass


@dataclass(frozen=True)
class Target:
    room: str
    player: str = ""
    name: str = ""


class State:
    def __init__(self, clock=time.monotonic, capacity=2000):
        self.clock, self.capacity = clock, capacity
        self.users, self.messages, self.choices, self.seen = (OrderedDict() for _ in range(4))
        self.forms = OrderedDict()
        self.tables = {
            id(t): t for t in (self.users, self.messages, self.choices, self.seen, self.forms)
        }
        self.allocations = OrderedDict()
        self.bytes = 0
        self.next_sequence = 0
        self.maximum_bytes = 4 * 1024 * 1024

    def _put(self, table, key, value):
        allocation = (id(table), key)

        def encode(v):
            return asdict(v) if is_dataclass(v) else str(v)

        size = len(json.dumps([key, value], ensure_ascii=False, default=encode).encode())
        self.bytes -= self.allocations.pop(allocation, 0)
        self.allocations[allocation] = size
        self.bytes += size
        table[key] = value
        table.move_to_end(key)
        while len(table) > self.capacity:
            removed, _ = table.popitem(last=False)
            self.bytes -= self.allocations.pop((id(table), removed), 0)
        while self.bytes > self.maximum_bytes:
            (table_id, removed), used = self.allocations.popitem(last=False)
            self.tables[table_id].pop(removed, None)
            self.bytes -= used

    def begin(self, key):
        previous = self.users.get(key, {})
        self.clear_form(key)
        self.next_sequence += 1
        sequence = self.next_sequence
        if key in self.choices:
            self.choices.pop(key)
            self.bytes -= self.allocations.pop((id(self.choices), key), 0)
        self._put(
            self.users,
            key,
            {
                "sequence": sequence,
                "target": None,
                "evidence": previous.get("evidence", []),
                "expires": previous.get("expires", 0),
            },
        )
        return sequence

    def remember(self, key, sequence, target, summary):
        current = self.users.get(key)
        if not current or current["sequence"] != sequence:
            return
        # A latest unresolved request blocks pronouns; an older response never
        # changes the latest target.
        evidence = current["evidence"][-7:] + [
            {
                "roomId": target.room,
                "playerId": target.player,
                "name": target.name,
                "summary": summary[:320],
                "recordedAt": time.time(),
            }
        ]
        self._put(
            self.users,
            key,
            {
                "sequence": sequence,
                "target": target,
                "expires": self.clock() + 600,
                "evidence": evidence,
            },
        )

    def suspend(self, key, sequence):
        if self.users.get(key, {}).get("sequence") == sequence:
            self.users[key]["target"] = None

    def target(self, key):
        value = self.users.get(key, {})
        return value.get("target") if value.get("expires", 0) > self.clock() else None

    def evidence(self, key):
        return self.users.get(key, {}).get("evidence", []) if self.target(key) else []

    def associate(self, scope, message_id, target):
        if message_id and not isinstance(message_id, bool):
            self._put(self.messages, (*scope, str(message_id)), (self.clock() + 86400, target))

    def quoted(self, scope, message_id):
        value = self.messages.get((*scope, str(message_id)))
        return value[1] if value and value[0] > self.clock() else None

    def choose(self, key, intent, candidates, room, sequence, mode="target", page_size=5):
        if self.users.get(key, {}).get("sequence") != sequence:
            return False
        self._put(
            self.choices,
            key,
            {
                "until": self.clock() + 120,
                "intent": intent,
                "candidates": [
                    {
                        k: p[k]
                        for k in (
                            "id",
                            "name",
                            "online",
                            "presenceStatus",
                            "worldConfirmed",
                            "worldName",
                            "kind",
                            "capability",
                        )
                        if k in p
                    }
                    for p in candidates[:256]
                ],
                "mode": mode,
                "page": 0,
                "pageSize": page_size,
                "room": room,
                "sequence": sequence,
                "messageId": "",
            },
        )
        return True

    def selection(self, key, text, quote, woken):
        value = self.choices.get(key)
        if not value or value["until"] <= self.clock() or not text.isdigit():
            return None
        if not (woken or quote and quote == value["messageId"]):
            return None
        index = value.get("page", 0) * value.get("pageSize", 5) + int(text) - 1
        if not 1 <= int(text) <= value.get("pageSize", 5) or not 0 <= index < len(
            value["candidates"]
        ):
            return None
        del self.choices[key]
        self.bytes -= self.allocations.pop((id(self.choices), key), 0)
        return value, index

    def duplicate(self, scope, message_id):
        if not message_id:
            return False
        key = (*scope, str(message_id))
        now = self.clock()
        if self.seen.get(key, 0) > now:
            return True
        self._put(self.seen, key, now + 600)
        return False

    def choice_sent(self, key, message_id, sequence):
        if self.choices.get(key, {}).get("sequence") == sequence:
            value = dict(self.choices[key])
            value["messageId"] = (
                str(message_id) if message_id and not isinstance(message_id, bool) else ""
            )
            self._put(self.choices, key, value)

    def clear_form(self, key):
        self.forms.pop(key, None)
        self.bytes -= self.allocations.pop((id(self.forms), key), 0)

    def form(self, key, value):
        self._put(self.forms, key, {**value, "until": self.clock() + 120})

    def pending_form(self, key):
        value = self.forms.get(key)
        if value and value["until"] > self.clock():
            return value
        self.clear_form(key)
        return None

    def handles(self, key, text, quote, woken):
        value = self.choices.get(key)
        if (
            value
            and value["until"] > self.clock()
            and (woken or quote and quote == value["messageId"])
            and (text.isdigit() or text in {"下一页", "上一页"})
        ):
            return True
        return bool(woken and self.pending_form(key))

    def page(self, key, direction):
        value = self.choices.get(key)
        if not value or value["until"] <= self.clock():
            return None
        count = max(1, (len(value["candidates"]) + value["pageSize"] - 1) // value["pageSize"])
        value["page"] = max(0, min(count - 1, value["page"] + direction))
        return value
