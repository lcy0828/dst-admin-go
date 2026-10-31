"""Group enrollment is shared even when AstrBot isolates each member's chat."""

import re


def group_origin(session):
    value = str(session)
    parts = value.split(":", 2)
    if (
        len(parts) == 3
        and parts[1] == "GroupMessage"
        and re.fullmatch(r"[0-9]{1,20}(?:_[0-9]{1,20})?", parts[2])
    ):
        # Matches AstrBot's OneBot send_by_session convention: user_group.
        return parts[0] + ":GroupMessage:" + parts[2].rsplit("_", 1)[-1]
    return value


def canonical_records(records):
    result = {}
    for session, record in records.items():
        target = group_origin(session)
        if target in result and result[target] != record:
            # Never combine different room scopes, sources or opt-outs.
            result[target] = {"enabled": False}
        else:
            result[target] = record
    return result
