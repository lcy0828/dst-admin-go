"""Bounded temporary files for AstrBot's authenticated callback/file adapter."""

import os
import threading
import time
import uuid
from pathlib import Path

from .gateway import QueryError

_lock = threading.Lock()


def cache_file(resource, root=None):
    if root is None:
        from astrbot.api.star import StarTools
        root = StarTools.get_data_dir("astrbot_plugin_dst_companion") / "query_files"
    root = Path(root)
    raw = resource["data"]
    if len(raw) > 4 << 20:
        raise QueryError("RESOURCE_TOO_LARGE")
    name = Path(str(resource.get("name", "查询结果")).replace("\\", "/")).name
    name = "".join(c for c in name if c.isprintable())[:120] or "查询结果"
    with _lock:
        root.mkdir(parents=True, exist_ok=True, mode=0o700)
        size, count = 0, 0
        for file in root.iterdir():
            if file.is_symlink() or not file.is_file():
                continue
            stat = file.stat()
            if time.time() - stat.st_mtime > 86400:
                file.unlink()
            else:
                size += stat.st_size
                count += 1
        if count >= 64 or size + len(raw) > 32 << 20:
            raise QueryError("RESOURCE_CACHE_FULL", message="文件临时空间已满，请稍后再试。")
        path = root / (uuid.uuid4().hex + ".data")
        fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        try:
            with os.fdopen(fd, "wb") as file:
                file.write(raw)
        except BaseException:
            path.unlink(missing_ok=True)
            raise
    return path, name
