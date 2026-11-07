"""Bounded, request-driven protocol v1 directory and fixed endpoint execution."""

import asyncio
import copy
import json
import math
import re
import time
import uuid
import weakref
from collections import OrderedDict

from .gateway import QueryError

KNOWN_CATEGORIES = frozenset(
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
        "management",
    }
)
ID = re.compile(r"[a-z][a-z0-9_.-]{0,63}\Z")
TOOL = re.compile(r"api_[A-Za-z][A-Za-z0-9]{0,100}\Z")
SCHEMA_KEYS = {
    "type",
    "properties",
    "required",
    "additionalProperties",
    "items",
    "enum",
    "minimum",
    "maximum",
    "minLength",
    "maxLength",
    "minItems",
    "maxItems",
    "description",
    "title",
    "default",
    "pattern",
    "format",
}


def compatible(schema, depth=0):
    try:
        return _compatible(schema, depth)
    except (TypeError, ValueError, OverflowError):
        return False


def _compatible(schema, depth=0):
    if depth > 4 or not isinstance(schema, dict) or set(schema) - SCHEMA_KEYS:
        return False
    kind = schema.get("type")
    if kind not in {"object", "array", "string", "integer", "number", "boolean"}:
        return False
    if "enum" in schema and (
        not isinstance(schema["enum"], list) or not 1 <= len(schema["enum"]) <= 64
    ):
        return False
    # No downloaded refs or executable regex supplied by the directory. The
    # client validates types/bounds; the server alone enforces anchored patterns.
    if "pattern" in schema and (
        not isinstance(schema["pattern"], str) or len(schema["pattern"]) > 256
    ):
        return False
    if schema.get("format") not in {None, "date-time"}:
        return False
    if kind == "object":
        props = schema.get("properties", {})
        required = schema.get("required", [])
        return (
            isinstance(props, dict)
            and len(props) <= 32
            and isinstance(required, list)
            and set(required) <= set(props)
            and schema.get("additionalProperties") is False
            and all(compatible(v, depth + 1) for v in props.values())
        )
    if kind == "array":
        return (
            type(schema.get("maxItems")) is int
            and 0 <= schema["maxItems"] <= 64
            and compatible(schema.get("items"), depth + 1)
        )
    if kind == "string":
        return (
            type(schema.get("maxLength")) is int
            and 0 < schema["maxLength"] <= 1024
            or isinstance(schema.get("enum"), list)
            or "pattern" in schema
        )
    return True


def validate(value, schema):
    kind = schema["type"]
    valid = {
        "string": isinstance(value, str),
        "integer": type(value) is int,
        "number": type(value) in {int, float} and math.isfinite(value),
        "boolean": type(value) is bool,
        "array": isinstance(value, list),
        "object": isinstance(value, dict),
    }[kind]
    if not valid or "enum" in schema and value not in schema["enum"]:
        raise QueryError("PARAMETER_INVALID")
    if kind in {"integer", "number"}:
        if value < schema.get("minimum", -math.inf) or value > schema.get("maximum", math.inf):
            raise QueryError("PARAMETER_INVALID")
    if kind == "string" and not schema.get("minLength", 0) <= len(value) <= schema.get(
        "maxLength", 1024
    ):
        raise QueryError("PARAMETER_INVALID")
    if kind == "object":
        props = schema.get("properties", {})
        if set(value) - set(props) or not set(schema.get("required", [])) <= set(value):
            raise QueryError("PARAMETER_INVALID")
        for k, v in value.items():
            validate(v, props[k])
    if kind == "array":
        if not schema.get("minItems", 0) <= len(value) <= schema["maxItems"]:
            raise QueryError("PARAMETER_INVALID")
        for v in value:
            validate(v, schema["items"])
    return value


def parse_parameter(text, schema):
    try:
        value = text if schema["type"] == "string" else json.loads(text)
        return validate(value, schema)
    except (ValueError, TypeError):
        raise QueryError("PARAMETER_INVALID") from None


