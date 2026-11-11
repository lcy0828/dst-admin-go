"""Select a saved read by explicit file time, never assert a player's location."""

import re
import time

from .display import local_time, time_text
from .formatters import world_name


def sources(details):
    candidates = (details or {}).get("candidates", [])
    if not isinstance(candidates, list) or not 1 <= len(candidates) <= 64:
        return [], None
    rows, stamps = [], []
    for candidate in candidates:
        if not isinstance(candidate, dict) or not re.fullmatch(r"[a-f0-9]{64}", str(candidate.get("sourceId", ""))):
            return [], None
        world = candidate.get("worldId")
        if not isinstance(world, str) or not world or len(world) > 255:
            return [], None
        stamp = local_time(candidate.get("fileModifiedAt"))
        stamps.append(stamp.timestamp() if stamp else None)
        rows.append({"id": candidate["sourceId"], "worldId": world,
                     "name": world_name(candidate.get("worldName") or world)
                     + " · 保存于 " + time_text(candidate.get("fileModifiedAt"), year=True)})
    # Different sessions within one shard are genuinely ambiguous. Missing,
    # identical or future file times must not silently choose a source either.
    if (len({r["id"] for r in rows}) != len(rows)
            or len({r["worldId"] for r in rows}) != len(rows)
            or any(s is None or s > time.time() + 300 for s in stamps)):
        return rows, None
    newest = max(stamps)
    return rows, rows[stamps.index(newest)] if stamps.count(newest) == 1 else None


def unique_read_match(result):
    """Only a complete unique directory match may skip the read-only picker."""
    return (result.get("status") == "ambiguous" and result.get("match") == "substring"
            and type(result.get("total")) is int and result["total"] == 1
            and result.get("coverage") == "current_directory"
            and not result.get("hasMore") and not result.get("_aiTruncated")
            and len(result.get("items", [])) == 1)
