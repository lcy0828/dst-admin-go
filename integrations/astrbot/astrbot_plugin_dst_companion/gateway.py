"""One async read gateway per configured panel, without SDK write replay."""

import asyncio
import json
import re
import time
import uuid
from contextvars import ContextVar


_submission_guard = ContextVar("dst_submission_guard", default=None)


def check_submission():
    """Synchronous check at a transport's final boundary before sending."""
    guard = _submission_guard.get()
    if guard is not None:
        guard()


class QueryError(RuntimeError):
    def __init__(self, code, *, message="", details=None, call_id=""):
        self.code = code
        self.message, self.details, self.call_id = message, details, call_id
        super().__init__(code)


def api_failure(data, fallback):
    """Preserve typed API errors; legacy text alone cannot define a workflow."""
    if isinstance(data, dict):
        failure = data.get("platformError") or data.get("error")
        if isinstance(failure, dict) and re.fullmatch(r"[A-Z][A-Z0-9_]{0,63}", str(failure.get("code", ""))):
            return QueryError(failure["code"], message=str(failure.get("message", ""))[:500],
                              details=failure.get("details") if isinstance(failure.get("details"), dict) else None,
                              call_id=str(data.get("callId", ""))[:128])
    return QueryError(fallback)


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
        if isinstance(value, dict) and value.get("status") in ("executing", "uncertain"):
            raise QueryError("RESULT_UNCERTAIN")
        raise api_failure(value.get("data") if isinstance(value, dict) else None, "MCP_OPERATION_FAILED")
    data = value.get("data")
    if isinstance(data, dict) and "httpStatus" in data:
        code = data["httpStatus"]
        if type(code) is not int or not 200 <= code < 300:
            raise api_failure(data, "API_FAILED")
        data = data.get("data")
    if isinstance(data, dict) and data.get("_aiTruncated"):
        raise QueryError("RESULT_TRUNCATED")
    return data


def read_result(value):
    """Decode a controlled read receipt without treating acceptance as data."""
    if not isinstance(value, dict):
        raise QueryError("MCP_RESPONSE_INVALID")
    state = value.get("state")
    if not isinstance(state, str):
        raise QueryError("MCP_RESPONSE_INVALID")
    if state != "succeeded":
        if state == "failed":
            raise api_failure(value.get("result"), "MCP_OPERATION_FAILED")
        raise QueryError("RESULT_UNCERTAIN")
    result = value.get("result")
    if not isinstance(result, dict) or type(result.get("httpStatus")) is not int:
        raise QueryError("MCP_RESPONSE_INVALID")
    if not 200 <= result["httpStatus"] < 300:
        raise api_failure(result, "API_FAILED")
    data = result.get("data")
    if isinstance(data, dict) and data.get("_aiTruncated"):
        raise QueryError("RESULT_TRUNCATED")
    return data


class Gateway:
    def __init__(self, transport, authorize, limit=2, queue=32, authorize_receipt=None, write_guard=None):
        self.transport, self.authorize = transport, authorize
        self.authorize_receipt = authorize_receipt
        self.write_guard = write_guard
        self.limit = asyncio.Semaphore(limit)
        self.queue = queue
        self.pending = {}
        self.by_user = {}
        self.closed = False
        self.calls = 0
        self.merged = 0

    async def call(self, actor, name, args, deadline, *, write=False, guard=None):
        if write and self.write_guard is not None:
            caller_guard = guard

            def guard():
                # An operation-specific guard cannot bypass the shared gate.
                self.write_guard(actor)
                if caller_guard is not None:
                    caller_guard()

        await self.authorize(actor, name, args)  # each waiter, including merged reads
        if guard is not None:
            guard()
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
                task = asyncio.create_task(self._run(actor, name, args, deadline, write, guard))
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

    async def _run(self, actor, name, args, deadline, write, guard=None):
        async with asyncio.timeout(max(0, deadline - time.monotonic())):
            async with self.limit:
                # Queued writes must not execute after the caller loses access.
                await self.authorize(actor, name, args)
                token = _submission_guard.set(guard)
                try:
                    check_submission()
                    self.calls += 1
                    result = await self.transport(
                        name, args, max(0.01, deadline - time.monotonic()), write
                    )
                    return unpack(result)
                finally:
                    _submission_guard.reset(token)

    async def close(self):
        self.closed = True
        tasks = list(self.pending.values())
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        self.pending.clear()  # shared framework client belongs to AstrBot
