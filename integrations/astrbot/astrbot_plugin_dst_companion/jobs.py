"""Persistent submit-before-send journal. Recovery only reads original calls/jobs."""

import asyncio
import hashlib
import json
import time
import uuid

from .formatters import safe
from .gateway import QueryError
from .presentation import business


def outcome(data):
    status = data.get("status")
    if status in {"queued", "running", "canceling"}:
        return "正在执行" if status != "queued" else "已排队"
    if status == "succeeded" and data.get("outcome") == "full":
        return "已完成"
    if data.get("outcome") == "partial":
        return "部分完成"
    if status in {"failed", "canceled"}:
        return "失败" if status == "failed" else "已取消"
    if data.get("outcome") in {"partial", "partial_success"}:
        return "部分完成"
    return "结果尚未确认"


def terminal(data):
    return data.get("status") in {"succeeded", "failed", "canceled", "rejected"} or (
        data.get("status") == "partial" and data.get("nextAction") == "none"
    )


class Actions:
    def __init__(
        self,
        gateway,
        load,
        save,
        notify=None,
        poll_interval=5,
        tracking_seconds=300,
        directory=None,
    ):
        self.gateway, self.load, self.save = gateway, load, save
        self.directory = directory
        self.lock = asyncio.Lock()
        self.notify = notify
        self.poll_interval = poll_interval
        self.tracking_seconds = tracking_seconds
        self.watches = {}
        self.worker = None
        self.closed = False

    async def submit(self, *args):
        async with self.lock:
            return await self._submit(*args)

    async def _submit(self, actor, intent, target, deadline):
        if self.directory:
            capability = {"backup": "room.backup.create", "kick": "player.kick"}.get(
                intent.kind, "room." + intent.kind
            )
            try:
                items = await self.directory.items(actor, deadline)
            except QueryError:
                # Never silently downgrade writes to the older, non-recoverable path.
                raise QueryError("CAPABILITY_MISSING") from None
            item = next((i for i in items if i["id"] == capability), None)
            if not item:
                raise QueryError("CAPABILITY_MISSING")
            parameters = {"playerId": target.player} if intent.kind == "kick" else {}
            return await self._submit_capability(actor, item, target, parameters, deadline)
        return await self._legacy_submit(actor, intent, target, deadline)

    async def submit_capability(self, actor, item, target, parameters, deadline):
        async with self.lock:
            return await self._submit_capability(actor, item, target, parameters, deadline)

    async def _submit_capability(self, actor, item, target, parameters, deadline):
        source = (
            *actor.key,
            str(getattr(getattr(actor.event, "message_obj", None), "message_id", "")),
        )
        if not source[-1]:
            raise QueryError("MESSAGE_ID_REQUIRED")
        key = hashlib.sha256(json.dumps(source).encode()).hexdigest()
        fingerprint = hashlib.sha256(
            json.dumps(
                [item["id"], item["schemaRevision"], target.room, parameters], sort_keys=True
            ).encode()
        ).hexdigest()
        records = await self.load()
        if key in records:
            if records[key]["fingerprint"] != fingerprint:
                raise QueryError("REQUEST_CONFLICT")
            return await self.read(actor, records[key], deadline)
        if len(records) >= 256:
            # Stable identities are never erased to make a duplicate executable.
            raise QueryError("ACTION_JOURNAL_FULL")
        record = {
            "requestId": "astrbot-" + uuid.uuid4().hex,
            "fingerprint": fingerprint,
            "roomId": target.room,
            "createdAt": time.time(),
            "state": "prepared",
            "owner": hashlib.sha256(json.dumps(actor.key).encode()).hexdigest(),
            "capabilityId": item["id"],
            "schemaRevision": item["schemaRevision"],
            "nativeTools": item["nativeTools"],
            "protocol": 1,
            "connectionId": actor.connection_id,
            "delivery": "not_attempted",
        }
        records[key] = record
        await self.save(records)
        try:
            data = await self.directory.execute(
                actor, item, target.room, parameters, deadline, record["requestId"]
            )
            record.update(
                state="terminal" if terminal(data) else "received",
                jobId=data.get("jobId"),
                data=data,
            )
            records[key] = record
            await self.save(records)
        except BaseException as error:
            # Cancellation and process interruption also leave the prepared identity.
            record["state"] = "uncertain"
            records[key] = record
            await self.save(records)
            if isinstance(error, asyncio.CancelledError):
                raise
            raise QueryError("RESULT_UNCERTAIN") from None
        if data.get("status") in {"pending", "running", "uncertain"}:
            self.watch(actor, key, record)
        body, footer = business(data)
        return body + "\n" + footer

    async def _legacy_submit(self, actor, intent, target, deadline):
        # Source message identity makes duplicate dispatch stable across reloads.
        source = (
            *actor.key,
            str(getattr(getattr(actor.event, "message_obj", None), "message_id", "")),
        )
        if not source[-1]:
            raise QueryError("MESSAGE_ID_REQUIRED")
        key = hashlib.sha256(json.dumps(source).encode()).hexdigest()
        fingerprint = hashlib.sha256(
            json.dumps([intent.kind, target.room, target.player]).encode()
        ).hexdigest()
        records = await self.load()
        old = records.get(key)
        if old:
            if old["fingerprint"] != fingerprint:
                raise QueryError("REQUEST_CONFLICT")
            return await self.read(actor, old, deadline)
        records = {
            k: v
            for k, v in records.items()
            if not (
                v.get("state") == "terminal" and time.time() - v.get("createdAt", 0) > 7 * 86400
            )
        }
        if len(records) >= 256:
            raise QueryError("ACTION_JOURNAL_FULL")
        record = {
            "requestId": "astrbot-" + uuid.uuid4().hex,
            "fingerprint": fingerprint,
            "roomId": target.room,
            "createdAt": time.time(),
            "state": "prepared",
            "owner": hashlib.sha256(json.dumps(actor.key).encode()).hexdigest(),
        }
        records[key] = record
        await self.save(records)  # failure here prevents sending
        if intent.kind == "backup":
            name = "api_createRoomBackup"
            args = {
                "path": {"roomId": target.room},
                "body": {},
                "requestId": record["requestId"],
            }
        elif intent.kind == "kick":
            name = "api_kickRoomPlayer"
            args = {
                "path": {"roomId": target.room, "playerId": target.player},
                "body": {},
                "requestId": record["requestId"],
            }
        else:
            name = "api_runRoomAction"
            args = {
                "path": {"roomId": target.room, "action": intent.kind},
                "body": {},
                "requestId": record["requestId"],
            }
        try:
            data = await self.gateway.call(actor, name, args, deadline, write=True)
            record.update(
                state=(
                    "terminal"
                    if data.get("status") in {"succeeded", "failed", "canceled"}
                    else "received"
                ),
                jobId=data.get("id"),
                data={k: data[k] for k in ("status", "outcome") if k in data},
            )
            # A direct action is not necessarily a Job. A message or 200 alone
            # cannot prove in-game execution succeeded.
            records[key] = record
            await self.save(records)
        except Exception:
            record["state"] = "uncertain"
            records[key] = record
            await self.save(records)
            raise QueryError("RESULT_UNCERTAIN") from None
        if data.get("id") and data.get("status") in {"queued", "running", "canceling"}:
            self.watch(actor, key, record)
        return (
            (outcome(data) + " · 任务 " + safe(data["id"]))
            if data.get("id")
            else "请求已提交，游戏内结果尚未确认。"
        )

    async def read(self, actor, record, deadline):
        if record.get("protocol") == 1:
            data = await self.get_job(actor, record, deadline)
            body, footer = business(data)
            return body + "\n" + footer
        if not record.get("jobId"):
            return "原请求结果待确认；为避免重复执行，没有重新提交。"
        data = await self.get_job(actor, record, deadline)
        return outcome(data) + " · 任务 " + safe(record["jobId"])

    async def get_job(self, actor, record, deadline):
        if record.get("connectionId", actor.connection_id) != actor.connection_id:
            raise QueryError("CONNECTION_CHANGED")
        if record["roomId"] not in actor.rooms:
            raise QueryError("ROOM_DENIED")
        if record.get("protocol") == 1:
            from .capabilities import permitted

            item = (
                self.directory.lookup(actor, record.get("capabilityId")) if self.directory else None
            )
            if item is None:
                await self.directory.items(actor, deadline)
                item = self.directory.lookup(actor, record.get("capabilityId"))
            # Removed capability: still require the exact old category and native
            # tools plus current explicit write allowlist to read its own result.
            if item is None:
                item = {
                    "id": record.get("capabilityId"),
                    "access": "write",
                    "category": "management",
                    "nativeTools": record.get("nativeTools", []),
                }
            if not permitted(actor, item):
                raise QueryError("PERMISSION_DENIED")
            if self.gateway.authorize_receipt:
                await self.gateway.authorize_receipt(actor, item)
            else:
                for tool in item["nativeTools"]:
                    await self.gateway.authorize(
                        actor, tool, {"path": {"roomId": record["roomId"]}}
                    )
            return await self.gateway.call(
                actor,
                "api_getBotCapabilityResult",
                {"path": {"roomId": record["roomId"], "requestId": record["requestId"]}},
                deadline,
            )
        data = await self.gateway.call(
            actor, "api_getJob", {"path": {"jobId": record["jobId"]}}, deadline
        )
        if data.get("roomId") != record["roomId"]:
            raise QueryError("ROOM_DENIED")
        return data

    async def latest(self, actor, room, deadline):
        owner = hashlib.sha256(json.dumps(actor.key).encode()).hexdigest()
        records = await self.load()
        candidates = [
            (k, r)
            for k, r in records.items()
            if r.get("owner") == owner and r.get("roomId") == room
        ]
        if not candidates:
            return "没有找到你在此房间提交的任务。"
        key, record = max(candidates, key=lambda item: item[1].get("createdAt", 0))
        if record.get("protocol") == 1:
            data = await self.get_job(actor, record, deadline)
            if data.get("status") in {"pending", "running", "uncertain"}:
                self.watch(actor, key, record)
            body, footer = business(data)
            return body + "\n" + footer
        if not record.get("jobId"):
            return await self.read(actor, record, deadline)
        data = await self.get_job(actor, record, deadline)
        text = outcome(data) + " · 任务 " + safe(record["jobId"])
        if data.get("status") in {"queued", "running", "canceling"}:
            self.watch(actor, key, record)
        elif data.get("status") in {"succeeded", "failed", "canceled"}:
            async with self.lock:
                current = await self.load()
                if key in current:
                    current[key]["state"] = "terminal"
                    current[key]["data"] = {k: data[k] for k in ("status", "outcome") if k in data}
                    await self.save(current)
        return text

    def watch(self, actor, key, record):
        if not self.notify or self.closed or key in self.watches:
            return
        if len(self.watches) >= 32:
            return
        self.watches[key] = {
            "actor": actor,
            "record": dict(record),
            "until": time.monotonic() + self.tracking_seconds,
            "next": time.monotonic() + self.poll_interval,
        }
        if self.worker is None or self.worker.done():
            self.worker = asyncio.create_task(self._watch_loop())

    async def _finish_watch(self, key, watch, data=None):
        record = watch["record"]
        finished = data is not None and terminal(data)
        async with self.lock:
            records = await self.load()
            if key in records:
                records[key]["state"] = "terminal" if finished else "tracking_expired"
                if data is not None:
                    records[key]["data"] = {k: data[k] for k in ("status", "outcome") if k in data}
                records[key]["delivery"] = "delivery_unknown"
                await self.save(records)
        self.watches.pop(key, None)
        # Persist and remove before attempting a send; never retry uncertain delivery.
        text = (
            outcome(data)
            if finished
            else "任务结果未确认，已停止自动跟踪；可用任务进度回读原任务，没有重新提交。"
        )
        if record.get("protocol") == 1 and data:
            body, footer = business(data)
            text = body + "\n" + footer
        else:
            text += " · 任务 " + safe(record.get("jobId") or record["requestId"])
        receipt = await self.notify(watch["actor"], text)
        if receipt:
            async with self.lock:
                records = await self.load()
                if key in records:
                    records[key]["delivery"] = "sent"
                    records[key]["messageId"] = str(receipt)
                    await self.save(records)

    async def _watch_loop(self):
        while self.watches and not self.closed:
            for key, watch in list(self.watches.items()):
                if self.closed:
                    return
                if watch["next"] > time.monotonic():
                    continue
                actor, record = watch["actor"], watch["record"]

                expired = time.monotonic() >= watch["until"]
                try:
                    # This one scheduler uses at most one background request;
                    # it shares the same two MCP slots as interactive reads.
                    data = await self.get_job(actor, record, time.monotonic() + 8)
                    if terminal(data) or expired:
                        await self._finish_watch(key, watch, data)
                    else:
                        watch["next"] = time.monotonic() + self.poll_interval
                except asyncio.CancelledError:
                    raise
                except Exception as error:
                    if isinstance(error, QueryError) and error.code in {
                        "PERMISSION_DENIED",
                        "NATIVE_PERMISSION_DENIED",
                        "ROOM_DENIED",
                        "PLUGIN_RELOADED",
                    }:
                        self.watches.pop(key, None)
                        continue
                    if expired:
                        # A failed notification has already removed this watch.
                        if key in self.watches:
                            try:
                                await self._finish_watch(key, watch)
                            except Exception:
                                self.watches.pop(key, None)
                    else:
                        watch["next"] = time.monotonic() + max(10, self.poll_interval)
            if self.watches:
                await asyncio.sleep(min(self.poll_interval, 1))

    def stop_tracking(self, actor):
        self.watches = {k: v for k, v in self.watches.items() if v["actor"].key != actor.key}
        return "已停止向你跟踪和回复；已经提交的游戏任务会继续执行。"

    async def close(self):
        self.closed = True
        if self.worker:
            self.worker.cancel()
            await asyncio.gather(self.worker, return_exceptions=True)
        self.watches.clear()
