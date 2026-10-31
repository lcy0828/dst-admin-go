"""Chinese vanilla names without inventing mod names or freshness ratios."""

import importlib.util
import sys
import tempfile
import unittest
from pathlib import Path
from zipfile import ZipFile

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from astrbot_plugin_dst_companion.formatters import inventory
from astrbot_plugin_dst_companion.item_names import item_name
from package import build


class InventoryNamesTests(unittest.TestCase):
    def test_offline_prefabs_and_english_runtime_names_use_the_same_catalog(self):
        for item in (
            {"prefab": "meatballs", "name": "meatballs", "nameSource": "prefab"},
            {"prefab": "meatballs", "name": "Meatballs", "nameSource": "runtime"},
            {"prefab": "meatballs"},
        ):
            self.assertEqual(item_name(item), "肉丸")
        text = inventory(
            {
                "source": "save",
                "status": "complete",
                "equipment": [],
                "inventory": [
                    {
                        "prefab": "meatballs",
                        "nameSource": "prefab",
                        "count": 2,
                        "metrics": {
                            "perishable": {
                                "value": 957.606,
                                "unit": "seconds",
                                "availability": "value_only",
                            }
                        },
                    }
                ],
            }
        )
        self.assertIn("肉丸 ×2 [剩余保鲜 15分58秒]", text)
        self.assertIn("支持范围内完整", text)
        self.assertNotIn("%", text)
        self.assertNotIn("complete", text)

    def test_custom_names_unknown_mods_and_nested_items_are_preserved(self):
        for source in ("saved_name", "runtime"):
            self.assertEqual(
                item_name({"prefab": "meatballs", "name": "定制午餐", "nameSource": source}),
                "定制午餐",
            )
        self.assertEqual(
            item_name({"prefab": "meatballs", "name": "Meatballs", "nameSource": "saved_name"}),
            "Meatballs",
        )
        self.assertEqual(item_name({"prefab": "example_mod_item"}), "example_mod_item")
        text = inventory(
            {
                "source": "live",
                "status": "partial",
                "equipment": [
                    {
                        "prefab": "krampus_sack",
                        "slot": "body",
                        "contents": [
                            {"prefab": "cane"},
                            {
                                "prefab": "blueprint",
                                "name": "示例配方蓝图",
                                "nameSource": "saved_name",
                            },
                        ],
                    }
                ],
            }
        )
        for expected in ("身体：坎普斯背包", "└ 步行手杖", "示例配方蓝图", "部分内容"):
            self.assertIn(expected, text)

    def test_standalone_zip_contains_canonical_catalog_and_loads_without_the_repo(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            package = build(root, "companion")
            with ZipFile(package) as archive:
                data = archive.read("vanilla_items.json")
                canonical = (
                    Path(__file__).resolve().parents[3] / "internal/entitycatalog/data/vanilla.json"
                )
                self.assertEqual(data, canonical.read_bytes())
                (root / "vanilla_items.json").write_bytes(data)
                (root / "item_names.py").write_bytes(archive.read("item_names.py"))
            spec = importlib.util.spec_from_file_location(
                "isolated_item_names", root / "item_names.py"
            )
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)
            self.assertEqual(module.item_name({"prefab": "meatballs"}), "肉丸")
