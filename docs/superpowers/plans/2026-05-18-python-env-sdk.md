# Python env SDK + Notebook Image — Implementation Plan (Plan 1)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the `agentserver_sdk` Python package + `Dockerfile.notebook` image so that a developer running the image locally with a stub gateway can `await ctx.envs()` and `await alpha.shell("…")` from a Jupyter cell.

**Architecture:** Pure async/await Python SDK (`Ctx` → workspace handle, `Env` → executor handle, `Process` → long-running cmd) speaking JSON-RPC over a single WebSocket to the gateway. All env-mcp tools reachable via typed core methods or dynamic `env.call(name, args)` / `__getattr__`. Image bundles SDK + Jupyter + an IPython startup file that injects `ctx` on kernel boot. A reusable Python stub gateway lives in tests so SDK can be developed and validated end-to-end without the real backend.

**Tech Stack:** Python 3.12 · `websockets` ~14 · pytest + pytest-asyncio · jupyter-server ~2.14 · jupyterlab ~4.2 · ipykernel ~6.29 · Docker · docker-compose

**Out of scope for this plan (separate plans later):**
- Operation log DB write path + Interceptor (Plan 2)
- Notebook supervisor + k8s spawn + jupyter proxy (Plan 3)
- Web UI panels (part of Plan 3)
- env-mcp per-env capability reporting (separate spec follow-up)

For Plan 1 staging:
- All envs assumed to expose the same workspace-wide tool set (current env-mcp behaviour)
- `ctx.history(...)` issues `operations/list` RPC, but the real gateway doesn't implement it yet → tests mock it via stub gateway; real wiring lands in Plan 2
- `__getattr__` dynamic dispatch is implemented and tested with mock per-env tools metadata, but real metadata depends on env-mcp work

---

## File Structure

```
sdk/python/                                  # NEW Python package
├── pyproject.toml
├── README.md
├── src/agentserver_sdk/
│   ├── __init__.py                          # re-exports
│   ├── errors.py                            # ToolError, ConnectionError, NotConnectedError
│   ├── types.py                             # ShellResult, ToolMetadata, OperationRecord, Patch
│   ├── client.py                            # WSClient: ws connect + initialize + request/response
│   ├── env.py                               # Env: typed methods + dynamic dispatch
│   ├── process.py                           # Process: async context manager over exec_command tools
│   ├── ctx.py                               # Ctx: from_env, envs, env, copy, history
│   └── _repr.py                             # _repr_html_ implementations (jupyter rendering)
└── tests/
    ├── conftest.py                          # pytest fixtures: stub gateway, ctx
    ├── stub_gateway.py                      # reusable WS stub for tests + manual smoke
    ├── test_client.py
    ├── test_env.py
    ├── test_process.py
    ├── test_ctx.py
    ├── test_types.py
    └── test_repr.py

Dockerfile.notebook                          # NEW image at repo root
notebook/                                    # NEW config + startup files
├── ipython_startup/
│   └── 00-ctx.py
├── jupyter_server_config.py
├── docker-compose.smoke.yml                 # local smoke: jupyter + stub gateway
└── README.md                                # how to run smoke locally

Makefile                                     # MODIFY: add `notebook-image` + `sdk-test` targets
```

---

## Design Decisions Locked Before Tasks

These come up across multiple tasks. Capturing once so they're not re-invented per task.

**1. One WS connection per `Ctx`, lazy.** `Ctx.from_env()` is sync; `await ctx.envs()` (or any first call) triggers connect: open ws → `initialize` → `initialized` notification → `thread/start` to get a thread_id → cache. Reuse on subsequent calls. Lock around connect to serialise concurrent first-call races.

**2. One thread per `Ctx`.** Real codex threads carry LLM conversation; SDK never sends turns, so a single throwaway thread is enough as a routing key for `mcpServer/tool/call`. Thread_id cached on `Ctx`; not user-visible.

**3. Bearer auth via `AGENTSERVER_WORKSPACE_TOKEN`.** `Ctx.from_env()` reads `AGENTSERVER_GATEWAY_URL` (default `ws://localhost:8086/notebook/ws`), `AGENTSERVER_WORKSPACE_TOKEN`, `AGENTSERVER_WORKSPACE_ID`, `AGENTSERVER_USER_ID`. Token sent as `Authorization: Bearer …` on WS connect. user_id forwarded in every `mcpServer/tool/call` `_meta.user_id` for attribution (no signing — gateway trusts the workspace token).

**4. JSON-RPC framing.** Outgoing requests: `{"jsonrpc": "2.0", "id": <int>, "method": …, "params": …}`. Notifications: same minus `id`. Each request increments an `_next_id` counter. Pending map `id → asyncio.Future` resolves on matching response.

**5. Stub gateway shape.** `tests/stub_gateway.py` exposes a `StubGateway` class with `.on(method, handler)`. Defaults respond to `initialize` and `thread/start`. Test cases register handlers for `mcpServer/tool/call`, `operations/list`, etc. Records every received frame for assertions.

**6. Core tools list (typed wrappers in SDK):**
```
shell, read_file, write_file, apply_patch,
exec_command, write_stdin, read_output, terminate,
copy_path
```
Anything not in this list → `Env.call(name, args)` or `__getattr__`.

**7. `Env.call` argument shape.** `Env.call("submit_task", {...})` translates to MCP `mcpServer/tool/call` with `params = {server: "env_mcp", tool: "submit_task", arguments: {environment_id: env.name, ...args}}`. The `environment_id` injection is automatic.

**8. Result parsing.** MCP `CallToolResult` has `content[]` (text/image items), `structuredContent` (jsonb), `isError`. Typed wrappers (`ShellResult.from_mcp`) extract well-known fields. `Env.call(...)` returns the raw dict.

