"""One async read gateway per configured panel, without SDK write replay."""

import asyncio
import json
import time
import uuid


class QueryError(RuntimeError):
    def __init__(self, code):
        self.code = code
        super().__init__(code)


def unpack(result):
    if getattr(result, "isError", False):
        raise QueryError("MCP_TOOL_FAILED")
    value = getattr(result, "structuredContent", None)
    if not isinstance(value, dict):
        try:
            texts = [item.text for item in result.content if getattr(item, "type", None) == "text"]
            value = json.loads(texts[0])
        except (TypeError, ValueError, IndexError, AttributeError):
            raise QueryError("MCP_RESPONSE_INVALID") from None
    if not isinstance(value, dict) or value.get("status") != "succeeded":
        raise QueryError(
            "RESULT_UNCERTAIN"
            if isinstance(value, dict) and value.get("status") in ("executing", "uncertain")
            else "MCP_OPERATION_FAILED"
        )
    data = value.get("data")
    if isinstance(data, dict) and "httpStatus" in data:
        code = data["httpStatus"]
        if type(code) is not int or not 200 <= code < 300:
            raise QueryError("API_FAILED")
        data = data.get("data")
    if not isinstance(data, dict):
        raise QueryError("MCP_RESPONSE_INVALID")
    if data.get("_aiTruncated"):
        raise QueryError("RESULT_TRUNCATED")
    return data


class Gateway:
    def __init__(self, transport, authorize, limit=2, queue=32, authorize_receipt=None):
        self.transport, self.authorize = transport, authorize
        self.authorize_receipt = authorize_receipt
        self.limit = asyncio.Semaphore(limit)
        self.queue = queue
        self.pending = {}
        self.by_user = {}
        self.closed = False
        self.calls = 0
        self.merged = 0

    async def call(self, actor, name, args, deadline, *, write=False):
        await self.authorize(actor, name, args)  # each waiter, including merged reads
        if self.closed:
            raise QueryError("PLUGIN_RELOADED")
        if self.by_user.get(actor.key, 0) >= 2:
            raise QueryError("USER_BUSY")
        key = (
            actor.read_projection,
            name,
            json.dumps(args, ensure_ascii=False, sort_keys=True, separators=(",", ":")),
            uuid.uuid4().hex if write else "",
        )
        self.by_user[actor.key] = self.by_user.get(actor.key, 0) + 1
        try:
            task = None if write else self.pending.get(key)
            if task is None:
                if len(self.pending) >= self.queue:
                    raise QueryError("PANEL_BUSY")
                task = asyncio.create_task(self._run(actor, name, args, deadline, write))
                self.pending[key] = task

                def finish(done):
                    if self.pending.get(key) is done:
                        self.pending.pop(key, None)
                    if not done.cancelled():
                        done.exception()  # detached waiter can time out

                task.add_done_callback(finish)
            else:
                self.merged += 1
            async with asyncio.timeout(max(0, deadline - time.monotonic())):
                result = await asyncio.shield(task)  # one waiter cannot cancel others
            await self.authorize(actor, name, args)  # revoked/changed while waiting
            if self.closed:
                raise QueryError("PLUGIN_RELOADED")
            return result
        except TimeoutError:
            raise QueryError("QUERY_TIMEOUT") from None
        finally:
            self.by_user[actor.key] -= 1
            if not self.by_user[actor.key]:
                del self.by_user[actor.key]

    async def _run(self, actor, name, args, deadline, write):
        async with asyncio.timeout(max(0, deadline - time.monotonic())):
            async with self.limit:
                # Queued writes must not execute after the caller loses access.
                await self.authorize(actor, name, args)
                self.calls += 1
                result = await self.transport(
                    name, args, max(0.01, deadline - time.monotonic()), write
                )
                return unpack(result)

    async def close(self):
        self.closed = True
        tasks = list(self.pending.values())
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        self.pending.clear()  # shared framework client belongs to AstrBot
