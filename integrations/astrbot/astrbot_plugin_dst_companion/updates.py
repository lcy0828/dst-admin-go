"""Opt-in update checks and cancelable downloads; no model or unsolicited QQ sends."""

import asyncio
import json
import random
import shutil
import sys
import time
import uuid
from importlib.metadata import version
from pathlib import Path
from urllib.parse import urljoin

import httpx

from .update_host import HostInstaller, code_digest, save_record
from .update_security import MAX_EXPANDED, extract_verified, https_url, verify_manifest
from .version import VERSION

MANIFEST_URL = (
    "https://github.com/lcy0828/dst-admin-go/releases/latest/download/astrbot-plugin-release.json"
)


async def fetch(url, *, maximum, proxy="", output=None):
    https_url(url)
    if proxy and not proxy.startswith(("http://", "https://", "socks5://")):
        raise ValueError("更新代理格式无效")
    size, chunks = 0, []
    async with httpx.AsyncClient(
        proxy=proxy or None,
        trust_env=False,
        follow_redirects=False,
        timeout=httpx.Timeout(30, connect=5),
        limits=httpx.Limits(max_connections=1),
    ) as client:
        for _ in range(5):
            async with client.stream("GET", url) as response:
                if response.status_code in {301, 302, 303, 307, 308}:
                    url = https_url(urljoin(url, response.headers.get("location", "")))
                    continue
                response.raise_for_status()
                async for chunk in response.aiter_bytes():
                    size += len(chunk)
                    if size > maximum:
                        raise ValueError("更新下载超过大小限制")
                    if output:
                        output.write(chunk)
                    else:
                        chunks.append(chunk)
                return b"".join(chunks)
    raise ValueError("更新地址跳转次数过多")


