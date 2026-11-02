"""Small durable read handles, separate from the write replay journal."""

import asyncio
import hashlib
import json
import time

from .gateway import QueryError


class ReadReceipts:
    def __init__(self, load, save):
        self.load, self.save = load, save
        self.lock = asyncio.Lock()

    @staticmethod
    def owner(actor):
        return hashlib.sha256(json.dumps(actor.key).encode()).hexdigest()

    async def reserve(self, actor, item, room, request_id):
        async with self.lock:
            records = await self.load("task_reads_v1", {})
            now = time.time()
            records = {k: v for k, v in records.items() if now - v.get("createdAt", 0) < 86400}
            while len(records) >= 256:
                records.pop(min(records, key=lambda k: records[k].get("createdAt", 0)))
            records[request_id] = {
                "requestId": request_id,
                "owner": self.owner(actor),
                "roomId": room,
                "connectionId": actor.connection_id,
                "capabilityId": item["id"],
                "nativeTools": item["nativeTools"],
                "createdAt": now,
                "protocol": 1,
            }
            await self.save("task_reads_v1", records)

    async def find(self, actor, request_id):
        records = await self.load("task_reads_v1", {})
        record = records.get(request_id)
        if (
            not record
            or record.get("owner") != self.owner(actor)
            or record.get("roomId") not in actor.rooms
            or record.get("connectionId") != actor.connection_id
            or time.time() - record.get("createdAt", 0) >= 86400
        ):
            raise QueryError("PERMISSION_DENIED")
        return record
