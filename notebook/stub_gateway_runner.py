"""Run the test stub gateway as a long-lived process for local smoke.

Listens on 0.0.0.0:8086. Responds to:
  - initialize / initialized (no-op)
  - thread/start -> fake thread id
  - envs/list -> two fake envs
  - env/capabilities -> tools per env
  - mcpServer/tool/call -> for `shell`, returns the command back as stdout
  - operations/list -> 2 synthetic records (smoke walkthrough Cell 4)
"""
from __future__ import annotations

import asyncio
import datetime
import json
import os
import sys
import uuid

import websockets

# The compose file mounts sdk/python/tests/stub_gateway.py next to this
# runner at /app, so /app is on sys.path already (cwd). Keep the explicit
# insert so the script also works when invoked from elsewhere.
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
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
                {"name": "shell",     "description": "run sh", "inputSchema": {}},
                {"name": "read_file", "description": "read",   "inputSchema": {}},
            ]}
        return {"tools": [
            {"name": "shell",       "description": "run sh",     "inputSchema": {}},
            {"name": "submit_task", "description": "submit HPC", "inputSchema": {}},
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

    def operations_list(p):
        now = datetime.datetime.now(datetime.timezone.utc).isoformat()
        return {"operations": [
            {"id": str(uuid.uuid4()), "env_id": "alpha", "tool": "shell",
             "is_error": False, "started_at": now, "duration_ms": 7,
             "source": "sdk", "user_id": "smoke-user"},
            {"id": str(uuid.uuid4()), "env_id": "hpc", "tool": "submit_task",
             "is_error": False, "started_at": now, "duration_ms": 320,
             "source": "sdk", "user_id": "smoke-user"},
        ]}
    g.on("operations/list", operations_list)

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
    print("stub-gateway listening on ws://0.0.0.0:8086", flush=True)
    await server.wait_closed()


if __name__ == "__main__":
    asyncio.run(main())
