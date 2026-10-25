#!/usr/bin/env python3
"""Package a built, pinned upstream checkout for the node-owned installer."""

import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import tempfile
import zipfile


def sha256(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    recipe = Path(__file__).resolve().parent
    source = args.source.resolve()
    metadata = json.loads((recipe / "source.json").read_text())
    mod = source / "Mod"
    required = (
        "modinfo.lua", "modmain.lua", "modworldgenmain.lua",
        "inject_server_only_mod.lua", "plugins/init.lua", "libInjector.so",
        "bin64/linux/lib64/libInjector.so",
        "plugins/plugin_core_vm/plugin_core_vm.so",
        "deps/liblua51.so", "deps/liblua51DS.so", "deps/liblua51DS_gengc.so",
        "deps/libfrida-gum.so",
    )
    for name in required:
        if not (mod / name).is_file():
            parser.error(f"missing built file: {name}")
    version = re.search(r'^version\s*=\s*"([0-9.]+)"', (mod / "modinfo.lua").read_text(), re.M)
    if not version or version[1] != metadata["version"]:
        parser.error("built version differs from source.json")

    files = {}
    for path in sorted(mod.rglob("*")):
        relative = path.relative_to(mod)
        if not path.is_file():
            continue
        if any(p in {"windows", "osx", ".git", "__pycache__"} for p in relative.parts):
            continue
        # A reused checkout can retain outputs from older CMake target lists.
        if any(p in {"plugin_network_sim", "plugin_sim_lagcomp"} for p in relative.parts):
            continue
        if path.suffix.lower() in {".dll", ".pdb", ".exe", ".dylib", ".bat", ".pkg"}:
            continue
        if not path.resolve().is_relative_to(source):
            parser.error(f"file resolves outside the source tree: {relative}")
        files[relative.as_posix()] = path.read_bytes()  # Dereference library aliases.

    patches = {p.name: sha256(p) for p in sorted(recipe.glob("*.patch"))}
    provenance = dict(
        version=metadata["version"], revision=metadata["revision"],
        upstream="fesily/DontStarveLuaJIT2", upstreamCommit=metadata["commit"],
        upstreamTag=metadata["upstreamTag"], luajitCommit=metadata["luajitCommit"],
        vcpkgCommit=metadata["vcpkgCommit"], fridaCommit=metadata["fridaCommit"],
        platform="linux-amd64", distribution="compatibility", baseline="debian-12",
        sourceDateEpoch=metadata["sourceDateEpoch"], patches=patches,
    )
    files["dst_admin_build.json"] = (json.dumps(provenance, indent=2) + "\n").encode()
    license_roots = {
        "injector": source, "luajit": source / "luajit",
        "frida-gum": source / "3rd/frida-gum-src", "nucleus": source / "3rd/nucleus",
        "lua51": source / "src/lua51", "lua51original": source / "src/lua51original",
    }
    for name, root in license_roots.items():
        for path in sorted(root.iterdir()):
            if path.is_file() and path.name.lower().startswith(("license", "copyright", "copying")):
                files[f"licenses/{name}/{path.name}"] = path.read_bytes()
    dependencies = source / "builds/ninja-multi-vcpkg/vcpkg_installed/x64-linux-custom/share"
    for path in sorted(dependencies.glob("*/copyright")):
        files["licenses/dependencies/" + path.relative_to(dependencies).as_posix()] = path.read_bytes()
    for name in ("injector", "luajit", "frida-gum"):
        if not any(p.startswith(f"licenses/{name}/") for p in files):
            parser.error(f"missing {name} license")

    stamp = datetime.datetime.fromtimestamp(metadata["sourceDateEpoch"], datetime.timezone.utc).timetuple()[:6]
    args.output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(dir=args.output.parent, suffix=".zip", delete=False) as temporary:
        temporary_path = Path(temporary.name)
    try:
        with zipfile.ZipFile(temporary_path, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
            for name, data in sorted(files.items()):
                info = zipfile.ZipInfo("Mod/" + name, stamp)
                info.create_system = 3
                info.external_attr = (0o100755 if data.startswith(b"\x7fELF") else 0o100644) << 16
                info.compress_type = zipfile.ZIP_DEFLATED
                archive.writestr(info, data, compresslevel=9)
        os.replace(temporary_path, args.output)
    finally:
        temporary_path.unlink(missing_ok=True)
    digest = sha256(args.output)
    manifest = [{"file": args.output.name, "release": dict(
        version=metadata["version"], revision=metadata["revision"], sha256=digest,
        size=args.output.stat().st_size, id=digest, os="linux", arch="amd64",
        channel="compatibility",
    )}]
    args.output.with_suffix(".manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    print(json.dumps(manifest, indent=2))


if __name__ == "__main__":
    main()