def permitted(actor, item, parameters=None):
    from .interactions import effective_risk
    risk=effective_risk(item,parameters)
    if parameters is None and risk=="critical" and actor.workbench_mode=="standard":
        # Directory visibility is not execution authorization. Each submission
        # below rechecks the selected code-owned action, including phase.
        if any(isinstance(a,dict) and a.get("risk") in {"low","medium","high"} for a in item.get("actions",[])):
            risk="high"
    if item.get("id") in actor.disabled_capabilities:
        return False
    if item.get("category") not in KNOWN_CATEGORIES:
        return False
    if item.get("access") == "write":
        if "*" in actor.write_allowlist:
            # Simple policy: administrators may run every published write.
            # How it is confirmed is decided by decision.confirmation().
            return actor.role in {"game-manage", "panel-manage"}
        workbench = (
            item.get("resultKind") == "workbench-run"
            and (
                item.get("id", "").startswith("workbench.") or item.get("id") == "player.resurrect"
            )
            and item.get("risk") in {"low", "medium", "high", "critical"}
            and actor.workbench_mode in {"standard", "all"}
            and (risk != "critical" or actor.workbench_mode == "all")
        )
        return actor.role in {"game-manage", "panel-manage"} and (
            item["id"] in actor.write_allowlist or workbench
            or item["id"] in {"player.teleport", "location.save"} and actor.workbench_mode in {"standard", "all"}
        )
    return item.get("access") == "read" and item.get("category") in actor.visibility


