"""Verify signed release metadata before trusting any download location or code."""

import base64
import hashlib
import json
import re
import time
from pathlib import Path
from urllib.parse import urlsplit
from zipfile import ZipFile

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
from packaging.specifiers import SpecifierSet
from packaging.version import Version

PLUGIN = "astrbot_plugin_dst_companion"
MAX_ARCHIVE = 16 << 20
MAX_EXPANDED = 48 << 20


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()


def https_url(value):
    u = urlsplit(value)
    if (
        u.scheme != "https"
        or not u.hostname
        or u.username
        or u.password
        or u.fragment
        or len(value) > 2048
    ):
        raise ValueError("更新地址必须使用 HTTPS 且不含凭据")
    return value


def verify_manifest(raw, keys, *, current, sequence, python, astrbot, now=None):
    if len(raw) > 64 << 10:
        raise ValueError("更新清单过大")
    envelope = json.loads(raw)
    if not isinstance(envelope, dict) or set(envelope) != {"keyId", "payload", "signature"}:
        raise ValueError("更新清单无效")
    key = keys.get(envelope["keyId"])
    if not key:
        raise ValueError("更新签名密钥不受信任；请维护者安装受信版本")
    payload = envelope["payload"]
    Ed25519PublicKey.from_public_bytes(base64.b64decode(key, validate=True)).verify(
        base64.b64decode(envelope["signature"], validate=True), canonical(payload)
    )
    now = time.time() if now is None else now
    if (
        payload.get("plugin") != PLUGIN
        or payload.get("manifestVersion") != 1
        or type(payload.get("sequence")) is not int
        or payload["sequence"] < sequence
        or not isinstance(payload.get("issuedAt"), (int, float))
        or not isinstance(payload.get("expiresAt"), (int, float))
        or not now - 90 * 86400 <= payload["issuedAt"] <= now + 300
        or not now < payload["expiresAt"] <= payload["issuedAt"] + 90 * 86400
    ):
        raise ValueError("更新清单过期或版本倒退")
    release = payload.get("version", "")
    if not re.fullmatch(r"\d+\.\d+\.\d+", release) or Version(release) < Version(current):
        raise ValueError("只接受正式版且不能降级")
    if (
        Version(python) not in SpecifierSet(payload["python"])
        or Version(astrbot) not in SpecifierSet(payload["astrbot"])
        or payload.get("stateFormat") != 1
        or payload.get("rollbackCompatible") is not True
        or not set(payload.get("protocols", [])) & {1, 2}
    ):
        raise ValueError("当前宿主或数据格式不兼容；保留原版本")
    artifact = payload.get("artifact", {})
    if (
        type(artifact.get("size")) is not int
        or not 0 < artifact["size"] <= MAX_ARCHIVE
        or not re.fullmatch(r"[a-f0-9]{64}", str(artifact.get("sha256", "")))
    ):
        raise ValueError("更新文件信息无效")
    https_url(artifact["url"])
    return payload


def extract_verified(archive, destination, manifest):
    artifact = manifest["artifact"]
    if archive.stat().st_size != artifact["size"]:
        raise ValueError("更新文件大小不匹配")
    if hashlib.sha256(archive.read_bytes()).hexdigest() != artifact["sha256"]:
        raise ValueError("更新文件校验失败")
    destination.mkdir(parents=True, exist_ok=False)
    with ZipFile(archive) as z:
        names, total = set(), 0
        for entry in z.infolist():
            name = entry.filename
            mode = entry.external_attr >> 16
            if (
                name != Path(name).name
                or name in {"", ".", ".."}
                or "\\" in name
                or name in names
                or entry.is_dir()
                or mode & 0o170000 == 0o120000
                or not (
                    name.endswith(".py")
                    or name
                    in {
                        "metadata.yaml",
                        "_conf_schema.json",
                        "README.md",
                        "LICENSE",
                        "vanilla_items.json",
                        "trusted_updates.json",
                    }
                )
            ):
                raise ValueError("更新压缩包包含不允许的路径或文件")
            names.add(name)
            total += entry.file_size
            if len(names) > 128 or total > MAX_EXPANDED:
                raise ValueError("更新压缩包展开后过大")
            data = z.read(entry)
            if name.endswith(".py"):
                compile(data, name, "exec")  # Syntax check only; never execute during preflight.
            (destination / name).write_bytes(data)
        if (
            not {"main.py", "metadata.yaml", "version.py", "__init__.py", "_conf_schema.json"}
            <= names
        ):
            raise ValueError("更新文件不完整")
    import yaml

    metadata = yaml.safe_load((destination / "metadata.yaml").read_text())
    if metadata.get("name") != PLUGIN or metadata.get("version") != "v" + manifest["version"]:
        raise ValueError("插件身份或版本不匹配")
