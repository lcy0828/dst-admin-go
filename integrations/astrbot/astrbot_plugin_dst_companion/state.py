"""Bounded per-user state; message IDs are adapter receipts, never text hashes."""

import json
import re
import time
from collections import OrderedDict
from dataclasses import asdict, dataclass, field, is_dataclass

from .display import REFERENCE_TTL


@dataclass(frozen=True)
class Target:
    room: str
    player: str = ""
    name: str = ""
    # explicit | quote | focus | only_online — shown in receipts, not identity.
    source: str = field(default="", compare=False)


def player_prompt(form):
    return bool(form and (form.get("type") == "player" or
        form.get("type") == "parameter" and form.get("item", {}).get("parameterHints", {}).get(form.get("field")) == "player"))


class State:
    def __init__(self, clock=time.monotonic, capacity=2000):
        self.clock, self.capacity = clock, capacity
        self.users, self.messages, self.choices, self.seen = (OrderedDict() for _ in range(4))
        self.forms = OrderedDict()
        self.requests = OrderedDict()
        self.reply_hashes = OrderedDict()
        self.tables = {
            id(t): t for t in (self.users, self.messages, self.choices, self.seen, self.forms, self.requests, self.reply_hashes)
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

    def associate(self, scope, message_id, target, *, source=""):
        if message_id and not isinstance(message_id, bool):
            self._put(
                self.messages, (*scope, str(message_id)), (self.clock() + REFERENCE_TTL, target, source)
            )

    def quoted(self, scope, message_id, *, source=None):
        value = self.messages.get((*scope, str(message_id)))
        return (
            value[1]
            if value and value[0] > self.clock() and (source is None or value[2] == source)
            else None
        )

    def associate_text(self, scope, text, target, *, source=""):
        if isinstance(text, str) and text.strip() and target and target.player:
            self._put(
                self.reply_hashes,
                (*scope, text.strip()[:3500]),
                (self.clock() + REFERENCE_TTL, target, source),
            )

    def quoted_text(self, scope, text, *, source=None):
        if not isinstance(text, str) or not text.strip():
            return None
        value = self.reply_hashes.get((*scope, text.strip()[:3500]))
        return (
            value[1]
            if value and value[0] > self.clock() and (source is None or value[2] == source)
            else None
        )

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
                            "worldId",
                            "menuDescription",
                            "x",
                            "z",
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
        if not self.choice_matches(key, quote) or not self.number(text):
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

    def form_sent(self, key, message_id, form):
        if form is not None and self.forms.get(key) is form and message_id:
            self._put(self.forms, key, {**form, "messageId": str(message_id)})

    def clear_form(self, key):
        self.forms.pop(key, None)
        self.bytes -= self.allocations.pop((id(self.forms), key), 0)

    def claim_request(self, key, message_id):
        if message_id and self.requests.get(key) != str(message_id):
            current = self.users.get(key)
            if current:
                # Invalidate outstanding async results while retaining the
                # prompt/choice that this new message may be answering.
                self.next_sequence += 1
                self._put(self.users, key, {**current, "sequence": self.next_sequence})
            self._put(self.requests, key, str(message_id))

    def current_request(self, key, message_id):
        return not message_id or self.requests.get(key, str(message_id)) == str(message_id)

    def form(self, key, value, sequence=None):
        if sequence is not None and self.users.get(key, {}).get("sequence") != sequence:
            return False
        self._put(self.forms, key, {**value, "until": self.clock() + 120})
        return True

    def pending_form(self, key):
        value = self.forms.get(key)
        if value and value["until"] > self.clock():
            return value
        self.clear_form(key)
        return None

    def handles(self, key, text, quote, woken):
        from .routing import is_cancel_reply
        from .menus import request as menu_request

        if self.choice_matches(key, quote) and (
            self.number(text) or text in {"下一页", "上一页"} or is_cancel_reply(text)
            or self.choices[key].get("mode") == "menu" and menu_request(text) is not None
        ):
            return True
        form = self.pending_form(key)
        if not form:
            return False
        if quote and quote != form.get("messageId"):
            from .routing import form_reply

            from .decision import looks_like_confirmation

            return bool(woken and (
                form.get("type") == "confirmation" and looks_like_confirmation(text)
                or player_prompt(form) and form_reply(text) is not None))
        if form.get("type") == "confirmation":
            from .decision import looks_like_confirmation

            if looks_like_confirmation(text):
                return True
        if woken or quote and quote == form.get("messageId"):
            from .routing import form_reply

            return is_cancel_reply(text) or form_reply(text) is not None
        if quote:
            return False
        if is_cancel_reply(text):
            return True
        # Supplying a player completes this user's already-requested action.
        # The engine rechecks permissions; dangerous actions still confirm.
        from .routing import COMPOUND, DISCUSSION, explicit_ai, parse

        return bool(
            player_prompt(form)
            and 0 < len(text) <= 128
            and not (DISCUSSION.search(text) or COMPOUND.search(text) or explicit_ai(text))
            and parse(text, {}) is None
        )

    @staticmethod
    def number(text):
        return re.fullmatch(r"[0-9]{1,3}", text) is not None

    def choice_matches(self, key, quote):
        value = self.choices.get(key)
        return bool(
            value and value["until"] > self.clock() and (not quote or quote == value["messageId"])
        )

    def page(self, key, direction):
        value = self.choices.get(key)
        if not value or value["until"] <= self.clock():
            return None
        count = max(1, (len(value["candidates"]) + value["pageSize"] - 1) // value["pageSize"])
        value["page"] = max(0, min(count - 1, value["page"] + direction))
        return value
