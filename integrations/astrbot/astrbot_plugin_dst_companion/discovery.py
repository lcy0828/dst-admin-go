"""Bounded action descriptions. Discovery never grants access or executes code."""


def score(query, terms):
    best = 0
    for term in terms:
        if not isinstance(term, str) or not term or len(term) > 128:
            continue
        term = term.strip().casefold()
        value = (1000 + len(term) if query == term else
                 100 + len(term) if term in query else
                 len(query) if len(query) >= 2 and query in term else 0)
        best = max(best, value)
    return best


def actions(item):
    """Accept enum suggestions only; no defaults for targets or confirmations."""
    rows = item.get("actions", [])
    if not isinstance(rows, list) or len(rows) > 128:
        return []
    props = item.get("parameters", {}).get("properties", {})
    accepted, seen = [], set()
    for row in rows:
        if not isinstance(row, dict):
            continue
        ident, title, params = row.get("id"), row.get("title"), row.get("parameters")
        terms = row.get("searchTerms", [])
        if (not isinstance(ident, str) or not 1 <= len(ident) <= 128 or ident in seen
                or not isinstance(title, str) or not 1 <= len(title) <= 128
                or not isinstance(params, dict) or len(params) != 1
                or not isinstance(terms, list) or len(terms) > 32
                or any(not isinstance(t, str) or not 1 <= len(t) <= 128 for t in terms)):
            continue
        if any(k not in props or not isinstance(v, str) or v not in props[k].get("enum", [])
               or k in {"confirmed", "confirmation", "playerId", "player_id", "worldId"}
               for k, v in params.items()):
            continue
        seen.add(ident)
        accepted.append({"id": ident, "title": title, "parameters": dict(params),
                         "searchTerms": terms, "description": str(row.get("description", ""))[:512]})
    return accepted
