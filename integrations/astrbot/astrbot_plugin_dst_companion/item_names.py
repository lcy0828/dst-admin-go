"""Vanilla display names from the panel's bundled catalog, without MCP calls."""

import json
from functools import lru_cache
from pathlib import Path


@lru_cache(maxsize=1)
def names():
    resource = Path(__file__).with_name("vanilla_items.json")
    if not resource.is_file():
        # Source checkouts use the same canonical file that package.py embeds.
        resource = Path(__file__).resolve().parents[3] / "internal/entitycatalog/data/vanilla.json"
    try:
        if resource.stat().st_size > 1 << 20:
            return {}
        data = json.loads(resource.read_text(encoding="utf-8"))
        return {
            row[0]: (row[1], row[2])
            for row in data["items"]
            if isinstance(row, list)
            and len(row) >= 3
            and all(isinstance(value, str) for value in row[:3])
            and any("\u3400" <= char <= "\u9fff" for char in row[1])
        }
    except (OSError, ValueError, KeyError, TypeError):
        return {}


def item_name(item):
    prefab = str(item.get("prefab") or "")
    provided = str(item.get("name") or "")
    if provided and item.get("nameSource") == "saved_name":
        return provided  # A player-given name is not a missing translation.
    known = names().get(prefab)
    if known and (not provided or provided == prefab or provided.casefold() == known[1].casefold()):
        return known[0]
    return provided or prefab
