"""Fail closed before calling AstrBot's permission helper (which fails open)."""


async def check_native(load, checker, fallback, actor, name, context):
    if not checker:
        return False
    try:
        policies = await load("tool_permissions", {})
        if not isinstance(policies, dict) or not isinstance(policies.get("_default", {}), dict):
            return False
        defaults = policies.get("_default", {})
        rule = defaults.get(name)
        if rule is None:
            rule = fallback(name) if fallback else "member"
        if rule not in {"member", "admin"}:
            return False
        if rule == "admin" and not actor.event.is_admin():
            return False
        return await checker(name, context) is None
    except Exception:
        return False
