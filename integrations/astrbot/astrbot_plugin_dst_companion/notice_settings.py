"""Notification preferences; the master switch never changes category choices."""

import copy


def default_notices():
    return dict(
        presence=True,
        server=True,
        maintenance=True,
        season=True,
        updates=True,
        backup=True,
        software=True,
        players=True,
        boss=True,
        alerts=True,
        chat=False,
        mods=False,
    )


def upgrade_notices(record):
    """Fill missing choices only for groups already using ordinary broadcasts.

    Legacy all-off and chat/mod-only groups retain their original scope. Explicit
    category choices, disabled groups, permissions and delivery journals survive.
    A fully populated record is its own migration marker.
    """
    result = copy.deepcopy(record)
    if not result.get("enabled"):
        return result
    flags = result.get("noticeChoices", result.get("notices", {}))
    active = result.get("noticeEnabled", True) and any(
        value for key, value in flags.items() if key not in {"chat", "mods"}
    )
    result.setdefault("noticeEnabled", any(flags.values()))
    result["notices"] = {
        key: flags.get(key, value if active else False) for key, value in default_notices().items()
    }
    result.pop("noticeChoices", None)
    if not result["noticeEnabled"]:
        result["noticeChoices"] = result["notices"]
        # Older plugins understand only "notices". Keep their view muted too.
        result["notices"] = {key: False for key in default_notices()}
    return result


def notices_enabled(record):
    return record.get("noticeEnabled", True) and any(record.get("notices", {}).values())


def notice_choices(record):
    if record.get("noticeEnabled", True):
        return record.setdefault("notices", {})
    return record.setdefault("noticeChoices", copy.deepcopy(record.get("notices", {})))


def set_broadcast(record, enabled):
    choices = copy.deepcopy(notice_choices(record))
    record["noticeEnabled"] = enabled
    # Old versions erased category choices on "off". An explicit "on" gives
    # these groups the recommended choices; subsequent off/on preserves them.
    if enabled:
        record["notices"] = choices if any(choices.values()) else default_notices()
        record.pop("noticeChoices", None)
    else:
        record["noticeChoices"] = choices
        record["notices"] = {key: False for key in default_notices()}
