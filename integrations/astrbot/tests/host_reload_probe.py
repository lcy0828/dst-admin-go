"""Run in an isolated subprocess: real plugin import, init, unbind, reload, rollback."""

import asyncio
import copy
import json
import os
import shutil
import sys
import tempfile
from pathlib import Path
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock, patch

SOURCE = Path(__file__).resolve().parents[1] / "astrbot_plugin_dst_companion"


async def probe(root):
    from astrbot.core import sp
    from astrbot.core.provider.register import llm_tools
    from astrbot.core.star.star_manager import PluginManager
    from astrbot.core.utils.plugin_kv_store import PluginKVStoreMixin

    plugin_name = "astrbot_plugin_dst_companion"
    active = root / "data" / "plugins" / plugin_name
    shutil.copytree(SOURCE, active, ignore=shutil.ignore_patterns("__pycache__"))
    configs = root / "data" / "config"
    configs.mkdir(parents=True)
    (configs / (plugin_name + "_config.json")).write_text(json.dumps({
        "enabled": True, "configuration_version": 3, "plugin_updates": {"mode": "manual"},
    }))
    context = NS(get_llm_tool_manager=lambda: llm_tools, get_registered_star=lambda name: None)
    manager = PluginManager(context, {})
    manager.reserved_plugin_path = str(root / "no_builtin_plugins")
    ledger = {}

    async def get(self, key, default):
        return copy.deepcopy(ledger.get(key, default))

    async def put(self, key, value):
        ledger[key] = copy.deepcopy(value)

    with patch.object(sp, "global_get", AsyncMock(side_effect=lambda key, default=None: default)), \
         patch.object(PluginKVStoreMixin, "get_kv_data", get), \
         patch.object(PluginKVStoreMixin, "put_kv_data", put):
        loaded, error = await manager.load(specified_dir_name=plugin_name)
        assert loaded, error
        original = context.dst_companion_v1
        assert original.plugin_version == "0.4.0"
        ledger["actions_v1"] = {"fixture": {"state": "uncertain"}}
        ledger["probe_cursor"] = 23
        updates = original.updates
        update_root, record = updates.paths()
        update_root.mkdir(parents=True, exist_ok=True)
        stage = update_root / "code-valid"
        shutil.copytree(active, stage, ignore=shutil.ignore_patterns("__pycache__"))
        for filename in ("version.py", "metadata.yaml"):
            file = stage / filename
            file.write_text(file.read_text().replace("0.4.0", "0.4.1"))
        host = updates.host
        await host.start(original, stage, record, {"version": "0.4.1", "sequence": 5})
        assert host.state == "succeeded", (host.state, host.error)
        current = context.dst_companion_v1
        assert current is not original and not original.alive and current.plugin_version == "0.4.1"
        assert current.updates.host is host
        assert await current.get_kv_data("probe_cursor", 0) == 23
        ledger["probe_cursor"] = 24
        broken = update_root / "code-broken"
        shutil.copytree(active, broken, ignore=shutil.ignore_patterns("__pycache__"))
        for filename in ("version.py", "metadata.yaml"):
            file = broken / filename
            file.write_text(file.read_text().replace("0.4.1", "0.4.2"))
        file = broken / "main.py"
        file.write_text(file.read_text().replace("    async def initialize(self):", "    async def initialize(self):\n        raise RuntimeError('fixture initialize failure')"))
        await host.start(current, broken, record, {"version": "0.4.2", "sequence": 6})
        assert host.state == "rolled_back", (host.state, host.error)
        recovered = context.dst_companion_v1
        assert recovered.plugin_version == "0.4.1" and recovered.alive
        assert await recovered.get_kv_data("probe_cursor", 0) == 24
        assert (await recovered.load_journal())["fixture"]["state"] == "uncertain"
        await recovered.terminate()
        print("FULL_PLUGIN_RELOAD_AND_ROLLBACK_OK")


if __name__ == "__main__":
    with tempfile.TemporaryDirectory(prefix="dst-host-probe-") as temporary:
        root = Path(temporary)
        os.environ["ASTRBOT_ROOT"] = str(root)
        os.chdir(root)
        sys.path.insert(0, str(root))
        asyncio.run(probe(root))
