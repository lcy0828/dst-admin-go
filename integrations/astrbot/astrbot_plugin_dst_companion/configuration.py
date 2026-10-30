"""One configuration source; explicit overrides survive AstrBot's default filling."""

import asyncio
import copy
import hashlib
import json
from dataclasses import dataclass
from types import MappingProxyType

from .gateway import QueryError

CATEGORIES = frozenset(
    {
        "status",
        "players",
        "profile",
        "inventory",
        "network",
        "mods",
        "performance",
        "backups",
        "world",
        "help",
    }
)
DEFAULTS = {
    "mode": "mixed",
    "rooms": [],
    "defaultRoom": "",
    "roomAliases": {},
    "commandAliases": {},
    "menuItems": ["*"],
    "disabledCapabilities": [],
    "categories": ["status", "players", "profile", "inventory", "network", "mods", "help"],
    "writeAllowlist": [],
    "bindings": {},
    "pageSize": 5,
    "style": "concise",
    "templates": {},
    "taskNotifications": True,
    "queryTimeout": 8,
    "inventoryTimeout": 30,
    "directoryTTL": 60,
}
OVERRIDE_FIELDS = frozenset(DEFAULTS)


def fingerprint(value):
    return hashlib.sha256(
        json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()


def mapping(value):
    if isinstance(value, str):
        value = json.loads(value)
    if (
        not isinstance(value, dict)
        or len(value) > 64
        or any(
            not isinstance(k, str) or not isinstance(v, str) or len(k) > 128 or len(v) > 256
            for k, v in value.items()
        )
    ):
        raise ValueError("映射必须为最多 64 项的短字符串对象")
    return dict(value)


def validate(values):
    if set(values) != OVERRIDE_FIELDS:
        raise ValueError("配置字段不完整或存在未知字段")
    if values["mode"] not in {"shortcut", "mixed"} or values["style"] not in {
        "concise",
        "detailed",
    }:
        raise ValueError("未知交互或展示模式")
    for field in ("rooms", "categories", "writeAllowlist", "menuItems", "disabledCapabilities"):
        value = values[field]
        if (
            not isinstance(value, list)
            or len(value) > 256
            or any(not isinstance(v, str) or not v or len(v) > 255 for v in value)
            or len(set(value)) != len(value)
        ):
            raise ValueError("列表无效：" + field)
    if not set(values["categories"]) <= CATEGORIES:
        raise ValueError("未知数据类别")
    if (
        not isinstance(values["defaultRoom"], str)
        or values["defaultRoom"]
        and values["defaultRoom"] not in values["rooms"]
    ):
        raise ValueError("默认房间不在开放范围")
    for field in ("roomAliases", "commandAliases", "bindings", "templates"):
        values[field] = mapping(values[field])
    if not set(values["roomAliases"].values()) <= set(values["rooms"]):
        raise ValueError("别名包含未开放房间")
    for field, low, high in (
        ("pageSize", 1, 10),
        ("queryTimeout", 2, 20),
        ("inventoryTimeout", 5, 40),
        ("directoryTTL", 10, 300),
    ):
        if type(values[field]) is not int or not low <= values[field] <= high:
            raise ValueError("预算超限：" + field)
    if type(values["taskNotifications"]) is not bool:
        raise ValueError("通知开关必须是布尔值")
    return values


def freeze(value):
    if isinstance(value, dict):
        return MappingProxyType({k: freeze(v) for k, v in value.items()})
    if isinstance(value, list):
        return tuple(freeze(v) for v in value)
    return value


@dataclass(frozen=True)
class Effective:
    values: object
    origins: object
    revision: str

    def __getitem__(self, key):
        return self.values[key]


class Configuration:
    def __init__(self, source):
        self.source = source

    def effective(self, session):
        source = self.source
        if type(source.get("configuration_version", 1)) is not int or source.get(
            "configuration_version", 1
        ) not in {1, 2}:
            raise ValueError("未知配置版本")
        values = copy.deepcopy(DEFAULTS)
        if source.get("configuration_version", 1) == 1:
            # Preserve the old mixed-chat and explicit native administrator rules.
            values.update(
                mode="mixed",
                rooms=list(source.get("room_ids", [])),
                defaultRoom=source.get("default_room_id", ""),
                roomAliases=mapping(source.get("room_aliases", {})),
                bindings=mapping(source.get("player_bindings", {})),
                categories=list(source.get("public_queries", DEFAULTS["categories"])),
                writeAllowlist=[
                    "room.backup.create",
                    "room.start",
                    "room.stop",
                    "room.restart",
                    "player.kick",
                ],
            )
        else:
            supplied = source.get("defaults", {})
            if not isinstance(supplied, dict) or set(supplied) - OVERRIDE_FIELDS:
                raise ValueError("全局默认配置无效")
            values.update(copy.deepcopy(supplied))
        values = validate(values)
        global_values = copy.deepcopy(values)
        origins = {k: "global" for k in DEFAULTS}
        seen = set()
        overrides = source.get("session_overrides", [])
        if not isinstance(overrides, list) or len(overrides) > 64:
            raise ValueError("会话覆盖列表无效")
        for item in overrides:
            if (
                not isinstance(item, dict)
                or not isinstance(item.get("session"), str)
                or item["session"] in seen
            ):
                raise ValueError("无效或重复的会话配置")
            seen.add(item["session"])
            if item["session"] != session:
                continue
            fields = item.get("overrideFields", [])
            if (
                not isinstance(fields, list)
                or any(not isinstance(k, str) for k in fields)
                or len(fields) != len(set(fields))
                or not set(fields) <= OVERRIDE_FIELDS
                or not isinstance(item.get("values"), dict)
            ):
                raise ValueError("覆盖字段无效")
            for key in fields:
                if key not in item["values"]:
                    raise ValueError("缺少覆盖值：" + key)
                values[key] = copy.deepcopy(item["values"][key])
                origins[key] = "session"
        validate(values)
        # A session can narrow the global maximum, never expand authority.
        for key in ("rooms", "categories", "writeAllowlist"):
            if not set(values[key]) <= set(global_values[key]):
                raise ValueError("会话配置超出全局授权：" + key)
        if not set(global_values["disabledCapabilities"]) <= set(values["disabledCapabilities"]):
            raise ValueError("会话不能移除全局禁用的能力")
        return Effective(freeze(values), freeze(origins), fingerprint(values))

    def preview(self, session):
        result = self.effective(session)
        return {
            "revision": result.revision,
            "origins": dict(result.origins),
            "mode": result["mode"],
            "rooms": list(result["rooms"]),
            "categories": list(result["categories"]),
            "writeAllowlist": list(result["writeAllowlist"]),
            "pageSize": result["pageSize"],
        }


class ConfigEditor:
    """Optimistic content check within this plugin; no claim of global WebUI CAS."""

    def __init__(self, source, save):
        self.source, self.save = source, save
        self.lock = asyncio.Lock()

    async def apply(self, candidate, expected, sessions):
        async with self.lock:
            if fingerprint(dict(self.source)) != expected:
                raise QueryError("CONFIG_CONFLICT")
            for session in sessions:
                Configuration(candidate).effective(session)
            previous = copy.deepcopy(dict(self.source))
            self.source.clear()
            self.source.update(copy.deepcopy(candidate))
            try:
                committed = await self.save()
            except Exception:
                # Never restore authority after another editor has changed it.
                if fingerprint(dict(self.source)) == fingerprint(candidate):
                    self.source.clear()
                    self.source.update(previous)
                raise
            if committed is not True or fingerprint(dict(self.source)) != fingerprint(candidate):
                raise QueryError("CONFIG_CONFLICT")
            return fingerprint(candidate)


def safe_reload(manager, plugin_name, instances):
    """Return only a verified target; never invoke reload(None/unknown)."""
    if not plugin_name or plugin_name not in instances or not instances[plugin_name]:
        raise QueryError("PLUGIN_TARGET_NOT_FOUND")
    return manager.reload(plugin_name)


def migrate_v2(source):
    """Preview-only migration; preserve legacy authority and mixed interaction."""
    candidate = copy.deepcopy(dict(source))
    if candidate.get("configuration_version", 1) == 2:
        return candidate
    global_source = {**candidate, "session_overrides": []}
    effective = Configuration(global_source).effective("__migration_global__")

    def thaw(value):
        if isinstance(value, MappingProxyType):
            return {k: thaw(v) for k, v in value.items()}
        if isinstance(value, tuple):
            return [thaw(v) for v in value]
        return value

    candidate["defaults"] = thaw(effective.values)
    candidate["configuration_version"] = 2
    for session in candidate.get("allowed_sessions", []):
        Configuration(candidate).effective(str(session))
    return candidate
