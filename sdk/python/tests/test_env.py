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
    return ToolMetadata(
        name=name,
        description=desc,
        input_schema={},
        kind="core"
        if name
        in {
            "shell",
            "read_file",
            "write_file",
            "apply_patch",
            "exec_command",
            "write_stdin",
            "read_output",
            "terminate",
            "copy_path",
        }
        else "custom",
    )


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
    stub.on(
        "mcpServer/tool/call",
        lambda p: {
            "content": [{"type": "text", "text": "hi"}],
            "structuredContent": {"stdout": "hi", "stderr": "", "exit_code": 0},
            "isError": False,
        },
    )
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
    stub.on(
        "mcpServer/tool/call",
        lambda p: {
            "content": [{"type": "text", "text": base64.b64encode(payload).decode()}],
            "structuredContent": {"encoding": "base64"},
            "isError": False,
        },
    )
    c = await _connected_client(stub)
    env = Env(name="alpha", type="shell", tools=[_tool("read_file")], _client=c)
    try:
        data = await env.read_file("/x")
        assert data == payload
    finally:
        await c.close()


async def test_env_is_error_raises_tool_error(stub):
    stub.on(
        "mcpServer/tool/call",
        lambda p: {
            "content": [{"type": "text", "text": "boom"}],
            "isError": True,
        },
    )
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


async def test_custom_tool_called_via_attribute(stub):
    """A custom tool surfaced in tools metadata becomes a method on env."""
    stub.on(
        "mcpServer/tool/call",
        lambda p: {
            "content": [{"type": "text", "text": "job-123"}],
            "isError": False,
        },
    )
    c = await _connected_client(stub)
    custom = ToolMetadata(
        name="submit_task",
        description="submit HPC job",
        input_schema={"type": "object"},
        kind="custom",
    )
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
            _ = env.nonexistent_tool
    finally:
        await c.close()


async def test_dir_lists_custom_tools(stub):
    c = await _connected_client(stub)
    env = Env(
        name="hpc",
        type="hpc",
        tools=[ToolMetadata(name="submit_task", description="", input_schema={}, kind="custom")],
        _client=c,
    )
    try:
        assert "submit_task" in dir(env)
    finally:
        await c.close()
