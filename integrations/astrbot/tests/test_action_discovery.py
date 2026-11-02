import copy
import unittest
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from astrbot_plugin_dst_companion.discovery import actions, score


class ActionDiscoveryTests(unittest.TestCase):
    def item(self):
        return {"parameters": {"properties": {"action": {"type": "string", "enum": ["freeze", "delete"]}}},
                "actions": [{"id": "nearby.freeze", "title": "冻结附近生物", "searchTerms": ["冰冻", "冻结", "冻住"],
                             "parameters": {"action": "freeze"}}]}

    def test_validated_enum_suggestions_preserve_parent_permission(self):
        item = self.item()
        self.assertEqual(actions(item)[0]["parameters"], {"action": "freeze"})
        for marker in ("risk", "readOnly", "operation", "script", "confirmed"):
            modified = copy.deepcopy(item)
            modified["actions"][0][marker] = "forged"
            self.assertNotIn(marker, actions(modified)[0])

    def test_injection_and_confirmation_are_never_prepopulated(self):
        for params in ({"action": "freeze();c_save()"}, {"confirmed": True}, {"player_id": "KU_EXAMPLE"},
                       {"action": "freeze", "enabled": True}, {"action": {}}, {"action": "unknown"}):
            item = self.item()
            item["actions"][0]["parameters"] = params
            self.assertEqual(actions(item), [])

    def test_catalog_bounds_and_old_directory(self):
        self.assertEqual(actions({}), [])
        for value in (None, "bad", [{}] * 129):
            item = self.item()
            item["actions"] = value
            self.assertEqual(actions(item), [])

    def test_precise_phrase_wins_and_search_never_executes(self):
        for query in ("冻结附近生物", "冻住我附近的蜘蛛", "别冻结蜘蛛", "冻结会不会影响玩家"):
            self.assertGreater(score(query, self.item()["actions"][0]["searchTerms"] + ["冻结附近生物"]), 0)
        self.assertGreater(score("冻结附近生物", ["冻结附近生物", "冻结"]), score("冻结附近生物", ["生物"] * 100))
