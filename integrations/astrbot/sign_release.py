"""Create signed update metadata. Private key is read from a file or CI secret only."""

import argparse
import base64
import hashlib
import json
import os
import runpy
import time
from pathlib import Path

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

ROOT = Path(__file__).resolve().parent


def key_from_environment():
    encoded = os.environ.get("ASTRBOT_UPDATE_SIGNING_KEY", "").strip()
    return Ed25519PrivateKey.from_private_bytes(base64.b64decode(encoded, validate=True))


def public_keys():
    from cryptography.hazmat.primitives import serialization

    key = (
        key_from_environment()
        .public_key()
        .public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
    )
    key_id = hashlib.sha256(key).hexdigest()[:16]
    keys = {key_id: base64.b64encode(key).decode()}
    # Bootstrap a future root through a package signed by the current root.
    # The future private key is neither needed nor included in that package.
    next_key = os.environ.get("ASTRBOT_UPDATE_NEXT_PUBLIC_KEY", "").strip()
    if next_key:
        from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey

        raw = base64.b64decode(next_key, validate=True)
        Ed25519PublicKey.from_public_bytes(raw)
        keys[hashlib.sha256(raw).hexdigest()[:16]] = base64.b64encode(raw).decode()
    return keys


def sign(output, release, sequence):
    plugin = "astrbot_plugin_dst_companion"
    archive = output / (plugin + ".zip")
    version = runpy.run_path(str(ROOT / plugin / "version.py"))["VERSION"]
    now = int(time.time())
    payload = {
        "manifestVersion": 1,
        "plugin": plugin,
        "version": version,
        "sequence": sequence,
        "issuedAt": now,
        "expiresAt": now + 60 * 86400,
        "protocols": [1, 2],
        "python": ">=3.12,<3.14",
        "astrbot": ">=4.29.3,<4.30",
        "stateFormat": 1,
        "rollbackCompatible": True,
        "notes": "https://github.com/lcy0828/dst-admin-go/releases/tag/" + release,
        "artifact": {
            "url": "https://github.com/lcy0828/dst-admin-go/releases/download/"
            + release
            + "/"
            + archive.name,
            "size": archive.stat().st_size,
            "sha256": hashlib.sha256(archive.read_bytes()).hexdigest(),
        },
    }
    raw = json.dumps(payload, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode()
    envelope = {
        "keyId": next(iter(public_keys())),
        "payload": payload,
        "signature": base64.b64encode(key_from_environment().sign(raw)).decode(),
    }
    destination = output / "astrbot-plugin-release.json"
    destination.write_text(json.dumps(envelope, ensure_ascii=False, indent=2) + "\n")
    return destination


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--release", required=True)
    parser.add_argument("--sequence", type=int, required=True)
    args = parser.parse_args()
    import re

    if not re.fullmatch(r"v\d+\.\d+\.\d+", args.release) or args.sequence < 1:
        parser.error("only stable releases and positive sequences may be signed")
    print(sign(args.output, args.release, args.sequence))
