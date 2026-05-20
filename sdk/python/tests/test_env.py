import base64

import pytest

from agentserver_sdk.env import Env
from agentserver_sdk.errors import ToolError
from agentserver_sdk.types import ShellResult, ToolMetadata


def _tool(name, desc=""):
    from agentserver_sdk.types import CORE_TOOLS

    return ToolMetadata(
        name=name,
        description=desc,
        input_schema={},
        kind="core" if name in CORE_TOOLS else "custom",
    )


def make_env(client, name: str = "alpha", tools=None) -> Env:
    return Env(name=name, type="shell", tools=tools or [], _client=client)


async def test_call_injects_environment_id(stub_client):
    client, stub = stub_client

    async def tool(body, query):
        assert body["arguments"]["environment_id"] == "alpha"
        assert body["arguments"]["command"] == ["ls"]
        assert body["tool"] == "shell"
        return 200, {"isError": False, "content": [], "structuredContent": {}}

    stub.register("POST", "/api/sdk/envs/alpha/tool/call", tool)
    env = make_env(client, tools=[_tool("shell")])
    await env.call("shell", {"command": ["ls"]})


async def test_shell_str_wraps_as_single_argv(stub_client):
    """A bare-string command must be sent as ["cmd"] — not the string itself —
    so the server's argv contract holds. Multi-token strings are the caller's
    responsibility to wrap with sh -c / cmd /c."""
    client, stub = stub_client
    received = {}

    async def tool(body, query):
        received.update(body)
        return 200, {
            "structuredContent": {"stdout": "out", "stderr": "", "exit_code": 0},
            "content": [{"type": "text", "text": "out"}],
            "isError": False,
        }

    stub.register("POST", "/api/sdk/envs/alpha/tool/call", tool)
    env = make_env(client, tools=[_tool("shell")])
    await env.shell("hostname")
    assert received["arguments"]["command"] == ["hostname"]


async def test_shell_list_passed_through_and_timeout_in_ms(stub_client):
    client, stub = stub_client
    received = {}

    async def tool(body, query):
        received.update(body)
        return 200, {
            "structuredContent": {"stdout": "", "stderr": "", "exit_code": 0},
            "content": [{"type": "text", "text": ""}],
            "isError": False,
        }

    stub.register("POST", "/api/sdk/envs/alpha/tool/call", tool)
    env = make_env(client, tools=[_tool("shell")])
    await env.shell(["sh", "-c", "ls | wc -l"], timeout=2.5, cwd="/work")
    args = received["arguments"]
    assert args["command"] == ["sh", "-c", "ls | wc -l"]
    assert args["timeout_ms"] == 2500
    assert args["cwd"] == "/work"


async def test_shell_non_zero_exit_does_not_raise(stub_client):
    """Server contract change in 0.61.5: non-zero exit ships in
    ShellResult.exit_code with isError=False. Only failure-to-start /
    timeout-without-exit get isError=True."""
    client, stub = stub_client

    async def tool(body, query):
        return 200, {
            "structuredContent": {"stdout": "", "stderr": "no match", "exit_code": 1},
            "content": [{"type": "text", "text": "no match\n[exit_code=1]"}],
            "isError": False,
        }

    stub.register("POST", "/api/sdk/envs/alpha/tool/call", tool)
    env = make_env(client, tools=[_tool("shell")])
    r = await env.shell(["grep", "x", "/etc/hosts"])
    assert r.exit_code == 1
    assert r.stderr == "no match"


async def test_shell_returns_shell_result(stub_client):
    client, stub = stub_client

    async def tool(body, query):
        return 200, {
            "content": [{"type": "text", "text": "hi"}],
            "structuredContent": {"stdout": "hi", "stderr": "", "exit_code": 0},
            "isError": False,
        }

    stub.register("POST", "/api/sdk/envs/alpha/tool/call", tool)
    env = make_env(client, tools=[_tool("shell")])
    r = await env.shell("echo hi")
    assert isinstance(r, ShellResult)
    assert r.stdout == "hi"
    assert r.exit_code == 0


async def test_read_file_returns_bytes(stub_client):
    client, stub = stub_client
    payload = b"hello binary"

    async def tool(body, query):
        return 200, {
            "content": [{"type": "text", "text": base64.b64encode(payload).decode()}],
            "structuredContent": {"encoding": "base64"},
            "isError": False,
        }

    stub.register("POST", "/api/sdk/envs/alpha/tool/call", tool)
    env = make_env(client, tools=[_tool("read_file")])
    data = await env.read_file("/x")
    assert data == payload


async def test_is_error_raises_tool_error(stub_client):
    client, stub = stub_client

    async def tool(body, query):
        return 200, {"isError": True, "content": [{"type": "text", "text": "boom"}]}

    stub.register("POST", "/api/sdk/envs/alpha/tool/call", tool)
    env = make_env(client, tools=[_tool("shell")])
    with pytest.raises(ToolError) as ei:
        await env.shell("badcmd")
    assert ei.value.env == "alpha"
    assert ei.value.tool == "shell"
    assert "boom" in ei.value.message


async def test_write_file_passes_bytes_as_b64(stub_client):
    client, stub = stub_client
    received = {}

    async def tool(body, query):
        received.update(body)
        return 200, {"isError": False, "content": []}

    stub.register("POST", "/api/sdk/envs/alpha/tool/call", tool)
    env = make_env(client, tools=[_tool("write_file")])
    await env.write_file("/x", b"\x00\x01\x02")
    args = received["arguments"]
    assert args["path"] == "/x"
    assert base64.b64decode(args["content_b64"]) == b"\x00\x01\x02"


async def test_apply_patch_passes_through(stub_client):
    client, stub = stub_client
    received = {}

    async def tool(body, query):
        received.update(body)
        return 200, {"isError": False, "content": []}

    stub.register("POST", "/api/sdk/envs/alpha/tool/call", tool)
    env = make_env(client, tools=[_tool("apply_patch")])
    await env.apply_patch("*** Patch...")
    assert received["arguments"]["patch"] == "*** Patch..."


async def test_custom_tool_called_via_attribute(stub_client):
    """A custom tool surfaced in tools metadata becomes a method on env."""
    client, stub = stub_client

    async def tool(body, query):
        return 200, {
            "content": [{"type": "text", "text": "job-123"}],
            "isError": False,
        }

    stub.register("POST", "/api/sdk/envs/hpc-a/tool/call", tool)
    custom = ToolMetadata(
        name="submit_task",
        description="submit HPC job",
        input_schema={"type": "object"},
        kind="custom",
    )
    env = Env(name="hpc-a", type="hpc", tools=[custom], _client=client)
    assert hasattr(env, "submit_task")
    result = await env.submit_task(script="x", resources={"gpus": 4})
    call = stub.calls[0]
    assert call[2]["tool"] == "submit_task"
    assert call[2]["arguments"]["environment_id"] == "hpc-a"
    assert call[2]["arguments"]["script"] == "x"
    assert call[2]["arguments"]["resources"] == {"gpus": 4}
    assert result["content"][0]["text"] == "job-123"


async def test_unknown_attribute_raises_attribute_error(stub_client):
    client, stub = stub_client
    env = make_env(client, tools=[_tool("shell")])
    with pytest.raises(AttributeError):
        _ = env.nonexistent_tool


async def test_dir_lists_custom_tools(stub_client):
    client, stub = stub_client
    env = Env(
        name="hpc",
        type="hpc",
        tools=[ToolMetadata(name="submit_task", description="", input_schema={}, kind="custom")],
        _client=client,
    )
    assert "submit_task" in dir(env)
