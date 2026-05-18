"""Reusable WS stub gateway for tests + manual smoke.

Implements JSON-RPC 2.0 over WebSocket. By default answers `initialize`
and `thread/start`. Register more methods via `.on(method, handler)`.

Handler signature: `handler(params: dict) -> result_dict | {"error": {...}}`.
"""

from __future__ import annotations

import json
from collections.abc import Callable
from typing import Any

import websockets

HandlerResult = dict[str, Any]
Handler = Callable[[dict[str, Any]], HandlerResult]


class StubGateway:
    def __init__(self) -> None:
        self._handlers: dict[str, Handler] = {}
        self._server: websockets.Server | None = None
        self.port: int = 0
        self.received: list[dict[str, Any]] = []
        self.connections: int = 0
        self.last_headers: dict[str, str] = {}

        # Defaults
        self.on(
            "initialize",
            lambda p: {
                "protocolVersion": "1.0",
                "serverInfo": {"name": "stub", "version": "0"},
                "capabilities": {},
            },
        )
        self.on("thread/start", lambda p: {"thread_id": "stub-thread-1"})

    def on(self, method: str, handler: Handler) -> None:
        self._handlers[method] = handler

    @property
    def url(self) -> str:
        if self.port == 0:
            raise RuntimeError("StubGateway not started")
        return f"ws://127.0.0.1:{self.port}"

    async def start(self) -> None:
        self._server = await websockets.serve(
            self._handle,
            "127.0.0.1",
            0,
            process_request=self._capture_headers,
        )
        # websockets >=14 exposes sockets via .sockets
        self.port = self._server.sockets[0].getsockname()[1]

    async def stop(self) -> None:
        if self._server is not None:
            self._server.close()
            await self._server.wait_closed()
            self._server = None

    async def _capture_headers(self, conn, request):
        # websockets >=14: request.headers is a Headers obj
        self.last_headers = {k.lower(): v for k, v in request.headers.raw_items()}
        return None  # accept

    async def _handle(self, ws) -> None:
        self.connections += 1
        try:
            async for raw in ws:
                msg = json.loads(raw)
                self.received.append(msg)
                mid = msg.get("id")
                method = msg.get("method")
                if mid is None:
                    # notification (e.g. "initialized")
                    continue
                handler = self._handlers.get(method)
                if handler is None:
                    resp = {
                        "jsonrpc": "2.0",
                        "id": mid,
                        "error": {"code": -32601, "message": f"Method not found: {method}"},
                    }
                else:
                    try:
                        out = handler(msg.get("params", {}) or {})
                    except Exception as e:
                        resp = {
                            "jsonrpc": "2.0",
                            "id": mid,
                            "error": {"code": -32603, "message": str(e)},
                        }
                    else:
                        if isinstance(out, dict) and "error" in out and "code" in out["error"]:
                            resp = {"jsonrpc": "2.0", "id": mid, "error": out["error"]}
                        else:
                            resp = {"jsonrpc": "2.0", "id": mid, "result": out}
                await ws.send(json.dumps(resp))
        except websockets.ConnectionClosed:
            pass