**9. Errors.** `isError=true` → raise `ToolError(tool, env, message, raw)`. JSON-RPC error → same. Network failure → `ConnectionError`. Pre-connect call → `NotConnectedError` (shouldn't happen with lazy connect, but defensive).

**10. Dynamic dispatch.** On first `await ctx.envs()` SDK calls `env/capabilities { env_id }` (stub or future real). For each non-core tool in the metadata, `setattr(env, tool_name, generated_method)`. `__getattr__` is a safety net for tools not in metadata (raises `AttributeError`).

**11. Python version pin: 3.12.** Matches image. Use only stdlib + `websockets`. No `httpx`, no `pydantic`.

**12. Spec deviation — `env.tools` as field, not async method.** The design spec example reads `print(await hpc.tools())`. Plan 1 exposes `env.tools` as a sync `list[ToolMetadata]` field (populated at `Ctx.envs()` time via `env/capabilities`). Rationale: tools are already in-memory after env construction; making them awaitable adds round-trip cost for no benefit. Refresh via `await ctx.refresh()` if the workspace's tool catalogue changes mid-session. Documented here so reviewers don't flag it.

---

## Task 1: Repository skeleton

**Files:**
- Create: `sdk/python/pyproject.toml`
- Create: `sdk/python/README.md`
- Create: `sdk/python/src/agentserver_sdk/__init__.py`
- Create: `sdk/python/tests/conftest.py`
- Create: `sdk/python/tests/test_smoke.py`

- [ ] **Step 1: Create pyproject.toml**

Create `sdk/python/pyproject.toml`:

```toml
[build-system]
requires = ["hatchling"]
build-backend = "hatchling.build"

[project]
name = "agentserver-sdk"
version = "0.1.0"
description = "Async Python SDK for agentserver envs (env-mcp)"
requires-python = ">=3.12"
dependencies = [
    "websockets~=14.0",
]

[project.optional-dependencies]
dev = [
    "pytest~=8.0",
    "pytest-asyncio~=0.24",
    "ruff~=0.6",
]

[tool.hatch.build.targets.wheel]
packages = ["src/agentserver_sdk"]

[tool.pytest.ini_options]
asyncio_mode = "auto"
asyncio_default_fixture_loop_scope = "function"
testpaths = ["tests"]
```

- [ ] **Step 2: Create empty package + readme**

Create `sdk/python/src/agentserver_sdk/__init__.py`:

```python
"""agentserver_sdk — Python SDK for agentserver envs."""

__version__ = "0.1.0"
```

Create `sdk/python/README.md`:

```markdown
# agentserver-sdk

Async Python SDK for agentserver envs. Lets developers operate on workspace envs (executors) from Python — same env-mcp tools the LLM uses, just without going through prompts.

See `docs/superpowers/specs/2026-05-18-python-env-sdk-design.md` for design.

## Install (development)

```
cd sdk/python
pip install -e ".[dev]"
pytest
```

## Usage (in a kernel where ctx is pre-loaded)

```python
envs = await ctx.envs()
alpha = await ctx.env("alpha")
r = await alpha.shell("uname -a")
print(r.stdout)
```
```

- [ ] **Step 3: Empty conftest + smoke test**

Create `sdk/python/tests/conftest.py`:

```python
"""Shared pytest fixtures for agentserver_sdk tests."""
```

Create `sdk/python/tests/test_smoke.py`:

```python
import agentserver_sdk


def test_package_importable():
    assert agentserver_sdk.__version__ == "0.1.0"
```

- [ ] **Step 4: Install + run tests**

Run:
```bash
cd sdk/python
pip install -e ".[dev]"
pytest -v
```

Expected: `1 passed`.

- [ ] **Step 5: Commit**

```bash
git add sdk/python/
git commit -m "feat(sdk): python package skeleton

scaffolding for agentserver_sdk: pyproject, package init, smoke test."
```

---

## Task 2: Stub gateway fixture

**Files:**
- Create: `sdk/python/tests/stub_gateway.py`
- Modify: `sdk/python/tests/conftest.py`
- Create: `sdk/python/tests/test_stub.py`

- [ ] **Step 1: Write the failing test for stub gateway**

Create `sdk/python/tests/test_stub.py`:

```python
import json
import pytest
import websockets


async def test_stub_responds_to_initialize(stub):
    """Stub answers initialize with a default reply and records the frame."""
    async with websockets.connect(stub.url) as ws:
        await ws.send(json.dumps({
            "jsonrpc": "2.0", "id": 1,
            "method": "initialize",
            "params": {"clientInfo": {"name": "t", "title": "t", "version": "0"},
                       "capabilities": {}},
        }))
        raw = await ws.recv()
        resp = json.loads(raw)

    assert resp["id"] == 1
    assert "result" in resp
    assert resp["result"]["protocolVersion"] == "1.0"
    assert any(m.get("method") == "initialize" for m in stub.received)


async def test_stub_custom_handler_overrides_default(stub):
    """on(method, handler) wins over defaults."""
    stub.on("initialize", lambda params: {"protocolVersion": "9.9"})
    async with websockets.connect(stub.url) as ws:
        await ws.send(json.dumps({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}}))
        resp = json.loads(await ws.recv())
    assert resp["result"]["protocolVersion"] == "9.9"


async def test_stub_unknown_method_returns_error(stub):
    async with websockets.connect(stub.url) as ws:
        await ws.send(json.dumps({"jsonrpc": "2.0", "id": 1, "method": "nope", "params": {}}))
        resp = json.loads(await ws.recv())
    assert resp["error"]["code"] == -32601
```

- [ ] **Step 2: Run, confirm failure**

Run: `pytest tests/test_stub.py -v`
Expected: FAIL — `stub` fixture not found.

- [ ] **Step 3: Implement stub gateway**

Create `sdk/python/tests/stub_gateway.py`:

```python
"""Reusable WS stub gateway for tests + manual smoke.

Implements JSON-RPC 2.0 over WebSocket. By default answers `initialize`
and `thread/start`. Register more methods via `.on(method, handler)`.

Handler signature: `handler(params: dict) -> result_dict | {"error": {...}}`.
"""
from __future__ import annotations

import asyncio
import json
from typing import Any, Callable

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
        self.on("initialize", lambda p: {
            "protocolVersion": "1.0",
            "serverInfo": {"name": "stub", "version": "0"},
            "capabilities": {},
        })
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
            self._handle, "127.0.0.1", 0,
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
                        resp = {"jsonrpc": "2.0", "id": mid,
                                "error": {"code": -32603, "message": str(e)}}
                    else:
                        if isinstance(out, dict) and "error" in out and "code" in out["error"]:
                            resp = {"jsonrpc": "2.0", "id": mid, "error": out["error"]}
                        else:
                            resp = {"jsonrpc": "2.0", "id": mid, "result": out}
                await ws.send(json.dumps(resp))
        except websockets.ConnectionClosed:
            pass
```

- [ ] **Step 4: Wire the fixture**

Replace `sdk/python/tests/conftest.py` with:

```python
"""Shared pytest fixtures for agentserver_sdk tests."""
import pytest_asyncio

from tests.stub_gateway import StubGateway


@pytest_asyncio.fixture
async def stub():
    g = StubGateway()
    await g.start()
    try:
        yield g
    finally:
        await g.stop()
```

- [ ] **Step 5: Run tests, confirm pass**

Run: `pytest tests/test_stub.py -v`
Expected: `3 passed`.

- [ ] **Step 6: Commit**

```bash
git add sdk/python/tests/
git commit -m "test(sdk): stub gateway fixture + self-tests

reusable WS stub answering JSON-RPC; defaults for initialize / thread/start;
records received frames + headers for assertions."
```

---

## Task 3: WSClient — connect + handshake

**Files:**
- Create: `sdk/python/src/agentserver_sdk/errors.py`
- Create: `sdk/python/src/agentserver_sdk/client.py`
- Create: `sdk/python/tests/test_client.py`

- [ ] **Step 1: Write failing tests**

Create `sdk/python/tests/test_client.py`:

```python
import pytest

from agentserver_sdk.client import WSClient
from agentserver_sdk.errors import ConnectionError as SdkConnectionError


async def test_connect_sends_initialize_and_initialized(stub):
    c = WSClient(stub.url, token="t-1", workspace_id="ws-1", user_id="u-1")
    await c.connect()
    try:
        assert c.thread_id == "stub-thread-1"
        # First frame is initialize request
        init = stub.received[0]
        assert init["method"] == "initialize"
        assert init["id"] == 1
        # Next is initialized notification (no id)
        assert stub.received[1]["method"] == "initialized"
        assert "id" not in stub.received[1]
        # Then thread/start request
        ts = stub.received[2]
        assert ts["method"] == "thread/start"
        assert ts["id"] == 2
    finally:
        await c.close()


async def test_connect_sends_bearer_header(stub):
    c = WSClient(stub.url, token="bearer-xyz", workspace_id="ws", user_id="u")
    await c.connect()
    try:
        assert stub.last_headers.get("authorization") == "Bearer bearer-xyz"
    finally:
        await c.close()


async def test_connect_is_idempotent(stub):
    c = WSClient(stub.url, token="t", workspace_id="ws", user_id="u")
    await c.connect()
    await c.connect()
    try:
        # Only one initialize / initialized / thread-start cycle
        assert stub.connections == 1
        methods = [m.get("method") for m in stub.received]
        assert methods.count("initialize") == 1
        assert methods.count("thread/start") == 1
    finally:
        await c.close()


async def test_connect_failure_raises_sdk_error():
    c = WSClient("ws://127.0.0.1:1", token="t", workspace_id="w", user_id="u")
    with pytest.raises(SdkConnectionError):
        await c.connect()
```

- [ ] **Step 2: Run, confirm failure**

Run: `pytest tests/test_client.py -v`
Expected: FAIL — `agentserver_sdk.client` not found.

- [ ] **Step 3: Implement errors module**

Create `sdk/python/src/agentserver_sdk/errors.py`:

```python
"""SDK exception hierarchy."""
from __future__ import annotations

from typing import Any


class SdkError(Exception):
    """Base class for all SDK errors."""


class ConnectionError(SdkError):
    """Failure to establish or maintain the WS connection to the gateway."""


class NotConnectedError(SdkError):
    """Operation attempted before the client was connected."""


class ToolError(SdkError):
    """An env-mcp tool returned isError=true or the RPC errored."""

    def __init__(self, tool: str, env: str | None, message: str, raw: Any = None):
        super().__init__(f"{env or '?'}/{tool}: {message}")
        self.tool = tool
        self.env = env
        self.message = message
        self.raw = raw
```

- [ ] **Step 4: Implement WSClient (connect-only)**

Create `sdk/python/src/agentserver_sdk/client.py`:

```python
"""WebSocket JSON-RPC client to the agentserver codex-app-gateway.

One connection per Ctx. Connection is lazy: `connect()` is a no-op if
already connected. Bearer auth via constructor; user_id is forwarded
on every tool/call's _meta for attribution.
"""
from __future__ import annotations

import asyncio
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

            await self._request("initialize", {
                "clientInfo": {"name": "agentserver-sdk", "title": "agentserver-sdk",
                               "version": "0"},
                "capabilities": {
                    "experimentalApi": True,
                    "requestAttestation": False,
                    "optOutNotificationMethods": [],
                },
            })
            await self._notify("initialized")
            ts = await self._request("thread/start", {})
            self.thread_id = ts["thread_id"]

    async def close(self) -> None:
        if self._reader_task is not None:
            self._reader_task.cancel()
            try:
                await self._reader_task
            except (asyncio.CancelledError, Exception):
                pass
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

    async def _request(self, method: str, params: dict[str, Any]) -> dict[str, Any]:
        if self._ws is None:
            raise SdkConnectionError("not connected")
        self._next_id += 1
        rid = self._next_id
        fut: asyncio.Future[dict[str, Any]] = asyncio.get_running_loop().create_future()
        self._pending[rid] = fut
        try:
            await self._ws.send(json.dumps({
                "jsonrpc": "2.0", "id": rid, "method": method, "params": params,
            }))
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
                        SdkConnectionError(
                            f"rpc {err.get('code')}: {err.get('message')}"
                        )
                    )
                else:
                    fut.set_result(msg.get("result", {}))
        except websockets.ConnectionClosed:
            pass
```

- [ ] **Step 5: Run tests, confirm pass**

Run: `pytest tests/test_client.py -v`
Expected: `4 passed`.

- [ ] **Step 6: Commit**

```bash
git add sdk/python/src/agentserver_sdk/errors.py \
        sdk/python/src/agentserver_sdk/client.py \
        sdk/python/tests/test_client.py
git commit -m "feat(sdk): WSClient connect + handshake

opens ws with Bearer auth, runs initialize / initialized / thread/start,
caches thread_id, idempotent reconnect. Errors mapped to SdkError."
```

---

## Task 4: WSClient — `mcp_tool_call`

**Files:**
- Modify: `sdk/python/src/agentserver_sdk/client.py`
- Modify: `sdk/python/tests/test_client.py`

- [ ] **Step 1: Write failing tests**

Append to `sdk/python/tests/test_client.py`:

```python
async def test_mcp_tool_call_round_trip(stub):
    stub.on("mcpServer/tool/call", lambda p: {
        "content": [{"type": "text", "text": "hello"}],
        "structuredContent": {"k": "v"},
        "isError": False,
    })
    c = WSClient(stub.url, token="t", workspace_id="ws-1", user_id="u-1")
    await c.connect()
    try:
        result = await c.mcp_tool_call(
            server="env_mcp",
            tool="shell",
            arguments={"environment_id": "alpha", "command": "ls"},
        )
        assert result["content"][0]["text"] == "hello"
        # Find the tool/call frame
        call = next(m for m in stub.received if m.get("method") == "mcpServer/tool/call")
        assert call["params"]["thread_id"] == "stub-thread-1"
        assert call["params"]["server"] == "env_mcp"
        assert call["params"]["tool"] == "shell"
        assert call["params"]["arguments"]["command"] == "ls"
        assert call["params"]["_meta"]["agentserver_user_id"] == "u-1"
        assert call["params"]["_meta"]["agentserver_workspace_id"] == "ws-1"
    finally:
        await c.close()


async def test_mcp_tool_call_concurrent_dont_cross_streams(stub):
    """Two concurrent calls must each get their own response by id."""
    counter = {"n": 0}
    def handler(p):
        counter["n"] += 1
        return {"content": [{"type": "text", "text": f"call-{counter['n']}"}], "isError": False}
    stub.on("mcpServer/tool/call", handler)

    c = WSClient(stub.url, token="t", workspace_id="ws", user_id="u")
    await c.connect()
    try:
        import asyncio
        r1, r2 = await asyncio.gather(
            c.mcp_tool_call(server="s", tool="t", arguments={"environment_id": "a"}),
            c.mcp_tool_call(server="s", tool="t", arguments={"environment_id": "a"}),
        )
        # Both got valid distinct responses
        texts = {r1["content"][0]["text"], r2["content"][0]["text"]}
        assert texts == {"call-1", "call-2"}
    finally:
        await c.close()


async def test_mcp_tool_call_rpc_error_raises(stub):
    stub.on("mcpServer/tool/call", lambda p: {"error": {"code": -32602, "message": "bad params"}})
    c = WSClient(stub.url, token="t", workspace_id="ws", user_id="u")
    await c.connect()
    try:
        with pytest.raises(SdkConnectionError, match="bad params"):
            await c.mcp_tool_call(server="s", tool="t", arguments={"environment_id": "a"})
    finally:
        await c.close()
```

- [ ] **Step 2: Run, confirm failure**

Run: `pytest tests/test_client.py::test_mcp_tool_call_round_trip -v`
Expected: FAIL — `WSClient` has no `mcp_tool_call`.

- [ ] **Step 3: Add `mcp_tool_call` to WSClient**

Append to `sdk/python/src/agentserver_sdk/client.py` inside the `WSClient` class:

```python
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
```

- [ ] **Step 4: Run tests, confirm pass**

Run: `pytest tests/test_client.py -v`
Expected: `7 passed`.

- [ ] **Step 5: Commit**

```bash
git add sdk/python/src/agentserver_sdk/client.py sdk/python/tests/test_client.py
git commit -m "feat(sdk): WSClient.mcp_tool_call

issues mcpServer/tool/call with thread_id + _meta (user, workspace);
concurrent calls dispatched by id; rpc errors raise."
```

---

## Task 5: Result types — `ShellResult`, `ToolMetadata`, `OperationRecord`

**Files:**
- Create: `sdk/python/src/agentserver_sdk/types.py`
- Create: `sdk/python/tests/test_types.py`

- [ ] **Step 1: Write failing tests**

Create `sdk/python/tests/test_types.py`:

```python
import pytest

from agentserver_sdk.types import ShellResult, ToolMetadata, OperationRecord


def test_shell_result_from_mcp_text_content():
    raw = {
        "content": [{"type": "text", "text": "hi"}],
        "structuredContent": {"stdout": "hi", "stderr": "", "exit_code": 0},
        "isError": False,
    }
    r = ShellResult.from_mcp(raw)
    assert r.stdout == "hi"
    assert r.stderr == ""
    assert r.exit_code == 0


def test_shell_result_from_mcp_fallback_when_no_structured():
    raw = {"content": [{"type": "text", "text": "fallback"}], "isError": False}
    r = ShellResult.from_mcp(raw)
    assert r.stdout == "fallback"
    assert r.stderr == ""
    assert r.exit_code == 0


def test_shell_result_exit_code_nonzero():
    raw = {
        "content": [{"type": "text", "text": ""}],
        "structuredContent": {"stdout": "", "stderr": "boom", "exit_code": 1},
        "isError": False,
    }
    r = ShellResult.from_mcp(raw)
    assert r.exit_code == 1
    assert r.stderr == "boom"


def test_tool_metadata_from_dict():
    m = ToolMetadata.from_dict({
        "name": "submit_task",
        "description": "submit HPC job",
        "inputSchema": {"type": "object"},
    })
    assert m.name == "submit_task"
    assert m.description == "submit HPC job"
    assert m.kind == "custom"  # default for non-core


def test_tool_metadata_core_marker():
    m = ToolMetadata.from_dict({"name": "shell", "description": "x", "inputSchema": {}})
    assert m.kind == "core"


def test_operation_record_from_dict():
    o = OperationRecord.from_dict({
        "id": "op_1",
        "env_id": "alpha",
        "tool": "shell",
        "is_error": False,
        "started_at": "2026-05-18T10:00:00Z",
        "duration_ms": 42,
        "user_id": "u",
        "source": "sdk",
    })
    assert o.id == "op_1"
    assert o.is_error is False
    assert o.duration_ms == 42
```

- [ ] **Step 2: Run, confirm failure**

Run: `pytest tests/test_types.py -v`
Expected: FAIL — `agentserver_sdk.types` not found.

- [ ] **Step 3: Implement types**

Create `sdk/python/src/agentserver_sdk/types.py`:

```python
"""SDK result + metadata types. Pure data; no I/O.

`from_mcp` / `from_dict` constructors take the JSON-shape returned by the
gateway and produce typed Python objects. Wrappers are minimal — most
fields are exposed as-is.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

CORE_TOOLS = frozenset({
    "shell", "read_file", "write_file", "apply_patch",
    "exec_command", "write_stdin", "read_output", "terminate",
    "copy_path",
})


@dataclass
class ShellResult:
    stdout: str
    stderr: str
    exit_code: int
    raw: dict[str, Any] = field(default_factory=dict)

    @classmethod
    def from_mcp(cls, raw: dict[str, Any]) -> ShellResult:
        sc = raw.get("structuredContent") or {}
        if sc:
            return cls(
                stdout=sc.get("stdout", ""),
                stderr=sc.get("stderr", ""),
                exit_code=int(sc.get("exit_code", 0)),
                raw=raw,
            )
        # Fallback: join text content as stdout
        text = "".join(
            item.get("text", "") for item in raw.get("content", [])
            if item.get("type") == "text"
        )
        return cls(stdout=text, stderr="", exit_code=0, raw=raw)


@dataclass
class ToolMetadata:
    name: str
    description: str
    input_schema: dict[str, Any]
    kind: str  # "core" | "custom"

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> ToolMetadata:
        name = d["name"]
        return cls(
            name=name,
            description=d.get("description", ""),
            input_schema=d.get("inputSchema", {}),
            kind="core" if name in CORE_TOOLS else "custom",
        )


@dataclass
class OperationRecord:
    id: str
    env_id: str
    tool: str
    is_error: bool
    started_at: str
    duration_ms: int
    user_id: str | None
    source: str
    arguments: dict[str, Any] | None = None
    result_summary: str | None = None

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> OperationRecord:
        return cls(
            id=d["id"],
            env_id=d["env_id"],
            tool=d["tool"],
            is_error=bool(d.get("is_error", False)),
            started_at=d.get("started_at", ""),
            duration_ms=int(d.get("duration_ms", 0)),
            user_id=d.get("user_id"),
            source=d.get("source", ""),
            arguments=d.get("arguments"),
            result_summary=d.get("result_summary"),
        )
```

- [ ] **Step 4: Run tests, confirm pass**

Run: `pytest tests/test_types.py -v`
Expected: `5 passed`.

- [ ] **Step 5: Commit**

```bash
git add sdk/python/src/agentserver_sdk/types.py sdk/python/tests/test_types.py
git commit -m "feat(sdk): result + metadata types

ShellResult.from_mcp (structuredContent preferred, text fallback),
ToolMetadata with core/custom classification, OperationRecord."
```

---

## Task 6: `Env` — core methods (`shell`, `read_file`, `write_file`, `apply_patch`, `call`)

**Files:**
- Create: `sdk/python/src/agentserver_sdk/env.py`
- Create: `sdk/python/tests/test_env.py`

- [ ] **Step 1: Write failing tests**

Create `sdk/python/tests/test_env.py`:

```python
import base64
import pytest

from agentserver_sdk.client import WSClient
from agentserver_sdk.env import Env
from agentserver_sdk.errors import ToolError
from agentserver_sdk.types import ShellResult, ToolMetadata


async def _connected_client(stub):
    c = WSClient(stub.url, token="t", workspace_id="ws", user_id="u")
    await c.connect()
    return c


def _tool(name, desc=""):
    return ToolMetadata(name=name, description=desc, input_schema={}, kind="core" if name in {
        "shell", "read_file", "write_file", "apply_patch", "exec_command",
        "write_stdin", "read_output", "terminate", "copy_path",
    } else "custom")


async def test_env_call_injects_environment_id(stub):
    stub.on("mcpServer/tool/call", lambda p: {"content": [], "isError": False})
    c = await _connected_client(stub)
    env = Env(name="alpha", type="shell", tools=[_tool("shell")], _client=c)
    try:
        await env.call("shell", {"command": "ls"})
        call = next(m for m in stub.received if m.get("method") == "mcpServer/tool/call")
        assert call["params"]["arguments"]["environment_id"] == "alpha"
        assert call["params"]["arguments"]["command"] == "ls"
        assert call["params"]["tool"] == "shell"
    finally:
        await c.close()


async def test_env_shell_returns_shell_result(stub):
    stub.on("mcpServer/tool/call", lambda p: {
        "content": [{"type": "text", "text": "hi"}],
        "structuredContent": {"stdout": "hi", "stderr": "", "exit_code": 0},
        "isError": False,
    })
    c = await _connected_client(stub)
    env = Env(name="alpha", type="shell", tools=[_tool("shell")], _client=c)
    try:
        r = await env.shell("echo hi")
        assert isinstance(r, ShellResult)
        assert r.stdout == "hi"
        assert r.exit_code == 0
    finally:
        await c.close()


async def test_env_read_file_returns_bytes(stub):
    payload = b"hello binary"
    stub.on("mcpServer/tool/call", lambda p: {
        "content": [{"type": "text", "text": base64.b64encode(payload).decode()}],
        "structuredContent": {"encoding": "base64"},
        "isError": False,
    })
    c = await _connected_client(stub)
    env = Env(name="alpha", type="shell", tools=[_tool("read_file")], _client=c)
    try:
        data = await env.read_file("/x")
        assert data == payload
    finally:
        await c.close()


async def test_env_is_error_raises_tool_error(stub):
    stub.on("mcpServer/tool/call", lambda p: {
        "content": [{"type": "text", "text": "boom"}],
        "isError": True,
    })
    c = await _connected_client(stub)
    env = Env(name="alpha", type="shell", tools=[_tool("shell")], _client=c)
    try:
        with pytest.raises(ToolError) as ei:
            await env.shell("badcmd")
        assert ei.value.env == "alpha"
        assert ei.value.tool == "shell"
        assert "boom" in ei.value.message
    finally:
        await c.close()


async def test_env_write_file_passes_bytes_as_b64(stub):
    stub.on("mcpServer/tool/call", lambda p: {"content": [], "isError": False})
    c = await _connected_client(stub)
    env = Env(name="alpha", type="shell", tools=[_tool("write_file")], _client=c)
    try:
        await env.write_file("/x", b"\x00\x01\x02")
        call = next(m for m in stub.received if m.get("method") == "mcpServer/tool/call")
        args = call["params"]["arguments"]
        assert args["path"] == "/x"
        assert base64.b64decode(args["content_b64"]) == b"\x00\x01\x02"
    finally:
        await c.close()


async def test_env_apply_patch_passes_through(stub):
    stub.on("mcpServer/tool/call", lambda p: {"content": [], "isError": False})
    c = await _connected_client(stub)
    env = Env(name="alpha", type="shell", tools=[_tool("apply_patch")], _client=c)
    try:
        await env.apply_patch("*** Patch...")
        call = next(m for m in stub.received if m.get("method") == "mcpServer/tool/call")
        assert call["params"]["arguments"]["patch"] == "*** Patch..."
    finally:
        await c.close()
```

- [ ] **Step 2: Run, confirm failure**

Run: `pytest tests/test_env.py -v`
Expected: FAIL — `agentserver_sdk.env` not found.

- [ ] **Step 3: Implement Env (core methods only — dynamic dispatch in Task 8)**

Create `sdk/python/src/agentserver_sdk/env.py`:

```python
"""Env class — one instance per executor; wraps env-mcp tool calls."""
from __future__ import annotations

import base64
from dataclasses import dataclass, field
from typing import Any, TYPE_CHECKING

from .errors import ToolError
from .types import CORE_TOOLS, ShellResult, ToolMetadata

if TYPE_CHECKING:
    from .client import WSClient


_TOOL_SERVER = "env_mcp"  # all env-mcp tools live under one MCP server name


@dataclass
class Env:
    name: str
    type: str
    tools: list[ToolMetadata]
    _client: "WSClient"
    _tool_index: dict[str, ToolMetadata] = field(init=False)

    def __post_init__(self) -> None:
        self._tool_index = {t.name: t for t in self.tools}

    # ---------- generic dispatch ----------

    async def call(self, tool: str, arguments: dict[str, Any] | None = None) -> dict[str, Any]:
        """Universal MCP tool call — even tools the SDK doesn't know about.

        `environment_id` is injected automatically. Raises ToolError on
        isError=true. Returns the raw MCP result dict.
        """
        args = dict(arguments or {})
        args.setdefault("environment_id", self.name)
        raw = await self._client.mcp_tool_call(
            server=_TOOL_SERVER, tool=tool, arguments=args,
        )
        if raw.get("isError"):
            msg = _extract_error_text(raw)
            raise ToolError(tool=tool, env=self.name, message=msg, raw=raw)
        return raw

    # ---------- core typed wrappers ----------

    async def shell(self, command: str, *, timeout: int | None = None) -> ShellResult:
        args: dict[str, Any] = {"command": command}
        if timeout is not None:
            args["timeout_s"] = timeout
        raw = await self.call("shell", args)
        return ShellResult.from_mcp(raw)

    async def read_file(self, path: str) -> bytes:
        raw = await self.call("read_file", {"path": path})
        return _decode_file_content(raw)

    async def write_file(self, path: str, content: bytes) -> None:
        await self.call("write_file", {
            "path": path,
            "content_b64": base64.b64encode(content).decode("ascii"),
        })

    async def apply_patch(self, patch: str) -> None:
        await self.call("apply_patch", {"patch": patch})


# ---------- helpers ----------

def _extract_error_text(raw: dict[str, Any]) -> str:
    items = raw.get("content", [])
    texts = [it.get("text", "") for it in items if it.get("type") == "text"]
    return " ".join(texts) or "tool reported isError=true"


def _decode_file_content(raw: dict[str, Any]) -> bytes:
    """env-mcp's read_file convention: text content is base64 if
    structuredContent.encoding == 'base64', else raw text bytes.

    v0 stub mirrors this; real env-mcp may differ — adjust if needed."""
    sc = raw.get("structuredContent") or {}
    items = raw.get("content", [])
    text = "".join(it.get("text", "") for it in items if it.get("type") == "text")
    if sc.get("encoding") == "base64":
        return base64.b64decode(text)
    return text.encode("utf-8")
```

- [ ] **Step 4: Run tests, confirm pass**

Run: `pytest tests/test_env.py -v`
Expected: `6 passed`.

- [ ] **Step 5: Commit**

```bash
git add sdk/python/src/agentserver_sdk/env.py sdk/python/tests/test_env.py
git commit -m "feat(sdk): Env core typed methods + generic call

shell/read_file/write_file/apply_patch wrappers; isError=true raises
ToolError with env+tool context; environment_id auto-injected."
```

---

## Task 7: `Process` — async context manager

**Files:**
- Create: `sdk/python/src/agentserver_sdk/process.py`
- Modify: `sdk/python/src/agentserver_sdk/env.py` (add `spawn`)
- Create: `sdk/python/tests/test_process.py`

- [ ] **Step 1: Write failing tests**

Create `sdk/python/tests/test_process.py`:

```python
import pytest

from agentserver_sdk.client import WSClient
from agentserver_sdk.env import Env
from agentserver_sdk.types import ToolMetadata


def _tool(name): return ToolMetadata(name=name, description="", input_schema={}, kind="core")


async def test_spawn_calls_exec_command_and_returns_process(stub):
    stub.on("mcpServer/tool/call", lambda p: {
        "content": [],
        "structuredContent": {"session_id": "sess-1"},
        "isError": False,
    })
    c = WSClient(stub.url, token="t", workspace_id="w", user_id="u")
    await c.connect()
    env = Env(name="alpha", type="shell",
              tools=[_tool("exec_command"), _tool("write_stdin"),
                     _tool("read_output"), _tool("terminate")],
              _client=c)
    try:
        async with env.spawn("./run.sh") as proc:
            assert proc.session_id == "sess-1"
            # exec_command was sent
            calls = [m for m in stub.received if m.get("method") == "mcpServer/tool/call"]
            assert calls[0]["params"]["tool"] == "exec_command"
            assert calls[0]["params"]["arguments"]["command"] == "./run.sh"
        # On exit, terminate was sent
        all_tools = [m["params"]["tool"] for m in stub.received if m.get("method") == "mcpServer/tool/call"]
        assert "terminate" in all_tools
    finally:
        await c.close()


async def test_process_write_and_read(stub):
    state = {"step": 0}
    def handler(p):
        state["step"] += 1
        tool = p["tool"]
        if tool == "exec_command":
            return {"structuredContent": {"session_id": "s-1"}, "isError": False, "content": []}
        if tool == "write_stdin":
            return {"content": [], "isError": False}
        if tool == "read_output":
            return {
                "structuredContent": {"chunk_b64": "aGVsbG8="},  # base64('hello')
                "isError": False,
                "content": [{"type": "text", "text": "hello"}],
            }
        if tool == "terminate":
            return {"content": [], "isError": False}
        return {"error": {"code": -32601, "message": "unknown"}}

    stub.on("mcpServer/tool/call", handler)

    c = WSClient(stub.url, token="t", workspace_id="w", user_id="u")
    await c.connect()
    env = Env(name="alpha", type="shell",
              tools=[_tool("exec_command"), _tool("write_stdin"),
                     _tool("read_output"), _tool("terminate")],
              _client=c)
    try:
        async with env.spawn("./run.sh") as proc:
            await proc.write_stdin(b"hi\n")
            data = await proc.read_output(timeout=1.0)
            assert data == b"hello"
    finally:
        await c.close()


async def test_process_terminate_runs_even_on_exception(stub):
    stub.on("mcpServer/tool/call", lambda p:
        {"structuredContent": {"session_id": "s"}, "isError": False, "content": []}
        if p["tool"] == "exec_command"
        else {"content": [], "isError": False})
    c = WSClient(stub.url, token="t", workspace_id="w", user_id="u")
    await c.connect()
    env = Env(name="alpha", type="shell",
              tools=[_tool("exec_command"), _tool("terminate"), _tool("write_stdin"), _tool("read_output")],
              _client=c)
    try:
        with pytest.raises(RuntimeError):
            async with env.spawn("./run.sh"):
                raise RuntimeError("boom")
        # terminate happened
        tools = [m["params"]["tool"] for m in stub.received if m.get("method") == "mcpServer/tool/call"]
        assert tools.count("terminate") == 1
    finally:
        await c.close()
```

- [ ] **Step 2: Run, confirm failure**

Run: `pytest tests/test_process.py -v`
Expected: FAIL — `Env.spawn` not found.

- [ ] **Step 3: Implement Process**

Create `sdk/python/src/agentserver_sdk/process.py`:

```python
"""Process — async context manager wrapping exec_command/stdin/output/terminate."""
from __future__ import annotations

import base64
from typing import TYPE_CHECKING, Any

if TYPE_CHECKING:
    from .env import Env


class Process:
    def __init__(self, env: "Env", session_id: str) -> None:
        self.env = env
        self.session_id = session_id
        self._terminated = False

    async def __aenter__(self) -> "Process":
        return self

    async def __aexit__(self, exc_type, exc, tb) -> None:
        await self.terminate()

    async def write_stdin(self, data: bytes) -> None:
        await self.env.call("write_stdin", {
            "session_id": self.session_id,
            "data_b64": base64.b64encode(data).decode("ascii"),
        })

    async def read_output(self, timeout: float | None = None) -> bytes:
        args: dict[str, Any] = {"session_id": self.session_id}
        if timeout is not None:
            args["timeout_ms"] = int(timeout * 1000)
        raw = await self.env.call("read_output", args)
        sc = raw.get("structuredContent") or {}
        b64 = sc.get("chunk_b64")
        if b64 is not None:
            return base64.b64decode(b64)
        # fallback to text content
        texts = [it.get("text", "") for it in raw.get("content", []) if it.get("type") == "text"]
        return "".join(texts).encode("utf-8")

    async def terminate(self) -> None:
        if self._terminated:
            return
        self._terminated = True
        try:
            await self.env.call("terminate", {"session_id": self.session_id})
        except Exception:
            # best-effort; don't mask a user exception
            pass
```

- [ ] **Step 4: Add `Env.spawn`**

Append to `sdk/python/src/agentserver_sdk/env.py`:

```python
    async def spawn(self, command: str) -> "Process":
        """Start a long-running command. Use as `async with env.spawn(cmd) as proc:`."""
        from .process import Process  # avoid circular at module load
        raw = await self.call("exec_command", {"command": command})
        sc = raw.get("structuredContent") or {}
        session_id = sc.get("session_id")
        if not session_id:
            raise ToolError(tool="exec_command", env=self.name,
                            message="exec_command did not return session_id", raw=raw)
        return Process(self, session_id)
```

- [ ] **Step 5: Run, confirm pass**

Run: `pytest tests/test_process.py -v`
Expected: `3 passed`.

- [ ] **Step 6: Commit**

```bash
git add sdk/python/src/agentserver_sdk/process.py \
        sdk/python/src/agentserver_sdk/env.py \
        sdk/python/tests/test_process.py
git commit -m "feat(sdk): Process async context manager

env.spawn(cmd) returns Process; write_stdin/read_output/terminate;
auto-terminate on context exit even if cell raises."
```

---

## Task 8: `Env` — dynamic dispatch via `__getattr__` + `setattr`

**Files:**
- Modify: `sdk/python/src/agentserver_sdk/env.py`
- Modify: `sdk/python/tests/test_env.py`

- [ ] **Step 1: Write failing tests**

Append to `sdk/python/tests/test_env.py`:

```python
async def test_custom_tool_called_via_attribute(stub):
    """A custom tool surfaced in tools metadata becomes a method on env."""
    stub.on("mcpServer/tool/call", lambda p: {
        "content": [{"type": "text", "text": "job-123"}],
        "isError": False,
    })
    c = await _connected_client(stub)
    custom = ToolMetadata(name="submit_task",
                          description="submit HPC job",
                          input_schema={"type": "object"},
                          kind="custom")
    env = Env(name="hpc-a", type="hpc", tools=[custom], _client=c)
    try:
        # Method exists thanks to setattr in __post_init__
        assert hasattr(env, "submit_task")
        result = await env.submit_task(script="x", resources={"gpus": 4})
        call = next(m for m in stub.received if m.get("method") == "mcpServer/tool/call")
        assert call["params"]["tool"] == "submit_task"
        assert call["params"]["arguments"]["environment_id"] == "hpc-a"
        assert call["params"]["arguments"]["script"] == "x"
        assert call["params"]["arguments"]["resources"] == {"gpus": 4}
        assert result["content"][0]["text"] == "job-123"
    finally:
        await c.close()


async def test_unknown_attribute_raises_attribute_error(stub):
    c = await _connected_client(stub)
    env = Env(name="alpha", type="shell", tools=[_tool("shell")], _client=c)
    try:
        with pytest.raises(AttributeError):
            env.nonexistent_tool
    finally:
        await c.close()


async def test_dir_lists_custom_tools(stub):
    c = await _connected_client(stub)
    env = Env(name="hpc", type="hpc",
              tools=[ToolMetadata(name="submit_task", description="", input_schema={}, kind="custom")],
              _client=c)
    try:
        assert "submit_task" in dir(env)
    finally:
        await c.close()
```

- [ ] **Step 2: Run, confirm failure**

Run: `pytest tests/test_env.py -v -k custom`
Expected: FAIL — `Env` has no `submit_task`.

- [ ] **Step 3: Extend `Env.__post_init__` to bind dynamic methods**

Replace the `__post_init__` in `sdk/python/src/agentserver_sdk/env.py`:

```python
    def __post_init__(self) -> None:
        self._tool_index = {t.name: t for t in self.tools}
        for tool in self.tools:
            if tool.kind == "core":
                continue  # core tools have typed wrappers
            method = self._make_dynamic(tool)
            object.__setattr__(self, tool.name, method)

    def _make_dynamic(self, tool: ToolMetadata):
        tool_name = tool.name

        async def method(**kwargs: Any) -> dict[str, Any]:
            return await self.call(tool_name, kwargs)

        method.__name__ = tool_name
        method.__doc__ = tool.description or f"Call env-mcp tool {tool_name}."
        return method

    def __dir__(self) -> list[str]:
        base = list(super().__dir__())
        base.extend(t.name for t in self.tools if t.kind == "custom")
        return base
```

- [ ] **Step 4: Run, confirm pass**

Run: `pytest tests/test_env.py -v`
Expected: All env tests pass (6 existing + 3 new = 9).

- [ ] **Step 5: Commit**

```bash
git add sdk/python/src/agentserver_sdk/env.py sdk/python/tests/test_env.py
git commit -m "feat(sdk): Env dynamic dispatch for custom tools

setattr custom tools at construction; __dir__ surfaces them for
tab-complete; AttributeError for unknown."
```

---

## Task 9: `Ctx` — connection, `envs()`, `env()`, `copy()`, `history()`

**Files:**
- Create: `sdk/python/src/agentserver_sdk/ctx.py`
- Create: `sdk/python/tests/test_ctx.py`
- Modify: `sdk/python/src/agentserver_sdk/__init__.py`

- [ ] **Step 1: Write failing tests**

Create `sdk/python/tests/test_ctx.py`:

```python
import os
import pytest

from agentserver_sdk.ctx import Ctx


def _setup_stub_envs(stub):
    """Make stub return two envs (alpha + hpc) via env/capabilities + envs/list."""
    stub.on("envs/list", lambda p: {"envs": [
        {"name": "alpha", "type": "shell"},
        {"name": "hpc",   "type": "hpc"},
    ]})

    def caps(p):
        env_id = p["env_id"]
        if env_id == "alpha":
            return {"tools": [
                {"name": "shell", "description": "run sh", "inputSchema": {}},
                {"name": "read_file", "description": "read", "inputSchema": {}},
            ]}
        if env_id == "hpc":
            return {"tools": [
                {"name": "shell", "description": "run sh", "inputSchema": {}},
                {"name": "submit_task", "description": "submit HPC", "inputSchema": {}},
            ]}
        return {"error": {"code": -32602, "message": "unknown env"}}
    stub.on("env/capabilities", caps)


async def test_ctx_from_env_reads_env_vars(monkeypatch, stub):
    monkeypatch.setenv("AGENTSERVER_GATEWAY_URL", stub.url)
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_TOKEN", "tok")
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_ID", "ws")
    monkeypatch.setenv("AGENTSERVER_USER_ID", "u")
    ctx = Ctx.from_env()
    assert ctx.gateway_url == stub.url
    assert ctx.workspace_id == "ws"


async def test_ctx_envs_returns_env_objects(monkeypatch, stub):
    _setup_stub_envs(stub)
    monkeypatch.setenv("AGENTSERVER_GATEWAY_URL", stub.url)
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_TOKEN", "tok")
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_ID", "ws")
    monkeypatch.setenv("AGENTSERVER_USER_ID", "u")
    ctx = Ctx.from_env()
    try:
        envs = await ctx.envs()
        assert {e.name for e in envs} == {"alpha", "hpc"}
        hpc = next(e for e in envs if e.name == "hpc")
        assert hasattr(hpc, "submit_task")  # custom tool surfaced
    finally:
        await ctx.close()


async def test_ctx_env_by_name(monkeypatch, stub):
    _setup_stub_envs(stub)
    monkeypatch.setenv("AGENTSERVER_GATEWAY_URL", stub.url)
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_TOKEN", "tok")
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_ID", "ws")
    monkeypatch.setenv("AGENTSERVER_USER_ID", "u")
    ctx = Ctx.from_env()
    try:
        alpha = await ctx.env("alpha")
        assert alpha.name == "alpha"
        assert alpha.type == "shell"
    finally:
        await ctx.close()


async def test_ctx_env_missing_raises(monkeypatch, stub):
    _setup_stub_envs(stub)
    monkeypatch.setenv("AGENTSERVER_GATEWAY_URL", stub.url)
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_TOKEN", "tok")
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_ID", "ws")
    monkeypatch.setenv("AGENTSERVER_USER_ID", "u")
    ctx = Ctx.from_env()
    try:
        with pytest.raises(KeyError):
            await ctx.env("nope")
    finally:
        await ctx.close()


async def test_ctx_copy_uses_copy_path_tool(monkeypatch, stub):
    _setup_stub_envs(stub)
    stub.on("mcpServer/tool/call", lambda p: {"content": [], "isError": False})
    monkeypatch.setenv("AGENTSERVER_GATEWAY_URL", stub.url)
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_TOKEN", "tok")
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_ID", "ws")
    monkeypatch.setenv("AGENTSERVER_USER_ID", "u")
    ctx = Ctx.from_env()
    try:
        alpha = await ctx.env("alpha")
        hpc = await ctx.env("hpc")
        await ctx.copy(src=(alpha, "/a/x"), dst=(hpc, "/b/x"))
        call = next(m for m in stub.received if m.get("method") == "mcpServer/tool/call")
        assert call["params"]["tool"] == "copy_path"
        args = call["params"]["arguments"]
        assert args["src_env"] == "alpha"
        assert args["src_path"] == "/a/x"
        assert args["dst_env"] == "hpc"
        assert args["dst_path"] == "/b/x"
    finally:
        await ctx.close()


async def test_ctx_history_returns_records(monkeypatch, stub):
    stub.on("envs/list", lambda p: {"envs": []})
    stub.on("operations/list", lambda p: {"operations": [
        {"id": "op_1", "env_id": "alpha", "tool": "shell", "is_error": False,
         "started_at": "2026-05-18T10:00:00Z", "duration_ms": 5,
         "user_id": "u", "source": "sdk"},
    ]})
    monkeypatch.setenv("AGENTSERVER_GATEWAY_URL", stub.url)
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_TOKEN", "tok")
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_ID", "ws")
    monkeypatch.setenv("AGENTSERVER_USER_ID", "u")
    ctx = Ctx.from_env()
    try:
        ops = await ctx.history(limit=10)
        assert len(ops) == 1
        assert ops[0].tool == "shell"
        # Ensure filter forwarded
        call = next(m for m in stub.received if m.get("method") == "operations/list")
        assert call["params"]["limit"] == 10
    finally:
        await ctx.close()
```

- [ ] **Step 2: Run, confirm failure**

Run: `pytest tests/test_ctx.py -v`
Expected: FAIL — `agentserver_sdk.ctx` not found.

- [ ] **Step 3: Implement Ctx**

Create `sdk/python/src/agentserver_sdk/ctx.py`:

```python
"""Ctx — workspace handle, entry point for the SDK.

`Ctx.from_env()` constructs a lazy handle (no I/O). The first `await`
on any method triggers WS connect + handshake. One thread per Ctx;
cached internally.
"""
from __future__ import annotations

import os
from dataclasses import dataclass
from typing import Any

from .client import WSClient
from .env import Env
from .types import OperationRecord, ToolMetadata


@dataclass
class Ctx:
    gateway_url: str
    workspace_id: str
    user_id: str | None
    _client: WSClient

    @classmethod
    def from_env(cls) -> "Ctx":
        url = os.environ.get("AGENTSERVER_GATEWAY_URL",
                             "ws://localhost:8086/notebook/ws")
        token = os.environ.get("AGENTSERVER_WORKSPACE_TOKEN", "")
        workspace_id = os.environ.get("AGENTSERVER_WORKSPACE_ID", "")
        user_id = os.environ.get("AGENTSERVER_USER_ID")
        client = WSClient(url, token=token, workspace_id=workspace_id, user_id=user_id)
        return cls(gateway_url=url, workspace_id=workspace_id,
                   user_id=user_id, _client=client)

    async def envs(self) -> list[Env]:
        """List envs in the workspace. Caches inside Ctx for the kernel
        lifetime — call `refresh()` to refetch."""
        if self._envs_cache is None:
            await self._client.connect()
            listing = await self._client._request("envs/list", {})
            envs: list[Env] = []
            for e in listing.get("envs", []):
                caps = await self._client._request(
                    "env/capabilities", {"env_id": e["name"]},
                )
                tools = [ToolMetadata.from_dict(t) for t in caps.get("tools", [])]
                envs.append(Env(name=e["name"], type=e.get("type", ""),
                                tools=tools, _client=self._client))
            self._envs_cache = envs
        return list(self._envs_cache)

    async def env(self, name: str) -> Env:
        for e in await self.envs():
            if e.name == name:
                return e
        raise KeyError(f"env not found: {name}")

    async def refresh(self) -> None:
        self._envs_cache = None
        await self.envs()

    async def copy(self, *, src: tuple[Env, str], dst: tuple[Env, str]) -> None:
        src_env, src_path = src
        dst_env, dst_path = dst
        await self._client.mcp_tool_call(
            server="env_mcp", tool="copy_path",
            arguments={
                "src_env": src_env.name, "src_path": src_path,
                "dst_env": dst_env.name, "dst_path": dst_path,
            },
        )

    async def history(
        self,
        *,
        limit: int = 100,
        env: str | None = None,
        tool: str | None = None,
        is_error: bool | None = None,
        since: str | None = None,
        id: str | None = None,  # noqa: A002 (shadow of builtin is fine here)
    ) -> list[OperationRecord]:
        await self._client.connect()
        params: dict[str, Any] = {"limit": limit}
        if env is not None: params["env_id"] = env
        if tool is not None: params["tool"] = tool
        if is_error is not None: params["is_error"] = is_error
        if since is not None: params["since"] = since
        if id is not None: params["id"] = id
        resp = await self._client._request("operations/list", params)
        return [OperationRecord.from_dict(o) for o in resp.get("operations", [])]

    async def close(self) -> None:
        await self._client.close()

    # ---------- dataclass field default for cache ----------

    def __post_init__(self) -> None:
        self._envs_cache: list[Env] | None = None
```

- [ ] **Step 4: Update package exports**

Replace `sdk/python/src/agentserver_sdk/__init__.py`:

```python
"""agentserver_sdk — Python SDK for agentserver envs."""

from .ctx import Ctx
from .env import Env
from .errors import ConnectionError, NotConnectedError, SdkError, ToolError
from .process import Process
from .types import OperationRecord, ShellResult, ToolMetadata

__version__ = "0.1.0"
__all__ = [
    "Ctx", "Env", "Process",
    "ShellResult", "ToolMetadata", "OperationRecord",
    "SdkError", "ConnectionError", "NotConnectedError", "ToolError",
]
```

- [ ] **Step 5: Run all tests, confirm pass**

Run: `pytest -v`
Expected: All previous tests + 6 new ctx tests pass.

- [ ] **Step 6: Commit**

```bash
git add sdk/python/src/agentserver_sdk/ctx.py \
        sdk/python/src/agentserver_sdk/__init__.py \
        sdk/python/tests/test_ctx.py
git commit -m "feat(sdk): Ctx workspace handle

from_env constructor; envs() + env() with per-env capability fetch;
copy() cross-env; history() over operations/list RPC; public exports."
```

---

## Task 10: Jupyter `_repr_html_` rendering

**Files:**
- Create: `sdk/python/src/agentserver_sdk/_repr.py`
- Modify: `sdk/python/src/agentserver_sdk/env.py` (add `_repr_html_`)
- Modify: `sdk/python/src/agentserver_sdk/types.py` (add `_repr_html_`)
- Modify: `sdk/python/src/agentserver_sdk/errors.py` (add `_repr_html_` to ToolError)
- Create: `sdk/python/tests/test_repr.py`

- [ ] **Step 1: Write failing tests**

Create `sdk/python/tests/test_repr.py`:

```python
from agentserver_sdk._repr import envs_table_html
from agentserver_sdk.env import Env
from agentserver_sdk.errors import ToolError
from agentserver_sdk.types import ShellResult, ToolMetadata


class _FakeClient:
    pass


def _env(name, tools):
    return Env(name=name, type="shell",
               tools=[ToolMetadata(name=t, description="", input_schema={},
                                   kind="core" if t in {"shell"} else "custom") for t in tools],
               _client=_FakeClient())


def test_env_repr_html_contains_name_and_tool_count():
    e = _env("alpha", ["shell", "submit_task"])
    html = e._repr_html_()
    assert "alpha" in html
    assert "2" in html  # tool count


def test_envs_table_html_has_one_row_per_env():
    envs = [_env("alpha", ["shell"]), _env("beta", ["shell", "submit_task"])]
    html = envs_table_html(envs)
    assert html.count("<tr") >= 3  # header + 2 envs
    assert "alpha" in html
    assert "beta" in html


def test_shell_result_repr_html_shows_exit_code():
    r = ShellResult(stdout="hi", stderr="", exit_code=0)
    assert "0" in r._repr_html_()
    assert "hi" in r._repr_html_()
    r2 = ShellResult(stdout="", stderr="boom", exit_code=1)
    assert "boom" in r2._repr_html_()


def test_tool_error_repr_html():
    e = ToolError(tool="shell", env="alpha", message="bad", raw={"isError": True})
    html = e._repr_html_()
    assert "alpha" in html
    assert "shell" in html
    assert "bad" in html
```

- [ ] **Step 2: Run, confirm failure**

Run: `pytest tests/test_repr.py -v`
Expected: FAIL — modules/methods not found.

- [ ] **Step 3: Implement `_repr.py`**

Create `sdk/python/src/agentserver_sdk/_repr.py`:

```python
"""Jupyter-friendly HTML renderers. Pure functions, no I/O."""
from __future__ import annotations

import html
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from .env import Env


def envs_table_html(envs: list["Env"]) -> str:
    rows = [
        "<tr><th align='left'>name</th><th align='left'>type</th>"
        "<th align='left'>tools</th></tr>"
    ]
    for e in envs:
        rows.append(
            "<tr>"
            f"<td><code>{html.escape(e.name)}</code></td>"
            f"<td>{html.escape(e.type)}</td>"
            f"<td>{len(e.tools)}</td>"
            "</tr>"
        )
    return f"<table>{''.join(rows)}</table>"
```

- [ ] **Step 4: Add `_repr_html_` to Env, ShellResult, ToolError**

Append to `Env` in `sdk/python/src/agentserver_sdk/env.py`:

```python
    def _repr_html_(self) -> str:
        import html as _html
        return (
            f"<table>"
            f"<tr><th>env</th><td><code>{_html.escape(self.name)}</code></td></tr>"
            f"<tr><th>type</th><td>{_html.escape(self.type)}</td></tr>"
            f"<tr><th>tools</th><td>{len(self.tools)}</td></tr>"
            f"</table>"
        )
```

Append to `ShellResult` in `sdk/python/src/agentserver_sdk/types.py`:

```python
    def _repr_html_(self) -> str:
        import html as _html
        colour = "green" if self.exit_code == 0 else "red"
        return (
            f"<div>exit_code: <b style='color:{colour}'>{self.exit_code}</b></div>"
            f"<details open><summary>stdout</summary>"
            f"<pre>{_html.escape(self.stdout)}</pre></details>"
            + (f"<details><summary>stderr</summary><pre>{_html.escape(self.stderr)}</pre></details>"
               if self.stderr else "")
        )
```

Append to `ToolError` in `sdk/python/src/agentserver_sdk/errors.py`:

```python
    def _repr_html_(self) -> str:
        import html as _html
        return (
            f"<div style='border-left:3px solid red;padding-left:8px'>"
            f"<b>ToolError</b> on env <code>{_html.escape(self.env or '?')}</code>, "
            f"tool <code>{_html.escape(self.tool)}</code><br>"
            f"<pre>{_html.escape(self.message)}</pre>"
            f"</div>"
        )
```

- [ ] **Step 5: Run tests, confirm pass**

Run: `pytest tests/test_repr.py -v`
Expected: `4 passed`.

- [ ] **Step 6: Commit**

```bash
git add sdk/python/src/agentserver_sdk/_repr.py \
        sdk/python/src/agentserver_sdk/env.py \
        sdk/python/src/agentserver_sdk/types.py \
        sdk/python/src/agentserver_sdk/errors.py \
        sdk/python/tests/test_repr.py
git commit -m "feat(sdk): jupyter _repr_html_ for Env / ShellResult / ToolError

table + colour-coded exit code + error styling."
```

---

## Task 11: Full lint + test pass

**Files:** none new.

- [ ] **Step 1: Add ruff config + run**

Append to `sdk/python/pyproject.toml`:

```toml
[tool.ruff]
line-length = 100
target-version = "py312"

[tool.ruff.lint]
select = ["E", "F", "I", "B", "UP", "SIM"]
ignore = ["E501"]  # let the formatter handle long lines
```

Run:
```bash
cd sdk/python
ruff check .
ruff format --check .
```

Fix any issues raised (formatting only — substance has tests).

- [ ] **Step 2: Run full test suite**

Run: `pytest -v`
Expected: All tests pass; no test errors or warnings about the asyncio fixtures.

- [ ] **Step 3: Commit any lint fixups**

```bash
git add -A
git commit -m "chore(sdk): ruff config + lint pass" || true  # skip if no diff
```

---

## Task 12: `Dockerfile.notebook` + IPython startup + Jupyter config

**Files:**
- Create: `Dockerfile.notebook` (repo root)
- Create: `notebook/ipython_startup/00-ctx.py`
- Create: `notebook/jupyter_server_config.py`
- Create: `notebook/README.md`

- [ ] **Step 1: Create the startup file**

Create `notebook/ipython_startup/00-ctx.py`:

```python
"""Auto-injected into every ipykernel session inside the notebook image.

Exposes:
  - ctx: lazy Ctx instance bound to AGENTSERVER_* env vars
  - asyncio: convenience re-import so users don't need `import asyncio`
"""
import asyncio  # noqa: F401  (intentionally injected into kernel namespace)
from agentserver_sdk import Ctx

ctx = Ctx.from_env()
```

- [ ] **Step 2: Create jupyter server config (v1 minimal)**

Create `notebook/jupyter_server_config.py`:

```python
"""Plan 1 minimum config — no IdentityProvider or KernelProvisioner yet
(those land with notebook hosting plan). Disable jupyter's own token
auth so the smoke run is just-open-and-go. Real deployment will plug
in agentserver auth in Plan 3."""

c = get_config()  # type: ignore[name-defined]  # noqa: F821 (provided by jupyter at runtime)

c.ServerApp.ip = "0.0.0.0"
c.ServerApp.port = 8888
c.ServerApp.open_browser = False
c.ServerApp.token = ""       # SECURITY: only safe inside the local smoke env / agentserver-proxied prod
c.ServerApp.password = ""
c.ServerApp.disable_check_xsrf = True
c.ServerApp.allow_origin = "*"
c.ServerApp.root_dir = "/workspace"
c.ServerApp.allow_root = True
```

- [ ] **Step 3: Write Dockerfile**

Create `Dockerfile.notebook`:

```dockerfile
# syntax=docker/dockerfile:1
FROM python:3.12-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1

# System deps kept tiny — jupyter is pure-python.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tini \
 && rm -rf /var/lib/apt/lists/*

# Install jupyter + ipykernel
RUN pip install --no-cache-dir \
      jupyter-server~=2.14 \
      jupyterlab~=4.2 \
      ipykernel~=6.29

# Install agentserver-sdk from local source.
COPY sdk/python /tmp/sdk
RUN pip install --no-cache-dir /tmp/sdk && rm -rf /tmp/sdk

# IPython startup hook → injects `ctx`
RUN mkdir -p /etc/ipython/profile_default/startup
COPY notebook/ipython_startup/00-ctx.py /etc/ipython/profile_default/startup/

# Jupyter config
COPY notebook/jupyter_server_config.py /etc/jupyter/

# Workspace mount point — host bind or k8s PV later.
RUN mkdir -p /workspace
WORKDIR /workspace

ENV IPYTHONDIR=/etc/ipython \
    JUPYTER_CONFIG_DIR=/etc/jupyter

EXPOSE 8888
ENTRYPOINT ["tini", "--", "jupyter", "server", "--config=/etc/jupyter/jupyter_server_config.py"]
```

- [ ] **Step 4: Write notebook/README.md**

Create `notebook/README.md`:

```markdown
# Notebook image (Plan 1)

`Dockerfile.notebook` at repo root builds a self-contained jupyter image
with `agentserver_sdk` pre-installed. `ctx` is auto-injected into every
kernel via `ipython_startup/00-ctx.py`.

## Build

```bash
docker build -f Dockerfile.notebook -t agentserver-notebook:dev .
```

## Run (against a stub gateway)

See `docker-compose.smoke.yml`:

```bash
docker compose -f notebook/docker-compose.smoke.yml up --build
```

Then open <http://localhost:8888/lab>. In a new notebook:

```python
envs = await ctx.envs()
envs   # rendered as table thanks to _repr_html_
```

## Env vars consumed by `ctx = Ctx.from_env()`

| var | default | purpose |
|---|---|---|
| `AGENTSERVER_GATEWAY_URL` | `ws://localhost:8086/notebook/ws` | WS endpoint |
| `AGENTSERVER_WORKSPACE_TOKEN` | (empty) | Bearer for gateway |
| `AGENTSERVER_WORKSPACE_ID` | (empty) | workspace key |
| `AGENTSERVER_USER_ID` | (empty) | attribution only |
```

- [ ] **Step 5: Build the image**

Run from repo root:
```bash
docker build -f Dockerfile.notebook -t agentserver-notebook:dev .
```

Expected: build succeeds; final image ~250 MB.

- [ ] **Step 6: Smoke test — bare `import` works**

Run:
```bash
docker run --rm agentserver-notebook:dev \
  python -c "from agentserver_sdk import Ctx; print(Ctx.from_env().gateway_url)"
```

Expected: prints `ws://localhost:8086/notebook/ws`.

- [ ] **Step 7: Commit**

```bash
git add Dockerfile.notebook notebook/
git commit -m "feat(notebook): jupyter image with agentserver_sdk pre-installed

- Dockerfile.notebook at repo root following project convention
- ipython startup injects ctx
- jupyter config tuned for proxy use (no token, root_dir=/workspace)
- README documents env vars + smoke run"
```

---

## Task 13: docker-compose smoke (jupyter + stub gateway)

**Files:**
- Create: `notebook/docker-compose.smoke.yml`
- Create: `notebook/stub_gateway_runner.py`

- [ ] **Step 1: Create the stand-alone stub runner**

The pytest `StubGateway` class is import-only. For docker we need a runnable script. Create `notebook/stub_gateway_runner.py`:

```python
"""Run the test stub gateway as a long-lived process for local smoke.

Listens on 0.0.0.0:8086, path `/notebook/ws`. Responds to:
  - initialize / initialized (no-op)
  - thread/start → fake thread id
  - envs/list → two fake envs
  - env/capabilities → tools per env
  - mcpServer/tool/call → for `shell`, returns the command back as stdout
  - operations/list → empty
"""
from __future__ import annotations

import asyncio
import json
import sys
import os

import websockets

# Reuse the test class — the file lives in sdk/python/tests but is
# import-safe (no pytest imports at module scope).
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "sdk", "python", "tests"))
from stub_gateway import StubGateway  # noqa: E402


async def main() -> None:
    g = StubGateway()

    g.on("envs/list", lambda p: {"envs": [
        {"name": "alpha", "type": "shell"},
        {"name": "hpc",   "type": "hpc"},
    ]})

    def caps(p):
        env_id = p["env_id"]
        if env_id == "alpha":
            return {"tools": [
                {"name": "shell",     "description": "run sh",   "inputSchema": {}},
                {"name": "read_file", "description": "read",     "inputSchema": {}},
            ]}
        return {"tools": [
            {"name": "shell",        "description": "run sh",        "inputSchema": {}},
            {"name": "submit_task",  "description": "submit HPC",    "inputSchema": {}},
        ]}
    g.on("env/capabilities", caps)

    def tool_call(p):
        if p["tool"] == "shell":
            cmd = p["arguments"]["command"]
            return {
                "content": [{"type": "text", "text": cmd}],
                "structuredContent": {"stdout": f"stub ran: {cmd}", "stderr": "", "exit_code": 0},
                "isError": False,
            }
        return {"content": [{"type": "text", "text": f"stub: {p['tool']}"}], "isError": False}
    g.on("mcpServer/tool/call", tool_call)

    g.on("operations/list", lambda p: {"operations": []})

    # StubGateway uses port=0 by default — override to fixed 8086 + path.
    async def handle(ws):
        async for raw in ws:
            msg = json.loads(raw)
            g.received.append(msg)
            mid = msg.get("id")
            method = msg.get("method")
            if mid is None:
                continue
            h = g._handlers.get(method)
            if h is None:
                resp = {"jsonrpc": "2.0", "id": mid,
                        "error": {"code": -32601, "message": f"Method not found: {method}"}}
            else:
                out = h(msg.get("params", {}) or {})
                if isinstance(out, dict) and "error" in out and "code" in out["error"]:
                    resp = {"jsonrpc": "2.0", "id": mid, "error": out["error"]}
                else:
                    resp = {"jsonrpc": "2.0", "id": mid, "result": out}
            await ws.send(json.dumps(resp))

    server = await websockets.serve(handle, "0.0.0.0", 8086)
    print(f"stub-gateway listening on ws://0.0.0.0:8086", flush=True)
    await server.wait_closed()


if __name__ == "__main__":
    asyncio.run(main())
```

- [ ] **Step 2: Create docker-compose**

Create `notebook/docker-compose.smoke.yml`:

```yaml
# Local smoke test: jupyter + stub gateway, no agentserver backend.
# Run:  docker compose -f notebook/docker-compose.smoke.yml up --build
# Open: http://localhost:8888/lab
services:
  stub-gateway:
    image: python:3.12-slim
    working_dir: /app
    volumes:
      - ../sdk/python/tests:/app/sdk-tests:ro
      - ./stub_gateway_runner.py:/app/runner.py:ro
    command: >
      sh -c "pip install --quiet websockets~=14.0 &&
             python /app/runner.py"
    ports:
      - "8086:8086"

  notebook:
    build:
      context: ..
      dockerfile: Dockerfile.notebook
    depends_on:
      - stub-gateway
    environment:
      AGENTSERVER_GATEWAY_URL: ws://stub-gateway:8086
      AGENTSERVER_WORKSPACE_TOKEN: smoke-token
      AGENTSERVER_WORKSPACE_ID: smoke-ws
      AGENTSERVER_USER_ID: smoke-user
    volumes:
      - ./smoke-workspace:/workspace
    ports:
      - "8888:8888"
```

- [ ] **Step 3: Bring up the stack**

Run:
```bash
mkdir -p notebook/smoke-workspace
docker compose -f notebook/docker-compose.smoke.yml up --build
```

Expected: both services come up; jupyter logs show it listening on 0.0.0.0:8888; stub logs show it listening on 8086.

- [ ] **Step 4: Manual notebook validation**

In a separate terminal:
1. Open <http://localhost:8888/lab>
2. New Python 3 notebook
3. Cell 1: `envs = await ctx.envs(); envs`
   - Expect: table with `alpha` and `hpc` rows
4. Cell 2: `alpha = await ctx.env("alpha"); r = await alpha.shell("hi"); r`
   - Expect: rendered ShellResult with green exit_code=0 and "stub ran: hi" stdout
5. Cell 3: `hpc = await ctx.env("hpc"); await hpc.submit_task(script="x")`
   - Expect: a dict with `content[0].text == "stub: submit_task"`
6. Cell 4: `ops = await ctx.history(limit=5); ops`
   - Expect: `[]` (stub returns empty)

Tear down: `Ctrl-C` then `docker compose -f notebook/docker-compose.smoke.yml down -v`.

- [ ] **Step 5: Document the smoke flow**

Append to `notebook/README.md`:

```markdown
## Smoke walkthrough (4 cells)

Once `docker compose -f notebook/docker-compose.smoke.yml up --build` is running and you've opened <http://localhost:8888/lab>:

```python
# Cell 1 — list envs
envs = await ctx.envs()
envs

# Cell 2 — typed shell
alpha = await ctx.env("alpha")
await alpha.shell("hi")

# Cell 3 — dynamic dispatch (custom HPC tool)
hpc = await ctx.env("hpc")
await hpc.submit_task(script="x")

# Cell 4 — operations history (returns [] until Plan 2 lands)
await ctx.history(limit=5)
```
```

- [ ] **Step 6: Commit**

```bash
git add notebook/docker-compose.smoke.yml \
        notebook/stub_gateway_runner.py \
        notebook/README.md
git commit -m "feat(notebook): docker-compose smoke (jupyter + stub gateway)

local end-to-end without agentserver backend: stub returns canned
responses for envs/list, env/capabilities, mcpServer/tool/call,
operations/list. README has 4-cell walkthrough."
```

---

## Task 14: Makefile targets

**Files:**
- Modify: `Makefile`

- [ ] **Step 1: Add targets**

Append to `Makefile`:

```makefile
sdk-test:
	cd sdk/python && pytest -v

sdk-lint:
	cd sdk/python && ruff check . && ruff format --check .

notebook-image:
	docker build -f Dockerfile.notebook -t agentserver-notebook:dev .

notebook-smoke: notebook-image
	mkdir -p notebook/smoke-workspace
	docker compose -f notebook/docker-compose.smoke.yml up --build
```

- [ ] **Step 2: Verify**

Run: `make sdk-test`
Expected: all tests pass.

Run: `make sdk-lint`
Expected: no lint errors.

Run: `make notebook-image`
Expected: image build succeeds.

- [ ] **Step 3: Commit**

```bash
git add Makefile
git commit -m "build: makefile targets for sdk-test / sdk-lint / notebook-image / notebook-smoke"
```

---

## Self-review checklist (for the implementer)

After all tasks done:

- [ ] All spec § "Components" items 1, 2 covered (SDK, Dockerfile, ipython startup); 3-7 explicitly deferred to Plan 2/3
- [ ] All spec § "SDK shape" core methods present: `shell`, `read_file`, `write_file`, `apply_patch`, `spawn` (+ `Process`), `ctx.copy`, `ctx.history`, custom tools via `__getattr__`
- [ ] `_repr_html_` on Env / ShellResult / ToolError + envs_table_html helper
- [ ] No TODO/TBD/FIXME left in code
- [ ] Every PR-equivalent commit is green on its own (run `pytest -v` after each commit)
- [ ] Smoke flow in `notebook/README.md` actually works on a clean machine

## After this plan

When Plan 1 is merged:
- Plan 2 (operation log) can start in parallel; it shouldn't require SDK changes beyond pointing `ctx.history()` at the real RPC (already done)
- Plan 3 (notebook supervisor + web UI panel) can begin; SDK changes needed:
  - Refresh-token flow for `AGENTSERVER_WORKSPACE_TOKEN`
  - JWT minting integration
  - All confined to `client.py`
- env-mcp per-env capability spec: until it lands, real envs return the same static tool list for all envs, custom tools like `submit_task` would have to be reached via `env.call("submit_task", {...})`. This is acceptable degradation.
