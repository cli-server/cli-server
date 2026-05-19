"""WebSocket JSON-RPC client to the agentserver codex-app-gateway.

One connection per Ctx. Connection is lazy: `connect()` is a no-op if
already connected. Bearer auth via constructor; user_id is forwarded
on every tool/call's _meta for attribution.
"""

from __future__ import annotations

import asyncio
import contextlib
import json
from typing import Any

import websockets

from .errors import ConnectionError as SdkConnectionError


class WSClient:
    def __init__(
        self,
        url: str,
        *,
        token: str,
        workspace_id: str,
        user_id: str | None,
    ) -> None:
        self.url = url
        self.token = token
        self.workspace_id = workspace_id
        self.user_id = user_id

        self._ws: websockets.ClientConnection | None = None
        self._next_id = 0
        self._pending: dict[int, asyncio.Future[dict[str, Any]]] = {}
        self._reader_task: asyncio.Task | None = None
        self._connect_lock = asyncio.Lock()
        self.thread_id: str | None = None

    @property
    def is_connected(self) -> bool:
        return self._ws is not None and self.thread_id is not None

    async def connect(self) -> None:
        async with self._connect_lock:
            if self.is_connected:
                return
            try:
                self._ws = await websockets.connect(
                    self.url,
                    additional_headers={"Authorization": f"Bearer {self.token}"},
                    compression=None,  # codex app-server rejects permessage-deflate
                    max_size=64 * 1024 * 1024,
                )
            except Exception as e:
                raise SdkConnectionError(f"dial {self.url}: {e}") from e

            self._reader_task = asyncio.create_task(self._reader())

            try:
                await self._request(
                    "initialize",
                    {
                        "clientInfo": {
                            "name": "agentserver-sdk",
                            "title": "agentserver-sdk",
                            "version": "0",
                        },
                        "capabilities": {
                            "experimentalApi": True,
                            "requestAttestation": False,
                            "optOutNotificationMethods": [],
                        },
                    },
                )
                await self._notify("initialized")
                ts = await self._request("thread/start", {})
                # Codex ≥0.130 returns {"thread": {"id": ...}, ...};
                # older versions returned a flat {"thread_id": ...}.
                # Accept both.
                tid = ts.get("thread_id")
                if not tid:
                    tid = (ts.get("thread") or {}).get("id")
                if not tid:
                    raise SdkConnectionError(f"thread/start response missing thread_id: {ts!r}")
                self.thread_id = tid
            except Exception:
                # Don't leak ws + reader task if handshake fails mid-way
                await self.close()
                raise

    async def close(self) -> None:
        if self._reader_task is not None:
            self._reader_task.cancel()
            with contextlib.suppress(asyncio.CancelledError, Exception):
                await self._reader_task
            self._reader_task = None
        if self._ws is not None:
            await self._ws.close()
            self._ws = None
        self.thread_id = None
        # Fail any pending requests
        for fut in self._pending.values():
            if not fut.done():
                fut.set_exception(SdkConnectionError("connection closed"))
        self._pending.clear()

    async def mcp_tool_call(
        self,
        *,
        server: str,
        tool: str,
        arguments: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Issue `mcpServer/tool/call`. Returns the raw MCP CallToolResult dict.

        Caller is responsible for interpreting `content` / `structuredContent` /
        `isError`. RPC-level errors raise SdkConnectionError.
        """
        await self.connect()  # lazy
        assert self.thread_id is not None
        params: dict[str, Any] = {
            "thread_id": self.thread_id,
            "server": server,
            "tool": tool,
            "_meta": {
                "agentserver_user_id": self.user_id,
                "agentserver_workspace_id": self.workspace_id,
            },
        }
        if arguments is not None:
            params["arguments"] = arguments
        return await self._request("mcpServer/tool/call", params)

    async def _request(self, method: str, params: dict[str, Any]) -> dict[str, Any]:
        if self._ws is None:
            raise SdkConnectionError("not connected")
        self._next_id += 1
        rid = self._next_id
        fut: asyncio.Future[dict[str, Any]] = asyncio.get_running_loop().create_future()
        self._pending[rid] = fut
        try:
            await self._ws.send(
                json.dumps(
                    {
                        "jsonrpc": "2.0",
                        "id": rid,
                        "method": method,
                        "params": params,
                    }
                )
            )
            return await fut
        finally:
            self._pending.pop(rid, None)

    async def _notify(self, method: str, params: dict[str, Any] | None = None) -> None:
        if self._ws is None:
            raise SdkConnectionError("not connected")
        frame: dict[str, Any] = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            frame["params"] = params
        await self._ws.send(json.dumps(frame))

    async def _reader(self) -> None:
        assert self._ws is not None
        try:
            async for raw in self._ws:
                msg = json.loads(raw)
                rid = msg.get("id")
                if rid is None:
                    # server notification or request from server — ignore in v1
                    continue
                fut = self._pending.get(rid)
                if fut is None or fut.done():
                    continue
                if "error" in msg:
                    err = msg["error"]
                    fut.set_exception(
                        SdkConnectionError(f"rpc {err.get('code')}: {err.get('message')}")
                    )
                else:
                    fut.set_result(msg.get("result", {}))
        except websockets.ConnectionClosed:
            pass
