import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('builder', Path(__file__).with_name('rebuild-entity-directory.py'))
builder = importlib.util.module_from_spec(spec)
spec.loader.exec_module(builder)


class DirectoryBuilderTests(unittest.TestCase):
    def test_official_alias_and_untranslated_internal_prefab(self):
        po = '\n'.join(['msgctxt "STRINGS.NAMES.DEER_ANTLER"', 'msgid "Deer Antler"', 'msgstr "鹿角"', ''])
        export = {'items': [{'id': 'deer_antler1', 'nameKey': 'deer_antler', 'category': 'item'}, {'id': 'internal_fx'}]}
        rows = builder.rebuild(export, po, {})
        self.assertEqual(rows[0][1:3], ['鹿角', 'Deer Antler'])
        self.assertEqual(rows[0][4], 'official-alias')
        self.assertEqual(rows[1][1:], ['internal_fx', 'internal_fx', 'other', 'fallback'])

    def test_duplicate_ids_and_ambiguous_english_names(self):
        with self.assertRaises(ValueError):
            builder.rebuild({'items': [{'id': 'a'}, {'id': 'a'}]}, '', {})
        po = '\n'.join(['msgctxt "STRINGS.NAMES.A"', 'msgid "Shared"', 'msgstr "甲"', '', 'msgctxt "STRINGS.NAMES.B"', 'msgid "Shared"', 'msgstr "乙"', ''])
        rows = builder.rebuild({'items': [{'id': 'internal', 'nameEn': 'Shared'}]}, po, {})
        self.assertEqual(rows[0][4], 'fallback')
        self.assertEqual(rows[0][1], 'Shared')

    def test_multiline_po_names_and_reviewed_categories(self):
        po = '\n'.join(['msgctxt "STRINGS.NAMES.FIREPIT"', 'msgid "Fire"', '" Pit"', 'msgstr "石头"', '"营火"', ''])
        rows = builder.rebuild({'items': [{'id': 'firepit'}]}, po, {'items': [['firepit', '', '', 'structure']]})
        self.assertEqual(rows[0][1:5], ['石头营火', 'Fire Pit', 'structure', 'official'])


if __name__ == '__main__':
    unittest.main()
