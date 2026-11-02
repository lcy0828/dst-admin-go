import asyncio
import base64
import hashlib
import json
import os
import sys
import tempfile
import time
import unittest
from pathlib import Path
from types import ModuleType
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock, patch
from zipfile import ZipFile

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

from astrbot_plugin_dst_companion.update_host import HostInstaller, atomic_exchange
from astrbot_plugin_dst_companion.update_security import (
    canonical,
    extract_verified,
    verify_manifest,
)
from astrbot_plugin_dst_companion.updates import Updates
from astrbot_plugin_dst_companion.version import VERSION


class ManifestTests(unittest.TestCase):
    def setUp(self):
        self.key = Ed25519PrivateKey.generate()  # Ephemeral fixture, never a release key.
        self.keys = {
            "test": base64.b64encode(
                self.key.public_key().public_bytes(
                    serialization.Encoding.Raw, serialization.PublicFormat.Raw
                )
            ).decode()
        }
        self.payload = {
            "plugin": "astrbot_plugin_dst_companion",
            "manifestVersion": 1,
            "version": "0.4.1",
            "sequence": 5,
            "issuedAt": 1000,
            "expiresAt": 3000,
            "python": ">=3.12,<3.14",
            "astrbot": ">=4.29.3,<4.30",
            "stateFormat": 1,
            "rollbackCompatible": True,
            "protocols": [1, 2],
            "artifact": {
                "url": "https://downloads.invalid/plugin.zip",
                "sha256": "0" * 64,
                "size": 12,
            },
        }

    def signed(self, payload=None):
        payload = payload or self.payload
        return canonical(
            {
                "keyId": "test",
                "payload": payload,
                "signature": base64.b64encode(self.key.sign(canonical(payload))).decode(),
            }
        )

    def verify(self, raw):
        return verify_manifest(
            raw, self.keys, current="0.4.0", sequence=4, python="3.12.2", astrbot="4.29.3", now=2000
        )

    def test_signature_expiry_rollback_and_environment(self):
        self.assertEqual(self.verify(self.signed())["version"], "0.4.1")
        raw = json.loads(self.signed())
        raw["payload"]["artifact"]["url"] = "https://other.invalid/plugin.zip"
        with self.assertRaises(InvalidSignature):
            self.verify(canonical(raw))
        for change in (
            {"expiresAt": 1500},
            {"sequence": 3},
            {"version": "0.3.9"},
            {"python": ">=3.14"},
            {"stateFormat": 2},
            {"rollbackCompatible": False},
            {"protocols": [99]},
        ):
            with self.assertRaises(ValueError):
                self.verify(self.signed({**self.payload, **change}))
        with self.assertRaises(ValueError):
            verify_manifest(
                self.signed(),
                {},
                current="0.4.0",
                sequence=0,
                python="3.12.2",
                astrbot="4.29.3",
                now=2000,
            )

    def test_archive_traversal_tampering_and_executable_syntax(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            for index, files in enumerate(
                ({"../escape.py": "x=1"}, {"main.py": "invalid python !!!"}, {"main.py": "x=1"})
            ):
                archive = root / f"{index}.zip"
                with ZipFile(archive, "w") as z:
                    for name, data in files.items():
                        z.writestr(name, data)
                manifest = {
                    "version": "0.4.1",
                    "artifact": {
                        "size": archive.stat().st_size,
                        "sha256": hashlib.sha256(archive.read_bytes()).hexdigest(),
                    },
                }
                with self.assertRaises((ValueError, SyntaxError)):
                    extract_verified(archive, root / f"stage-{index}", manifest)
            self.assertFalse((root.parent / "escape.py").exists())

    def test_atomic_directory_switch_preserves_old_code_and_external_state(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            a, b = root / "active", root / "stage"
            a.mkdir()
            b.mkdir()
            (a / "code").write_text("old")
            (b / "code").write_text("new")
            ledger = root / "ledger.json"
            ledger.write_text('{"cursor":42}')
            atomic_exchange(a, b)
            self.assertEqual((a / "code").read_text(), "new")
            atomic_exchange(a, b)
            self.assertEqual((a / "code").read_text(), "old")
            self.assertEqual(ledger.read_text(), '{"cursor":42}')


class UpdateAsyncTests(unittest.IsolatedAsyncioTestCase):
    async def test_corrupt_update_journal_preserves_plugin_and_blocks_switch(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            plugin = NS(context=NS(), config={}, alive=True)
            updates = Updates(plugin)
            updates.paths = lambda: (root, root / "install.json")
            (root / "install.json").write_text('{"state":')
            updates.recover_record()
            self.assertTrue(plugin.alive)
            self.assertEqual(updates.state, "recovery_required")
            with self.assertRaises(ValueError):
                updates.install()
            self.assertEqual((root / "install.json").read_text(), '{"state":')

    async def test_failed_download_cleanup_is_reported_without_stopping_plugin(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            (root / "code-abandoned").mkdir()
            plugin = NS(context=NS(), config={}, alive=True)
            updates = Updates(plugin)
            updates.paths = lambda: (root, root / "install.json")

            def failed_cleanup(*args, **kwargs):
                if not kwargs.get("ignore_errors"):
                    raise OSError("fixture permission error")

            with patch(
                "astrbot_plugin_dst_companion.updates.shutil.rmtree", side_effect=failed_cleanup
            ):
                await updates._download({})
            self.assertTrue(plugin.alive)
            self.assertEqual(updates.state, "failed")
            self.assertIsNone(updates.host.task)

    async def test_slow_download_cancel_keeps_running_plugin_and_removes_partial_file(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            plugin = NS(context=NS(), config={}, alive=True)
            updates = Updates(plugin)
            updates.paths = lambda: (root, root / "install.json")
            started = asyncio.Event()

            async def slow(*args, **kwargs):
                kwargs["output"].write(b"partial")
                started.set()
                await asyncio.Event().wait()

            manifest = {
                "expiresAt": time.time() + 60,
                "artifact": {"url": "https://example.invalid/plugin.zip", "size": 100},
            }
            with patch("astrbot_plugin_dst_companion.updates.fetch", slow):
                updates.download = asyncio.create_task(updates._download(manifest))
                await started.wait()
                await updates.cancel()
            self.assertTrue(plugin.alive)
            self.assertEqual(updates.state, "cancelled")
            self.assertEqual(list(root.iterdir()), [])

    async def test_real_astrbot_reload_survives_plugin_termination_and_failed_load_rolls_back(self):
        # Actual AstrBot 4.29 reload/termination, temporary code, no real bot/game I/O.
        from astrbot.core.star import star_manager
        from astrbot.core.star.star import StarMetadata

        for failure in (False, True, "timeout"):
            with tempfile.TemporaryDirectory() as d:
                root = Path(d)
                active, stage = root / "active", root / "stage"
                active.mkdir()
                stage.mkdir()
                (active / "main.py").write_text("old")
                (stage / "main.py").write_text("new")
                ledger = root / "state.json"
                ledger.write_text('{"cursor":9,"write":"uncertain"}')
                context = NS()
                module = ModuleType("dst_update_test_plugin")
                module.__file__ = str(active / "main.py")

                class Plugin:
                    __module__ = module.__name__

                    def __init__(self):
                        self.alive = True
                        self.plugin_version = "0.4.0"
                        self.handlers = set()
                        self.gateway = NS(pending={})
                        self.notifications = NS(close=AsyncMock(), reconcile=AsyncMock())
                        self.engine = NS(actions=NS(close=AsyncMock(), closed=False))

                    async def terminate(self, context=context, ledger=ledger):
                        self.alive = False
                        context.dst_companion_v1 = None
                        # Business state advances during handover and must not be restored.
                        ledger.write_text('{"cursor":10,"write":"uncertain"}')

                plugin = Plugin()
                context.dst_companion_v1 = plugin
                manager = object.__new__(star_manager.PluginManager)
                manager._pm_lock = asyncio.Lock()
                metadata = StarMetadata(
                    name="astrbot_plugin_dst_companion",
                    module_path=module.__name__,
                    star_cls_type=Plugin,
                    star_cls=plugin,
                )
                registry, mapping = [metadata], {module.__name__: metadata}

                async def unbind(*args, registry=registry, mapping=mapping):
                    registry.clear()
                    mapping.clear()

                async def load(
                    *args,
                    active=active,
                    failure=failure,
                    context=context,
                    plugin_type=Plugin,
                    **kwargs,
                ):
                    if (active / "main.py").read_text() == "new" and failure:
                        if failure == "timeout":
                            await asyncio.Event().wait()
                        return False, "fixture load failure"
                    new = plugin_type()
                    new.plugin_version = (
                        "0.4.1" if (active / "main.py").read_text() == "new" else "0.4.0"
                    )
                    context.dst_companion_v1 = new
                    return True, None

                manager._unbind_plugin = unbind
                manager.load = load
                context._star_manager = manager
                host = HostInstaller(context)
                with (
                    patch.dict(sys.modules, {module.__name__: module}),
                    patch.object(star_manager, "star_registry", registry),
                    patch.object(star_manager, "star_map", mapping),
                    patch("astrbot.core.star.star.star_registry", registry),
                    patch("astrbot_plugin_dst_companion.update_host.HOST_TIMEOUT", 0.1),
                ):
                    await host.start(
                        plugin,
                        stage,
                        root / "installation.json",
                        {"version": "0.4.1", "sequence": 5},
                    )
                self.assertFalse(plugin.alive)
                self.assertEqual(host.state, "rolled_back" if failure else "succeeded")
                self.assertEqual((active / "main.py").read_text(), "old" if failure else "new")
                self.assertEqual(json.loads(ledger.read_text())["cursor"], 10)
                self.assertEqual(json.loads(ledger.read_text())["write"], "uncertain")


class FullHostLifecycleTests(unittest.TestCase):
    def test_isolated_real_plugin_load_reload_and_rollback(self):
        import subprocess

        result = subprocess.run(
            [sys.executable, str(Path(__file__).with_name("host_reload_probe.py"))],
            capture_output=True,
            text=True,
            timeout=60,
        )
        self.assertEqual(result.returncode, 0, result.stdout[-4000:] + result.stderr[-2000:])
        self.assertIn("FULL_PLUGIN_RELOAD_AND_ROLLBACK_OK", result.stdout)

    def test_release_archive_contains_public_key_only_and_signed_version(self):
        import runpy

        root = Path(__file__).resolve().parents[1]
        private = Ed25519PrivateKey.generate()
        next_public = (
            Ed25519PrivateKey.generate()
            .public_key()
            .public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
        )
        seed = base64.b64encode(
            private.private_bytes(
                serialization.Encoding.Raw,
                serialization.PrivateFormat.Raw,
                serialization.NoEncryption(),
            )
        ).decode()
        with (
            tempfile.TemporaryDirectory() as d,
            patch.dict(
                os.environ,
                {
                    "ASTRBOT_UPDATE_SIGNING_KEY": seed,
                    "ASTRBOT_UPDATE_NEXT_PUBLIC_KEY": base64.b64encode(next_public).decode(),
                },
            ),
        ):
            out = Path(d)
            archive = runpy.run_path(str(root / "package.py"))["build"](out, "companion")
            signed = runpy.run_path(str(root / "sign_release.py"))["sign"](out, "v9.9.9", 1)
            with ZipFile(archive) as z:
                keys = json.loads(z.read("trusted_updates.json"))
                self.assertEqual(len(keys), 2)
                self.assertIn(base64.b64encode(next_public).decode(), keys.values())
                self.assertTrue(all(seed.encode() not in z.read(name) for name in z.namelist()))
            manifest = verify_manifest(
                signed.read_bytes(),
                keys,
                current="0.3.0",
                sequence=0,
                python="3.12.2",
                astrbot="4.29.3",
            )
            extract_verified(archive, out / "verified", manifest)
            self.assertEqual(manifest["version"], VERSION)
