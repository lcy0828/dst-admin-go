"""Host-owned install task survives unloading the plugin; business state never rewinds."""

import asyncio
import ctypes
import hashlib
import json
import os
import sys
from pathlib import Path

from .update_security import PLUGIN

HOST_TIMEOUT = 60


def atomic_exchange(a, b):
    """Both paths always exist, including if the process stops during switching."""
    libc = ctypes.CDLL(None, use_errno=True)
    left, right = os.fsencode(a), os.fsencode(b)
    if sys.platform == "linux" and hasattr(libc, "renameat2"):
        status = libc.renameat2(-100, left, -100, right, 2)  # RENAME_EXCHANGE
    elif sys.platform == "darwin" and hasattr(libc, "renamex_np"):
        status = libc.renamex_np(left, right, 2)  # RENAME_SWAP
    else:
        raise RuntimeError("当前系统不支持安全切换；请在 AstrBot 中手动更新")
    if status:
        raise OSError(ctypes.get_errno(), "atomic plugin exchange failed")
    try:
        for parent in {Path(a).parent, Path(b).parent}:
            fd = os.open(parent, os.O_RDONLY)
            try:
                os.fsync(fd)
            finally:
                os.close(fd)
    except OSError as error:
        error.exchanged = True
        raise


def save_record(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(".tmp")
    with temporary.open("w") as out:
        json.dump(value, out, ensure_ascii=False)
        out.flush()
        os.fsync(out.fileno())
    os.replace(temporary, path)


def code_digest(root):
    value = hashlib.sha256()
    for path in sorted(Path(root).iterdir()):
        if path.is_file():
            value.update(path.name.encode())
            value.update(hashlib.sha256(path.read_bytes()).digest())
    return value.hexdigest()


class HostInstaller:
    def __init__(self, context):
        self.context = context
        self.task = None
        self.state = "idle"
        self.error = ""

    def supported(self):
        from importlib.metadata import version

        from packaging.version import Version

        manager = getattr(self.context, "_star_manager", None)
        return (
            Version("4.29.3") <= Version(version("astrbot")) < Version("4.30")
            and manager is not None
            and hasattr(manager, "_pm_lock")
            and sys.platform in {"linux", "darwin"}
        )

    def start(self, plugin, stage, record_path, manifest):
        if self.task and not self.task.done():
            raise RuntimeError("已有更新正在处理")
        if not self.supported():
            raise RuntimeError("此 AstrBot 环境未验证自动切换，请使用手动更新")
        self.task = asyncio.create_task(self.install(plugin, stage, record_path, manifest))
        self.task.add_done_callback(self.completed)
        return self.task

    def completed(self, task):
        if task.cancelled():
            self.state, self.error = "recovery_required", "CancelledError"
        elif error := task.exception():
            self.state, self.error = "recovery_required", type(error).__name__

    async def install(self, plugin, stage, record_path, manifest):
        active = Path(sys.modules[plugin.__class__.__module__].__file__).resolve().parent
        manager = self.context._star_manager
        record = {
            "state": "prepared",
            "active": str(active),
            "backup": str(stage),
            "version": manifest["version"],
            "sequence": manifest["sequence"],
            "previousVersion": plugin.plugin_version,
            "backupDigest": code_digest(active),
            "installedDigest": code_digest(stage),
            "holdSequence": manifest.get("holdSequence", 0),
        }
        swapped = False
        try:
            self.state = "waiting"
            plugin.updating = True
            # Do not wait for game jobs: their durable handles are already in the panel.
            async with asyncio.timeout(10):
                while plugin.handlers or plugin.gateway.pending:
                    await asyncio.sleep(0.1)
            async with asyncio.timeout(10):
                await plugin.notifications.close()
                await plugin.engine.actions.close()
            save_record(record_path, record)
            from astrbot.core.star.star import star_registry

            if manager._pm_lock.locked() or not any(
                s.name == PLUGIN and s.star_cls is plugin for s in star_registry
            ):
                raise RuntimeError("宿主正在管理插件，稍后再更新")
            self.state = "switching"
            atomic_exchange(active, stage)
            swapped = True
            save_record(record_path, {**record, "state": "loading"})
            async with asyncio.timeout(HOST_TIMEOUT):
                success, _ = await manager.reload(PLUGIN)
            new = getattr(self.context, "dst_companion_v1", None)
            if not success or new is plugin or new is None or not new.alive:
                raise RuntimeError("新版加载检查失败")
            from .version import VERSION

            if getattr(new, "plugin_version", VERSION) != manifest["version"]:
                raise RuntimeError("加载版本不符")
            self.state = "succeeded"
            save_record(record_path, {**record, "state": "succeeded"})
        except Exception as error:
            swapped = swapped or getattr(error, "exchanged", False)
            self.error = type(error).__name__  # Do not expose tokens/URLs from exception messages.
            if swapped:
                self.state = "rolling_back"
                try:
                    from astrbot.core.star.star import star_registry

                    current = getattr(self.context, "dst_companion_v1", None)
                    if current:
                        async with asyncio.timeout(10):
                            await current.terminate()
                    atomic_exchange(active, stage)
                    # reload(name) reloads ALL plugins when its name is absent. Never do that.
                    async with asyncio.timeout(HOST_TIMEOUT):
                        if any(s.name == PLUGIN for s in star_registry):
                            ok, _ = await manager.reload(PLUGIN)
                        else:
                            async with manager._pm_lock:
                                ok, _ = await manager.load(specified_dir_name=active.name)
                    self.state = "rolled_back" if ok else "recovery_required"
                except Exception:
                    self.state = "recovery_required"
            else:
                self.state = "deferred"
                plugin.updating = False
                if plugin.alive:
                    # Consumers were stopped only after the bounded drain; resume original instance.
                    plugin.engine.actions.closed = False
                    async with asyncio.timeout(10):
                        await plugin.notifications.reconcile()
            save_record(record_path, {**record, "state": self.state, "error": self.error})
        finally:
            current = getattr(self.context, "dst_companion_v1", None)
            if current:
                current.updating = False
