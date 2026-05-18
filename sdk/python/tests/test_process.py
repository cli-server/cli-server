import pytest

from agentserver_sdk.client import WSClient
from agentserver_sdk.env import Env
from agentserver_sdk.types import ToolMetadata


def _tool(name):
    return ToolMetadata(name=name, description="", input_schema={}, kind="core")


async def test_spawn_calls_exec_command_and_returns_process(stub):
    stub.on(
        "mcpServer/tool/call",
        lambda p: {
            "content": [],
            "structuredContent": {"session_id": "sess-1"},
            "isError": False,
        },
    )
    c = WSClient(stub.url, token="t", workspace_id="w", user_id="u")
    await c.connect()
    env = Env(
        name="alpha",
        type="shell",
        tools=[
            _tool("exec_command"),
            _tool("write_stdin"),
            _tool("read_output"),
            _tool("terminate"),
        ],
        _client=c,
    )
    try:
        async with env.spawn("./run.sh") as proc:
            assert proc.session_id == "sess-1"
            # exec_command was sent
            calls = [m for m in stub.received if m.get("method") == "mcpServer/tool/call"]
            assert calls[0]["params"]["tool"] == "exec_command"
            assert calls[0]["params"]["arguments"]["command"] == "./run.sh"
        # On exit, terminate was sent
        all_tools = [
            m["params"]["tool"] for m in stub.received if m.get("method") == "mcpServer/tool/call"
        ]
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
    env = Env(
        name="alpha",
        type="shell",
        tools=[
            _tool("exec_command"),
            _tool("write_stdin"),
            _tool("read_output"),
            _tool("terminate"),
        ],
        _client=c,
    )
    try:
        async with env.spawn("./run.sh") as proc:
            await proc.write_stdin(b"hi\n")
            data = await proc.read_output(timeout=1.0)
            assert data == b"hello"
    finally:
        await c.close()


async def test_process_terminate_runs_even_on_exception(stub):
    stub.on(
        "mcpServer/tool/call",
        lambda p: (
            {"structuredContent": {"session_id": "s"}, "isError": False, "content": []}
            if p["tool"] == "exec_command"
            else {"content": [], "isError": False}
        ),
    )
    c = WSClient(stub.url, token="t", workspace_id="w", user_id="u")
    await c.connect()
    env = Env(
        name="alpha",
        type="shell",
        tools=[
            _tool("exec_command"),
            _tool("terminate"),
            _tool("write_stdin"),
            _tool("read_output"),
        ],
        _client=c,
    )
    try:
        with pytest.raises(RuntimeError):
            async with env.spawn("./run.sh"):
                raise RuntimeError("boom")
        # terminate happened
        tools = [
            m["params"]["tool"] for m in stub.received if m.get("method") == "mcpServer/tool/call"
        ]
        assert tools.count("terminate") == 1
    finally:
        await c.close()