class Directory:
    def __init__(self, gateway, clock=time.monotonic, version=lambda: 1, receipts=None):
        self.gateway, self.clock = gateway, clock
        self.version = version
        self.receipts = receipts
        self.unsupported = OrderedDict()
        self.cache = OrderedDict()
        self.locks = weakref.WeakValueDictionary()
        self.fetch_one = None
        self.selected = OrderedDict()

    def seed(self, actor, item):
        if (not isinstance(item, dict) or item.get("contractVersion") != 1
                or not isinstance(item.get("schemaRevision"), str) or not compatible(item.get("parameters"))
                or not ID.fullmatch(str(item.get("id", "")))
                or not isinstance(item.get("nativeTools"), list)
                or not all(isinstance(n, str) and TOOL.fullmatch(n) for n in item["nativeTools"])):
            raise QueryError("CAPABILITY_INCOMPATIBLE")
        key = actor.read_projection, item["id"]
        self.selected[key] = (self.clock()+60, copy.deepcopy(item))
        self.selected.move_to_end(key)
        while len(self.selected)>32:
            self.selected.popitem(last=False)

    async def get(self, actor, capability, deadline):
        if self.fetch_one:
            result = await self.fetch_one(actor, capability)
            if result is not NotImplemented:
                return result
        rows = await self.items(actor, deadline)
        return next((c for c in rows if c["id"] == capability), None)

    async def items(self, actor, deadline, ttl=None, force=False):
        ttl = actor.directory_ttl if ttl is None else ttl
        key = actor.read_projection
        old = self.cache.get(key)
        if old and old.get("protocolVersion", 1) != self.version():
            old = None
        if old and not force and self.clock() - old["checked"] < ttl:
            return copy.deepcopy(old["items"])
        lock = self.locks.get(key)
        if lock is None:
            if len(self.locks) >= 64:
                raise QueryError("PANEL_BUSY")
            lock = asyncio.Lock()
            self.locks[key] = lock
        async with lock:
            old = self.cache.get(key)
            if old and old.get("protocolVersion", 1) != self.version():
                old = None
            if old and not force and self.clock() - old["checked"] < ttl:
                return copy.deepcopy(old["items"])
            version = self.version()
            args = {"query": {"protocolVersion": version, "limit": 32}}
            if old:
                args["query"]["knownRevision"] = old["revision"]
            items, revision, cursors, budget = [], "", set(), 0
            for _ in range(8):
                page = await self.gateway.call(actor, "api_listBotCapabilities", args, deadline)
                if version != self.version():
                    raise QueryError("DIRECTORY_CHANGED")
                if page.get("protocolVersion") != version or not isinstance(page.get("revision"), str):
                    raise QueryError("CAPABILITY_INCOMPATIBLE")
                if page.get("notModified") is True:
                    if not old or page["revision"] != old["revision"] or items:
                        raise QueryError("MCP_RESPONSE_INVALID")
                    self.cache[key] = {**old, "checked": self.clock()}
                    return copy.deepcopy(old["items"])
                if revision and revision != page["revision"]:
                    raise QueryError("DIRECTORY_CHANGED")
                revision = page["revision"]
                rows = page.get("items")
                if not isinstance(rows, list) or len(rows) > 64:
                    raise QueryError("MCP_RESPONSE_INVALID")
                budget += len(json.dumps(page).encode())
                if budget > 1 << 20 or len(items) + len(rows) > 256:
                    raise QueryError("DIRECTORY_TOO_LARGE")
                items.extend(rows)
                cursor = page.get("nextCursor")
                if not cursor:
                    break
                if not isinstance(cursor, str) or len(cursor) > 512 or cursor in cursors:
                    raise QueryError("MCP_RESPONSE_INVALID")
                cursors.add(cursor)
                args = {"query": {"protocolVersion": version, "limit": 32, "cursor": cursor}}
            else:
                raise QueryError("DIRECTORY_TOO_LARGE")
            accepted, rejected, ids = [], [], set()
            for item in items:
                if (
                    not isinstance(item, dict)
                    or not ID.fullmatch(str(item.get("id", "")))
                    or item["id"] in ids
                ):
                    raise QueryError("MCP_RESPONSE_INVALID")
                ids.add(item["id"])
                native = item.get("nativeTools")
                if (
                    item.get("contractVersion") == 1
                    and isinstance(item.get("schemaRevision"), str)
                    and isinstance(native, list)
                    and 1 <= len(native) <= 8
                    and all(isinstance(n, str) and TOOL.fullmatch(n) for n in native)
                    and compatible(item.get("parameters"))
                    and item.get("access") in {"read", "write"}
                    and item.get("category") in KNOWN_CATEGORIES
                ):
                    accepted.append(copy.deepcopy(item))
                else:
                    rejected.append({"id": item["id"], "title": str(item.get("title", ""))[:128],
                                     "access": item.get("access"), "category": item.get("category"),
                                     "status": "client_update_required"})
            self.unsupported[key] = rejected
            self.unsupported.move_to_end(key)
            while len(self.unsupported) > 64:
                self.unsupported.popitem(last=False)
            if version != self.version():
                raise QueryError("DIRECTORY_CHANGED")
            self.cache[key] = {"revision": revision, "items": accepted, "checked": self.clock(), "protocolVersion": version}
            self.cache.move_to_end(key)
            while len(self.cache) > 64:
                self.cache.popitem(last=False)
            return copy.deepcopy(accepted)

    def lookup(self, actor, capability):
        selected = self.selected.get((actor.read_projection, capability))
        if selected and selected[0] > self.clock():
            return copy.deepcopy(selected[1])
        entry = self.cache.get(actor.read_projection, {})
        return next(
            (copy.deepcopy(c) for c in entry.get("items", []) if c["id"] == capability), None
        )

    async def execute(self, actor, item, room, parameters, deadline, request_id=None):
        if not permitted(actor, item, parameters) or room not in actor.rooms:
            raise QueryError("PERMISSION_DENIED")
        validate(parameters, item["parameters"])
        request_id = request_id or "astrbot-" + uuid.uuid4().hex
        path = {"roomId": room, "capabilityId": item["id"]}
        if item["access"] == "read":
            args = {
                "path": path,
                "query": {
                    "contractVersion": 1,
                    "schemaRevision": item["schemaRevision"],
                    "requestId": request_id,
                    "parameters": json.dumps(parameters, ensure_ascii=False, separators=(",", ":")),
                },
            }
            if self.receipts:
                await self.receipts.reserve(actor, item, room, request_id)
            try:
                return await self.gateway.call(actor, "api_queryBotCapability", args, deadline)
            except QueryError as error:
                if self.receipts:
                    error.receipt = {
                        "requestId": request_id, "roomId": room, "capabilityId": item["id"],
                        "status": "uncertain", "complete": False,
                        "error": {"code": error.code, "message": "查询结果尚未确认，请回读原查询，不要重新提交。"},
                    }
                raise
        args = {
            "path": path,
            "body": {
                "contractVersion": 1,
                "schemaRevision": item["schemaRevision"],
                "parameters": parameters,
            },
            "requestId": request_id,
        }
        return await self.gateway.call(actor, "api_runBotCapability", args, deadline, write=True)
