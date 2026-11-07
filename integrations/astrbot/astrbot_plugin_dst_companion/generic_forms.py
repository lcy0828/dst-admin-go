"""Small schema-driven continuations; native validation remains authoritative."""

import json

from .gateway import QueryError


def dereference(schema, root):
    for _ in range(16):
        ref = schema.get("$ref", "") if isinstance(schema, dict) else ""
        if not ref.startswith("#/$defs/"):
            return schema
        schema = root.get("$defs", {}).get(ref[8:], {})
    raise QueryError("CAPABILITY_INCOMPATIBLE")


def missing(schema, value, root=None, path="", depth=0):
    root = root or schema
    if depth > 12:
        raise QueryError("CAPABILITY_INCOMPATIBLE")
    schema = dereference(schema, root)
    if not isinstance(value, dict):
        return None
    props = schema.get("properties", {})
    for key in schema.get("required", []):
        child = dereference(props.get(key, {}), root)
        pointer = path + "." + key if path else key
        if key not in value:
            # Fill structural containers only; never insert property defaults.
            if child.get("type") == "object" and child.get("required"):
                value[key] = {}
            else:
                return pointer, child
        found = missing(child, value[key], root, pointer, depth+1)
        if found:
            return found
    for key in value.keys() - set(schema.get("required", [])):
        if key in props:
            found = missing(props[key], value[key], root, path + "." + key if path else key, depth+1)
            if found:
                return found
    return None


def parse_answer(text, schema):
    if schema.get("type") == "string":
        return text
    if schema.get("type") == "boolean":
        from .parameter_ui import SWITCHES
        if text in SWITCHES:
            return SWITCHES[text]
    try:
        return json.loads(text)
    except ValueError:
        if schema.get("type") in (None, ["string", "null"]):
            return text
        raise QueryError("PARAMETER_INVALID", message="输入格式不符合要求，请按提示填写。") from None
