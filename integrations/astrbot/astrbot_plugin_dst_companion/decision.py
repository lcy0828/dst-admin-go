"""Risk tier and confirmation policy shared by direct commands and AI tools.

Access answers *who* may act: everyone reads, administrators manage. This
module answers *how* an authorized write is confirmed, from one place:

* R0 read                     — no confirmation
* R1 single-player reversible — no confirmation (revive, give item, modes …)
* R2 affects the whole room   — reply 确认 once (skip day, restart …)
* R3 destructive/irreversible — reply a typed phrase (reset world, ban …)

Servers that publish ``tier``/``confirm`` metadata win; older servers fall
back to the advertised risk and parameter hints.
"""

import re

from .interactions import effective_risk

TIERS = ("R0", "R1", "R2", "R3")
CONFIRMS = ("none", "once", "typed")
DEFAULT_CONFIRM = {"R0": "none", "R1": "none", "R2": "once", "R3": "typed"}

# Writes without a player target that still affect only one person, or room
# operations that older servers publish without tier metadata.
R2_IDS = frozenset({
    "room.start", "room.stop", "room.restart", "room.save", "room.backup.create",
    "room.backups.configure", "player.kick", "player.unban",
})
R3_IDS = frozenset({
    "room.reset", "workbench.regenerate", "workbench.rollback", "player.kill",
    "player.reset_character", "player.ban",
})
TYPED_PHRASES = {
    "room.reset": "确认重置",
    "workbench.regenerate": "确认重置",
    "workbench.rollback": "确认回滚",
    "player.kill": "确认击杀",
    "player.reset_character": "确认重选",
    "player.ban": "确认封禁",
    "player.unban": "确认解封",
    "workbench.clear_player_inventory": "确认清空",
}
BACKUP_CHOICE = frozenset({"room.reset"})
NO_BACKUP = frozenset({"不备份", "不要备份", "不用备份", "无需备份", "跳过备份", "免备份", "别备份"})
WITH_BACKUP = frozenset({"备份", "先备份", "要备份", "需要备份", "并备份"})
_SPACE = re.compile(r"[\s　]+")
_TRAIL = re.compile(r"[。！!～~,，.]+$")


def _declared(item, parameters, key, allowed):
    policies = item.get("actionPolicies") if isinstance(item.get("actionPolicies"), dict) else {}
    for row in item.get("actions", []) or []:
        if not isinstance(row, dict):
            continue
        defaults = row.get("parameters", {})
        if parameters is not None and defaults and all(parameters.get(k) == v for k, v in defaults.items()):
            # The selected sub-action's policy wins over its parent's.
            policy = policies.get(row.get("id")) if isinstance(policies.get(row.get("id")), dict) else row
            if policy.get(key) in allowed:
                return policy[key]
    value = item.get(key)
    return value if value in allowed else None


def tier(item, parameters=None):
    if item.get("access") != "write":
        return "R0"
    declared = _declared(item, parameters, "tier", TIERS)
    if declared:
        return declared
    if item.get("id") in R3_IDS or effective_risk(item, parameters) == "critical":
        return "R3"
    if item.get("id") in R2_IDS:
        return "R2"
    hints = item.get("parameterHints", {}) or {}
    if str(item.get("id", "")).startswith("workbench.") and "player" not in hints.values():
        return "R2"
    return "R1"


def confirmation(item, parameters=None):
    declared = _declared(item, parameters, "confirm", CONFIRMS)
    return declared or DEFAULT_CONFIRM[tier(item, parameters)]


def typed_phrase(item):
    phrase = item.get("confirmPhrase")
    if isinstance(phrase, str) and 2 <= len(phrase) <= 16:
        return phrase
    return TYPED_PHRASES.get(item.get("id"), "确认执行")


def wants_backup(item, parameters):
    return item.get("id") in BACKUP_CHOICE and parameters.get("backup", True) is not False


def looks_like_confirmation(text):
    """Owned by a pending confirmation, even when the phrase is wrong."""
    value = _TRAIL.sub("", text.strip())
    return value.startswith("确认") and len(value) <= 64


def confirmation_reply(text, form, room_name=""):
    """Parameter overrides when ``text`` confirms ``form``; ``None`` otherwise."""
    value = _SPACE.sub(" ", _TRAIL.sub("", text.strip()))
    if form.get("confirm") != "typed":
        return {} if value == "确认" else None
    phrase = form.get("phrase") or "确认执行"
    compact = value.replace(" ", "")
    if not compact.startswith(phrase):
        return None
    rest = compact[len(phrase):]
    if room_name:
        rest = rest.replace(room_name.replace(" ", ""), "", 1)
    if not rest:
        return {}
    if form.get("item", {}).get("id") in BACKUP_CHOICE:
        if rest in NO_BACKUP:
            return {"backup": False}
        if rest in WITH_BACKUP:
            return {"backup": True}
    return None


def confirmation_prompt(item, parameters, form_confirm):
    """The last lines of a confirmation prompt: what to reply, never internal names."""
    if form_confirm != "typed":
        return "回复「确认」执行，回复「取消」放弃。尚未执行。"
    phrase = typed_phrase(item)
    if item.get("id") in BACKUP_CHOICE:
        if wants_backup(item, parameters):
            return ("将先创建重置前备份，再重置。\n回复「" + phrase + "」执行；不需要备份请回复「"
                    + phrase + " 不备份」；回复「取消」放弃。尚未执行。")
        return ("按你的要求不备份，直接重置，旧进度无法恢复。\n回复「" + phrase + "」执行；需要备份请回复「"
                + phrase + " 备份」；回复「取消」放弃。尚未执行。")
    return "这是高风险操作。回复「" + phrase + "」执行，回复「取消」放弃。尚未执行。"