class Updates:
    def __init__(self, plugin):
        self.plugin = plugin
        self.worker = None
        self.download = None
        self.lock = asyncio.Lock()
        self.manifest = None
        self.state = "idle"
        self.message = "尚未检查插件版本"
        self.last_check = 0
        self.recovery_error = False
        host = getattr(plugin.context, "dst_update_host_v1", None)
        if host is None:
            host = HostInstaller(plugin.context)
            plugin.context.dst_update_host_v1 = host
        self.host = host

    def settings(self):
        value = self.plugin.config.get("plugin_updates", {})
        return value if isinstance(value, dict) else {}

    def paths(self):
        from astrbot.api.star import StarTools

        root = StarTools.get_data_dir("astrbot_plugin_dst_companion") / "updates"
        return root, root / "installation.json"

    def recover_record(self):
        try:
            self._recover_record()
        except (OSError, ValueError, TypeError, AttributeError):
            # A broken update journal must not prevent ordinary queries or
            # notification recovery. Preserve it and block further switches.
            self.recovery_error = True
            self.state, self.message = "recovery_required", "插件更新记录需要检查，当前插件继续运行"

    def _recover_record(self):
        # Called only after the loaded plugin has validated its stable state.
        # A process interruption never leaves the active code directory absent.
        root, path = self.paths()
        if not path.exists() or self.host.task and not self.host.task.done():
            return
        record = json.loads(path.read_text())
        if record.get("state") not in {"prepared", "loading"}:
            return
        active = Path(__file__).resolve().parent
        if str(active) != record.get("active"):
            return
        digest = code_digest(active)
        if VERSION == record.get("version") and digest == record.get("installedDigest"):
            record["state"] = "succeeded"
        elif VERSION == record.get("previousVersion") and digest == record.get("backupDigest"):
            record["state"] = "deferred"
        else:
            record["state"] = "recovery_required"
        save_record(path, record)

    def start(self):
        if self.settings().get("mode", "manual") in {"notify", "auto"} and self.worker is None:
            self.worker = asyncio.create_task(self.loop())

    async def loop(self):
        while self.plugin.alive and self.settings().get("mode", "manual") != "manual":
            try:
                if self.plugin.config.get("enabled") and self.plugin.server():
                    await self.check()
                    if (
                        self.state == "available"
                        and self.settings().get("mode") == "auto"
                        and self.host.supported()
                    ):
                        _, record = self.paths()
                        saved = json.loads(record.read_text()) if record.exists() else {}
                        if saved.get("holdSequence", 0) < self.manifest["sequence"] and (
                            saved.get("state") not in {"rolled_back", "recovery_required"}
                            or saved.get("sequence") != self.manifest["sequence"]
                        ):
                            self.install()
            except Exception:
                self.state, self.message = "check_failed", "插件版本检查未完成；当前版本继续运行"
            await asyncio.sleep(random.uniform(5.5, 6.5) * 3600)

    async def check(self):
        async with self.lock:
            if self.recovery_error:
                return
            root, _ = self.paths()
            root.mkdir(parents=True, exist_ok=True)
            keys_path = Path(__file__).with_name("trusted_updates.json")
            keys = json.loads(keys_path.read_text()) if keys_path.exists() else {}
            if not keys:
                self.state, self.message = (
                    "manual_required",
                    "此安装包尚未配置发布验签公钥，请维护者先安装带验签信息的正式包",
                )
                return
            if self.download and not self.download.done():
                self.message = "正在下载；可取消下载后更换代理"
                return
            sequence_path = root / "sequence.json"
            old = json.loads(sequence_path.read_text()) if sequence_path.exists() else {}
            async with asyncio.timeout(40):
                raw = await fetch(
                    self.settings().get("manifestUrl") or MANIFEST_URL,
                    maximum=64 << 10,
                    proxy=self.settings().get("proxy", ""),
                )
            manifest = verify_manifest(
                raw,
                keys,
                current=VERSION,
                sequence=old.get("sequence", 0),
                python=".".join(map(str, sys.version_info[:3])),
                astrbot=version("astrbot"),
            )
            # Equal sequence must represent the identical signed release, not a replacement.
            if (
                old.get("sequence") == manifest["sequence"]
                and old.get("artifact") != manifest["artifact"]["sha256"]
            ):
                raise ValueError("同一发布序号的文件发生变化")
            save_record(
                sequence_path,
                {"sequence": manifest["sequence"], "artifact": manifest["artifact"]["sha256"]},
            )
            self.manifest, self.last_check = manifest, time.time()
            self.state = "available" if manifest["version"] != VERSION else "current"
            self.message = (
                "插件可更新至 " + manifest["version"]
                if self.state == "available"
                else "插件已是最新版本"
            )

    def install(self):
        if self.recovery_error:
            raise ValueError("插件更新记录需要检查")
        _, saved_path = self.paths()
        if saved_path.exists() and json.loads(saved_path.read_text()).get("state") in {
            "prepared",
            "loading",
            "recovery_required",
        }:
            raise ValueError("上次切换需要核对，请先查看插件更新状态")
        if not self.manifest or self.state != "available":
            raise ValueError("请先检查并确认有兼容新版本")
        if not self.host.supported():
            raise ValueError("当前宿主未验证自动切换；请使用 AstrBot 手动更新")
        if self.host.task and not self.host.task.done():
            raise ValueError("正在切换插件，请稍后查看更新状态")
        if self.download and not self.download.done():
            raise ValueError("已有下载正在执行")
        self.download = asyncio.create_task(self._download(dict(self.manifest)))

    async def _download(self, manifest):
        root, record = self.paths()
        stage = root / ("code-" + uuid.uuid4().hex)
        archive = root / ("download-" + uuid.uuid4().hex + ".zip")
        self.state, self.message = "downloading", "正在下载插件；可以取消下载"
        handed_off = False
        try:
            saved = json.loads(record.read_text()) if record.exists() else {}
            keep = saved.get("backup", "")
            # Keep one recorded rollback copy, remove only abandoned staging.
            for path in root.glob("code-*"):
                if str(path) != keep and path.is_dir() and not path.is_symlink():
                    shutil.rmtree(path)
            for path in root.glob("download-*.zip"):
                path.unlink(missing_ok=True)
            if manifest["expiresAt"] <= time.time():
                raise ValueError("版本信息已过期，请重新检查")
            if shutil.disk_usage(root).free < MAX_EXPANDED * 3:
                raise ValueError("更新所需磁盘空间不足")
            async with asyncio.timeout(900):
                with archive.open("wb") as out:
                    await fetch(
                        manifest["artifact"]["url"],
                        maximum=manifest["artifact"]["size"],
                        proxy=self.settings().get("proxy", ""),
                        output=out,
                    )
            extract_verified(archive, stage, manifest)
            self.host.start(self.plugin, stage, record, manifest)
            handed_off = True
            self.state, self.message = "installing", "下载完成，正在安全切换插件；游戏世界继续运行"
        except asyncio.CancelledError:
            self.state, self.message = "cancelled", "下载已取消，当前插件继续运行"
            raise
        except Exception as error:
            self.state, self.message = (
                "failed",
                "插件更新未完成（" + type(error).__name__ + "），保留当前版本",
            )
        finally:
            try:
                archive.unlink(missing_ok=True)
            except OSError:
                pass  # Retained partial file is cleaned before the next download.
            if not handed_off:
                shutil.rmtree(stage, ignore_errors=True)

    async def cancel(self):
        if self.host.task and not self.host.task.done():
            return "已进入切换阶段，暂不能取消；正在完成加载检查或恢复"
        if self.download and not self.download.done():
            self.download.cancel()
            await asyncio.gather(self.download, return_exceptions=True)
            return "下载已取消，可修改代理后重新检查更新"
        return "当前没有正在下载的插件更新"

    def rollback(self):
        if self.recovery_error:
            raise ValueError("插件更新记录需要检查")
        _, record = self.paths()
        saved = json.loads(record.read_text()) if record.exists() else {}
        backup = Path(saved.get("backup", ""))
        if (
            saved.get("state") != "succeeded"
            or not saved.get("previousVersion")
            or not backup.is_dir()
            or code_digest(backup) != saved.get("backupDigest")
        ):
            raise ValueError("没有可验证的上一版插件代码")
        self.host.start(
            self.plugin,
            backup,
            record,
            {
                "version": saved["previousVersion"],
                "sequence": saved["sequence"],
                "holdSequence": saved["sequence"],
            },
        )
        self.message = "正在恢复上一版代码，任务和播报记录保持连续"

    async def handle(self, event, command, reply):
        if not event.is_admin():
            await reply("插件程序更新由机器人维护者管理。")
            return
        try:
            if command == "检查插件更新":
                await self.check()
            elif command == "安装插件更新":
                self.install()
                await reply("已开始下载插件，可用 /dst 取消插件下载 取消。")
                return
            elif command == "恢复上一版插件":
                self.rollback()
                await reply(self.message)
                return
            elif command == "取消插件下载":
                await reply(await self.cancel())
                return
            root, record = self.paths()
            saved = json.loads(record.read_text()) if record.exists() else {}
            await reply(
                "插件 "
                + VERSION
                + " · "
                + self.message
                + "\n安装状态："
                + str(saved.get("state", self.host.state))
            )
        except Exception as error:
            await reply("插件更新未完成（" + type(error).__name__ + "）；当前版本保留。")

    async def close(self):
        # The host-owned installer is deliberately NOT canceled here.
        tasks = [t for t in (self.worker, self.download) if t and not t.done()]
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
