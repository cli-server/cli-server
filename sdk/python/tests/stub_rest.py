"""ASGI stub for the codex-exec-gateway /api/sdk/* surface.

Each test registers a dict of `(method, path) -> handler` where handler
receives the parsed JSON body (or {} for GETs) and returns
(status_code, json_dict). The stub records every call so tests can
assert on what the SDK sent.
"""

from __future__ import annotations

import json
from typing import Any, Awaitable, Callable
from urllib.parse import parse_qs

Handler = Callable[[dict[str, Any], dict[str, list[str]]], Awaitable[tuple[int, dict[str, Any]]]]


class StubRest:
    """Minimal ASGI app for testing the SDK against a REST gateway.

    `routes` is `(method, path) -> handler(body, query) -> (status, json)`.
    Match is exact on method + path (no wildcards) — tests register paths
    that include `{name}` etc. literally, or use a fallback `("*", "*")` key.
    """

    def __init__(self, routes: dict[tuple[str, str], Handler] | None = None) -> None:
        self.routes: dict[tuple[str, str], Handler] = routes or {}
        self.calls: list[tuple[str, str, dict[str, Any], dict[str, list[str]]]] = []

    def register(self, method: str, path: str, handler: Handler) -> None:
        self.routes[(method, path)] = handler

    async def __call__(self, scope, receive, send) -> None:
        assert scope["type"] == "http"
        method = scope["method"]
        path = scope["path"]
        query = parse_qs(scope.get("query_string", b"").decode())
        body_bytes = b""
        while True:
            msg = await receive()
            body_bytes += msg.get("body", b"")
            if not msg.get("more_body"):
                break
        body: dict[str, Any] = json.loads(body_bytes) if body_bytes else {}
        self.calls.append((method, path, body, query))

        handler = self.routes.get((method, path))
        if handler is None:
            await self._send(
                send,
                404,
                {"error": {"code": "no_route", "message": f"no stub for {method} {path}"}},
            )
            return
        status, payload = await handler(body, query)
        await self._send(send, status, payload)

    @staticmethod
    async def _send(send, status: int, payload: dict[str, Any]) -> None:
        encoded = json.dumps(payload).encode()
        await send({
            "type": "http.response.start",
            "status": status,
            "headers": [(b"content-type", b"application/json")],
        })
        await send({"type": "http.response.body", "body": encoded})
