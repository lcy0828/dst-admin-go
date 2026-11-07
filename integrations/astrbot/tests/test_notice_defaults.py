"""Upgrade real preference shapes with synthetic rooms and no network or QQ I/O."""

import copy
import sys
import unittest
from pathlib import Path
from unittest.mock import AsyncMock

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from astrbot_plugin_dst_companion.event_notices import LABELS
from astrbot_plugin_dst_companion.gateway import QueryError
from astrbot_plugin_dst_companion.notice_settings import (
    default_notices,
    notices_enabled,
    set_broadcast,
    upgrade_notices,
)
from astrbot_plugin_dst_companion.onboarding import Enrollment

SESSION = "adapter:GroupMessage:123"


def record(**extra):
    return dict(enabled=True, query=True, source="example", rooms=["example"], **extra)


class NoticeDefaultsTests(unittest.IsolatedAsyncioTestCase):
    def test_defaults_cover_all_known_categories_and_high_volume_stays_separate(self):
        flags = default_notices()
        self.assertEqual(set(flags), set(LABELS) | {"presence", "server", "chat"})
        self.assertEqual({key for key, value in flags.items() if not value}, {"chat", "mods"})

    async def test_active_legacy_group_fills_missing_and_preserves_explicit_off_and_authority(self):
        old = record(
            notices=dict(presence=True, server=True, chat=True, season=False, boss=False),
            rolePermissions={"admin": {"write": ["player.resurrect"]}},
            messageTemplates=[{"example": "keep"}],
            noticeOwner="unified",
        )
        original = copy.deepcopy(old)
        store = {"sessions_v3": {SESSION: old}, "broadcast_state_v2": {"synthetic": "untouched"}}

        async def save(key, value):
            store[key] = copy.deepcopy(value)

        enrollment = Enrollment(save)
        await enrollment.load(AsyncMock(side_effect=lambda key, default: store.get(key, default)))
        actual = enrollment.records[SESSION]
        for key in (
            "rooms",
            "source",
            "query",
            "rolePermissions",
            "messageTemplates",
            "noticeOwner",
        ):
            self.assertEqual(actual[key], old[key])
        self.assertFalse(actual["notices"]["season"])
        self.assertFalse(actual["notices"]["boss"])
        self.assertTrue(actual["notices"]["chat"])
        for key in ("maintenance", "updates", "players", "backup", "software", "alerts"):
            self.assertTrue(actual["notices"][key])
        self.assertEqual(store["sessions_v3"], enrollment.records)
        self.assertEqual(store["sessions_v3_before_notice_defaults"], {SESSION: original})
        self.assertEqual(store["broadcast_state_v2"], {"synthetic": "untouched"})
        self.assertEqual(old, original)

    def test_all_off_empty_chat_only_and_mod_only_do_not_gain_categories(self):
        for flags in (
            {},
            {"presence": False, "server": False, "chat": False},
            {"chat": True},
            {"presence": False, "server": False, "chat": True},
            {"mods": True},
        ):
            with self.subTest(flags=flags):
                actual = upgrade_notices(record(notices=flags))
                self.assertEqual(
                    {key for key, value in actual["notices"].items() if value},
                    {key for key, value in flags.items() if value},
                )
                self.assertEqual(notices_enabled(actual), any(flags.values()))

    def test_disabled_group_and_master_opt_out_remain_disabled(self):
        disabled = {"enabled": False, "notices": {"presence": True, "chat": True}}
        self.assertEqual(upgrade_notices(disabled), disabled)
        old = record(noticeEnabled=False, notices={"presence": True})
        upgraded = upgrade_notices(old)
        self.assertFalse(notices_enabled(upgraded))
        self.assertFalse(upgraded["notices"]["season"])

    def test_muted_preferences_survive_reload_and_old_plugin_keeps_silence(self):
        value = upgrade_notices(record(notices={"server": True, "season": False}))
        expected = copy.deepcopy(value["notices"])
        set_broadcast(value, False)
        upgraded = upgrade_notices(value)
        self.assertEqual(upgraded, value)
        self.assertEqual(upgraded["noticeChoices"], expected)
        self.assertFalse(any(upgraded["notices"].values()))
        self.assertEqual(upgrade_notices(upgraded), upgraded)
        set_broadcast(upgraded, True)
        self.assertEqual(upgraded["notices"], expected)
        self.assertNotIn("noticeChoices", upgraded)

    async def test_upgrade_is_idempotent_and_does_not_repeat_storage_work(self):
        records = {SESSION: upgrade_notices(record(notices={"server": True}))}
        save = AsyncMock()
        enrollment = Enrollment(save)
        for _ in range(2):
            await enrollment.load(AsyncMock(return_value=records))
            self.assertEqual(enrollment.records, records)
        save.assert_not_awaited()

    async def test_save_false_exception_or_failed_backup_never_activates_new_defaults(self):
        for outcomes in (
            [False],
            [None, False],
            [OSError("unavailable")],
            [None, OSError("unavailable")],
        ):
            with self.subTest(outcomes=outcomes):
                enrollment = Enrollment(AsyncMock(side_effect=outcomes))
                with self.assertRaises((QueryError, OSError)):
                    await enrollment.load(
                        AsyncMock(return_value={SESSION: record(notices={"server": True})})
                    )
                self.assertEqual(enrollment.records, {})
                self.assertFalse(enrollment.loaded)

    async def test_failed_group_normalization_backup_never_grants_conflicting_scope(self):
        enrollment = Enrollment(AsyncMock(return_value=False))
        with self.assertRaises(QueryError):
            await enrollment.load(
                AsyncMock(
                    return_value={"adapter:GroupMessage:456_123": record(notices={"server": True})}
                )
            )
        self.assertFalse(enrollment.loaded)
        self.assertEqual(enrollment.records, {})

    def test_non_boolean_master_is_rejected(self):
        for invalid in ("false", 0, 1, None, {}):
            with self.subTest(value=invalid), self.assertRaises(ValueError):
                Enrollment.validate(SESSION, record(noticeEnabled=invalid))

    def test_invalid_saved_choices_are_rejected(self):
        for choices in ([], {"season": "false"}, {"unknown": True}):
            with self.subTest(choices=choices), self.assertRaises(ValueError):
                Enrollment.validate(SESSION, record(noticeChoices=choices))
