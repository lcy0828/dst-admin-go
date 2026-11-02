"""Build AstrBot-installable archives from reviewed source only."""

import argparse
import json
import os
import runpy
from pathlib import Path
from zipfile import ZIP_DEFLATED, ZipFile, ZipInfo

ROOT = Path(__file__).resolve().parent


def build(output, plugin):
    source = ROOT / ("astrbot_plugin_dst_" + plugin)
    destination = output / (source.name + ".zip")
    if plugin == "companion":
        version = runpy.run_path(str(source / "version.py"))["VERSION"]
        if "version: v" + version + "\n" not in (source / "metadata.yaml").read_text():
            raise ValueError("plugin version.py and metadata.yaml disagree")
    output.mkdir(parents=True, exist_ok=True)
    with ZipFile(destination, "w", compression=ZIP_DEFLATED) as archive:
        for path in sorted(source.iterdir()):
            if path.is_file() and (
                path.suffix == ".py"
                or path.name in {"metadata.yaml", "_conf_schema.json", "README.md", "LICENSE"}
            ):
                item = ZipInfo(path.name, date_time=(2026, 1, 1, 0, 0, 0))
                item.compress_type = ZIP_DEFLATED
                item.external_attr = 0o644 << 16
                data = path.read_bytes()
                if plugin == "broadcast" and path.name == "broadcast.py":
                    data = (ROOT / "astrbot_plugin_dst_companion" / "broadcast.py").read_bytes()
                archive.writestr(item, data)
        if plugin == "broadcast":
            for name in ("display.py", "message_templates.py"):
                item = ZipInfo(name, date_time=(2026, 1, 1, 0, 0, 0))
                item.compress_type = ZIP_DEFLATED
                item.external_attr = 0o644 << 16
                archive.writestr(item, (ROOT / "astrbot_plugin_dst_companion" / name).read_bytes())
        if plugin == "companion":
            # Reuse the panel's authoritative display metadata; no hand-kept
            # bot dictionary or per-item network round trips.
            item = ZipInfo("vanilla_items.json", date_time=(2026, 1, 1, 0, 0, 0))
            item.compress_type = ZIP_DEFLATED
            item.external_attr = 0o644 << 16
            archive.writestr(
                item, (ROOT.parents[1] / "internal/entitycatalog/data/vanilla.json").read_bytes()
            )
        if plugin == "companion":
            # Root keys are packaged out-of-band, never trusted from a downloaded manifest.
            keys = {}
            if os.environ.get("ASTRBOT_UPDATE_SIGNING_KEY"):
                from sign_release import public_keys
                keys = public_keys()
            item = ZipInfo("trusted_updates.json", date_time=(2026, 1, 1, 0, 0, 0))
            item.compress_type = ZIP_DEFLATED
            item.external_attr = 0o644 << 16
            archive.writestr(item, json.dumps(keys, sort_keys=True) + "\n")
    return destination


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--plugin", choices=("companion", "broadcast", "all"), default="companion")
    args = parser.parse_args()
    for plugin in ("companion", "broadcast") if args.plugin == "all" else (args.plugin,):
        print(build(args.output, plugin))
