"""Bounded action descriptions. Discovery never grants access or executes code."""


def normalize(value):
    return "".join(str(value or "").split()).casefold()


def score(query, terms):
    query = normalize(query)
    if not query:
        return 0
    best = 0
    for term in terms or ():
        if not isinstance(term, str) or not term or len(term) > 128:
            continue
        term = normalize(term)
        if not term:
            continue
        value = (1000 + len(term) if query == term else
                 100 + len(term) if term in query else
                 len(query) if len(query) >= 2 and query in term else 0)
        best = max(best, value)
    return best


def example_score(query, examples):
    """Examples match whole utterances; a short fragment inside an example earns 0."""
    query = normalize(query)
    if not query:
        return 0
    best = 0
    for example in examples or ():
        if not isinstance(example, str) or not example or len(example) > 128:
            continue
        example = normalize(example)
        if not example:
            continue
        if query == example:
            best = max(best, 1000 + len(example))
        elif example in query:
            best = max(best, 100 + len(example))
    return best


def negated(query, negatives):
    query = normalize(query)
    if not query:
        return False
    for negative in negatives or ():
        if isinstance(negative, str):
            norm = normalize(negative)
            if norm and norm in query:
                return True
    return False


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
                or not isinstance(terms, list) or len(terms) > 64
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
