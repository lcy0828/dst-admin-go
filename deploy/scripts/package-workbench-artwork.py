#!/usr/bin/env python3
"""Build a deterministic optional artwork archive from an exported WebP catalog.

Usage: package-workbench-artwork.py SOURCE_DIRECTORY OUTPUT.tar.gz VERSION
SOURCE_DIRECTORY contains catalog.json and images/*.webp. No game scripts,
saves, local previews or export metadata are included.
"""
import gzip
import hashlib
import io
import json
from pathlib import Path
import re
import sys
import tarfile


def main():
    source, output, version = Path(sys.argv[1]), Path(sys.argv[2]), sys.argv[3]
    catalog_bytes = (source / 'catalog.json').read_bytes()
    catalog = json.loads(catalog_bytes)
    paths = sorted({row['image'] for row in catalog['items'] if row.get('image')})
    assert len(catalog['items']) == catalog['entities']
    assert sum(bool(row.get('image')) for row in catalog['items']) == catalog['illustrated']
    readme = f'''DST Admin optional vanilla artwork pack {version}

Game build: {catalog['gameVersion']}
Catalog entries: {catalog['entities']}; illustrated entities: {catalog['illustrated']}; unique images: {len(paths)}.
Artwork and game data originate from Don't Starve Together by Klei Entertainment.
Game website: https://www.klei.com/games/dont-starve-together
Panel project: https://github.com/lcy0828/dst-admin-go

Install/import/uninstall through Game workbench > Artwork pack. The panel verifies
the release checksum before installation. Installation is optional, takes effect
immediately and changes neither game files nor saves. This is display artwork,
not the game server, a mod, or the complete set of game animations and skins.
WebP images keep their source dimensions and transparency. Some registered
entities have no matching image. World/mod catalogs still reflect the running game.

图片与数据来自 Klei Entertainment 的《饥荒联机版》。
在「游戏工作台 → 图片资源包」中安装、导入或卸载，立即生效。
图片包仅用于面板展示，不包含游戏服务端、存档或完整动画和皮肤。
尚未覆盖全部实体与模组图片；游戏内可用实体以目标世界实际加载内容为准。
'''.encode()
    files = {'catalog.json': catalog_bytes, 'README.txt': readme}
    for name in paths:
        assert re.fullmatch(r'images/[a-f0-9]{24}\.webp', name), name
        data = (source / name).read_bytes()
        assert data[:4] == b'RIFF' and data[8:12] == b'WEBP', name
        files[name] = data
    output.parent.mkdir(parents=True, exist_ok=True)
    with output.open('wb') as raw, gzip.GzipFile(fileobj=raw, filename='', mode='wb', mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode='w', format=tarfile.USTAR_FORMAT) as archive:
            for name, data in sorted(files.items()):
                info = tarfile.TarInfo('workbench/' + name)
                info.size, info.mode, info.mtime = len(data), 0o644, 0
                archive.addfile(info, io.BytesIO(data))
    summary = {'version': version, 'gameVersion': catalog['gameVersion'], 'entities': catalog['entities'],
               'illustrated': catalog['illustrated'], 'images': len(paths),
               'installedBytes': sum(map(len, files.values())), 'archiveBytes': output.stat().st_size,
               'sha256': hashlib.sha256(output.read_bytes()).hexdigest()}
    output.with_suffix(output.suffix + '.json').write_text(json.dumps(summary, indent=2) + '\n')
    output.with_suffix(output.suffix + '.sha256').write_text(summary['sha256'] + '  ' + output.name + '\n')
    print(json.dumps(summary))


if __name__ == '__main__':
    main()
