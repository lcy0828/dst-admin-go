#!/usr/bin/env python3
"""Rebuild display-only metadata from an unmodded world's registered Prefabs.

Inputs: registered JSON export and the SAME game's scripts.zip. Never executes
Lua or reads saves. Output contains names/categories, not game code or artwork.
See internal/entitycatalog/data/README.md for the readonly export command.
"""
import argparse
import ast
from collections import defaultdict
import hashlib
import json
from pathlib import Path
import re
import zipfile


def translations(text):
    entries, current, field = {}, {}, None
    def commit():
        context = current.get('msgctxt', '')
        if context.startswith('STRINGS.NAMES.') and current.get('msgstr'):
            entries[context.removeprefix('STRINGS.NAMES.').lower()] = (current.get('msgid', ''), current['msgstr'])
    for line in text.splitlines() + ['']:
        if not line.strip():
            commit(); current, field = {}, None
        elif line.startswith(('msgctxt ', 'msgid ', 'msgstr ')):
            field, literal = line.split(' ', 1)
            current[field] = ast.literal_eval(literal)
        elif line.startswith('"') and field:
            current[field] += ast.literal_eval(line)
    return entries


def rebuild(registered, strings, previous):
    names = translations(strings)
    by_english = defaultdict(set)
    for en, zh in names.values():
        if en: by_english[en].add(zh)
    categories = {row[0]: row[3] for row in previous.get('items', [])}
    rows, seen = [], set()
    for entity in sorted(registered['items'], key=lambda v: v['id']):
        prefab = entity['id']
        if not re.fullmatch(r'[a-z0-9_]{1,80}', prefab) or prefab in seen:
            raise ValueError('invalid or duplicate prefab: ' + prefab)
        seen.add(prefab)
        en, zh, source = entity.get('nameEn') or '', '', 'fallback'
        # Official scrapbook aliases cover generated variants (antlers,
        # ornaments, etc.) without guessing based on naming prefixes.
        for key in (prefab, entity.get('nameKey', '')):
            if key in names:
                official_en, zh = names[key]
                en = en or official_en
                source = 'official' if key == prefab else 'official-alias'
                break
        if not zh and len(by_english.get(en, set())) == 1:
            zh = next(iter(by_english[en])); source = 'official-shared-name'
        en = en or prefab
        zh = zh or en
        category = categories.get(prefab, 'other')
        if category == 'other':
            kind, sub = entity.get('category'), entity.get('subcategory')
            if kind in ('item', 'food', 'creature'): category = kind
            elif kind == 'thing': category = 'structure'
            if sub in ('weapon', 'armor', 'tool', 'clothing'): category = 'equipment'
            elif sub == 'boss': category = 'boss'
        rows.append([prefab, zh, en, category, source])
    return rows


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--registered', type=Path, required=True)
    p.add_argument('--scripts', type=Path, required=True)
    p.add_argument('--game-version', required=True)
    p.add_argument('--built-at', required=True, help='Pinned UTC RFC3339 timestamp for reproducible output')
    p.add_argument('--categories', type=Path, help='Previous directory, to preserve reviewed categories')
    p.add_argument('--output', type=Path, required=True)
    args = p.parse_args()
    if not re.fullmatch(r'[0-9]+', args.game_version) or not re.fullmatch(r'\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z', args.built_at):
        p.error('expected numeric game version and UTC RFC3339 timestamp')
    raw = args.registered.read_bytes()
    registered = json.loads(raw)
    if registered.get('gameVersion') != args.game_version:
        p.error('registered export does not match --game-version')
    with zipfile.ZipFile(args.scripts) as archive:
        po = archive.read('scripts/languages/chinese_s.po')
    previous = json.loads(args.categories.read_bytes()) if args.categories else {}
    value = {'schemaVersion': 2, 'gameVersion': args.game_version, 'builtAt': args.built_at,
             'source': 'Klei Entertainment / registered vanilla Prefabs + official Chinese localization',
             'registeredSHA256': hashlib.sha256(raw).hexdigest(), 'localizationSHA256': hashlib.sha256(po).hexdigest(),
             'items': rebuild(registered, po.decode('utf-8-sig'), previous)}
    args.output.write_text(json.dumps(value, ensure_ascii=False, separators=(',', ':')) + '\n', encoding='utf-8')
    print(json.dumps({'gameVersion': value['gameVersion'], 'entities': len(value['items']), 'bytes': args.output.stat().st_size}))


if __name__ == '__main__': main()
