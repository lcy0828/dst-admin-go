"""Trusted QQ roles and capability grants. Never infer authority from model input."""

import asyncio
import copy
import time
from dataclasses import dataclass

from .configuration import CATEGORIES, fingerprint
from .gateway import QueryError
from .sessions import group_origin

PLAYER_ACTIONS = ["player.resurrect", "player.give_item", "player.kick"]
ROOM_ACTIONS = [
    "room.start",
    "room.stop",
    "room.restart",
    "room.save",
    "room.backup.create",
    "room.backups.configure",
]
DEFAULT_RULES = {
    "member": {"read": sorted(CATEGORIES), "write": [], "workbench": "off", "deny": []},
    "admin": {"read": sorted(CATEGORIES), "write": PLAYER_ACTIONS, "workbench": "off", "deny": []},
    "owner": {
        "read": sorted(CATEGORIES),
        "write": PLAYER_ACTIONS + ROOM_ACTIONS,
        "workbench": "standard",
        "deny": [],
    },
}
LEVELS = {"off": 0, "standard": 1, "all": 2}


def enabled(config):
    # Missing field preserves the policy of existing installations.
    return config.get("permission_mode", "legacy") == "group"


def validate_rules(value):
    if not isinstance(value, dict) or set(value) - set(DEFAULT_RULES):
        raise ValueError("群权限只能设置 member、admin、owner")
    result = copy.deepcopy(DEFAULT_RULES)
    for role, rule in value.items():
        if not isinstance(rule, dict) or set(rule) - {"read", "write", "workbench", "deny"}:
            raise ValueError("群权限包含未知设置")
        result[role].update(copy.deepcopy(rule))
    for rule in result.values():
        for key in ("read", "write", "deny"):
            rows = rule[key]
            if (
                not isinstance(rows, list)
                or len(rows) > 256
                or any(not isinstance(v, str) or not v or len(v) > 128 for v in rows)
                or len(set(rows)) != len(rows)
            ):
                raise ValueError("群权限列表无效")
        if not set(rule["read"]) <= CATEGORIES or rule["workbench"] not in LEVELS:
            raise ValueError("群权限类型无效")
    return result


def grants(config, record, role):
    global_rules = validate_rules(config.get("group_permissions", {}))
    overrides = record.get("rolePermissions", {}) if record else {}
    if not isinstance(overrides, dict) or set(overrides) - set(DEFAULT_RULES):
        raise ValueError("本群权限无效")
    merged = copy.deepcopy(global_rules)
    for name, rule in overrides.items():
        if not isinstance(rule, dict):
            raise ValueError("本群权限无效")
        merged[name].update(rule)
    rules = validate_rules(merged)
    ceiling = global_rules["owner"]
    for rule in rules.values():
        if (
            not set(rule["read"]) <= set(ceiling["read"])
            or not set(rule["write"]) <= set(ceiling["write"])
            or LEVELS[rule["workbench"]] > LEVELS[ceiling["workbench"]]
        ):
            raise ValueError("本群权限超出全局群主授权范围")
    rule = rules.get(role, rules["member"])
    deny = (
        set(ceiling["deny"])
        | set(global_rules.get(role, global_rules["member"])["deny"])
        | set(rule["deny"])
    )
    return {**rule, "write": sorted(set(rule["write"]) - deny), "deny": sorted(deny)}, fingerprint(
        rules
    )


@dataclass(frozen=True)
class Observation:
    role: str
    at: float


class GroupRoles:
    """Bounded, demand-driven role reads; management submissions force a fresh read."""

    def __init__(self, clock=time.monotonic):
        self.clock = clock
        self.rows = {}
        self.pending = {}
        self.limit = asyncio.Semaphore(2)

    @staticmethod
    def key(event):
        return (
            str(event.get_platform_id()),
            str(event.message_obj.self_id),
            group_origin(event.unified_msg_origin),
            str(event.get_sender_id()),
        )

    def current(self, event):
        if event.is_admin():
            return "maintainer"
        row = self.rows.get(self.key(event))
        return row.role if row and row.role in DEFAULT_RULES and self.clock() - row.at < 30 else "member"

    def verified(self, event):
        if event.is_admin():
            return True
        row = self.rows.get(self.key(event))
        return bool(row and row.role in DEFAULT_RULES and self.clock() - row.at < 30)

    async def refresh(self, event, *, force=False):
        if event.is_admin():
            return "maintainer"
        key = self.key(event)
        if (
            event.is_private_chat()
            or event.get_platform_name() != "aiocqhttp"
            or not str(event.get_group_id()).isdigit()
            or not key[-1].isdigit()
        ):
            return "member"
        row = self.rows.get(key)
        if not force and row and row.role in DEFAULT_RULES and self.clock() - row.at < 30:
            return row.role
        task = self.pending.get(key)
        if task is None:
            if len(self.pending) >= 32:
                raise QueryError("IDENTITY_BUSY")
            task = asyncio.create_task(self._read(event, key))
            self.pending[key] = task

            def finish(done):
                if self.pending.get(key) is done:
                    self.pending.pop(key, None)
                if not done.cancelled():
                    done.exception()

            task.add_done_callback(finish)
        try:
            return await asyncio.shield(task)
        finally:
            if task.done() and self.pending.get(key) is task:
                self.pending.pop(key, None)

    async def _read(self, event, key):
        # Invalidate first: failed refresh must not extend a previous admin grant.
        self.rows.pop(key, None)
        try:
            async with asyncio.timeout(4), self.limit:
                value = await event.bot.call_action(
                    "get_group_member_info",
                    group_id=int(event.get_group_id()),
                    user_id=int(event.get_sender_id()),
                    no_cache=True,
                )
            if (
                not isinstance(value, dict)
                or str(value.get("user_id")) != key[-1]
                or str(value.get("group_id")) != str(event.get_group_id())
                or value.get("role") not in DEFAULT_RULES
            ):
                raise ValueError("invalid group member identity")
            role = value["role"]
        except asyncio.CancelledError:
            self.rows.pop(key, None)
            raise
        except Exception:
            self.rows[key] = Observation("unknown", self.clock())
            raise QueryError("GROUP_IDENTITY_UNAVAILABLE") from None
        if len(self.rows) >= 512:
            self.rows.pop(next(iter(self.rows)))
        self.rows[key] = Observation(role, self.clock())
        return role

    async def close(self):
        tasks = list(self.pending.values())
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        self.pending.clear()
        self.rows.clear()
