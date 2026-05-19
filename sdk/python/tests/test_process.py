import base64

import pytest

from agentserver_sdk.env import Env
from agentserver_sdk.errors import ToolError
from agentserver_sdk.types import ToolMetadata


def _tool(name):
    return ToolMetadata(name=name, description="", input_schema={}, kind="core")


def make_env(client, name: str = "my-mac") -> Env:
    return Env(
        name=name,
        type="executor",
        tools=[
            _tool("exec_command"),
            _tool("write_stdin"),
            _tool("read_output"),
            _tool("terminate"),
        ],
        _client=client,
    )


async def test_spawn_calls_exec_command_and_sets_session_id(stub_client):
    client, stub = stub_client
    terminated = []

    async def exec_cmd(body, query):
        return 200, {
            "isError": False,
            "structuredContent": {"session_id": "sid-1"},
            "content": [{"type": "text", "text": "started"}],
        }

    async def terminate(body, query):
        terminated.append(True)
        return 200, {"ok": True}

    stub.register("POST", "/api/sdk/envs/my-mac/tool/call", exec_cmd)
    stub.register("POST", "/api/sdk/processes/sid-1/terminate", terminate)

    env = make_env(client)
    async with env.spawn("./run.sh") as proc:
        assert proc.session_id == "sid-1"
        call = stub.calls[0]
        assert call[2]["tool"] == "exec_command"
        assert call[2]["arguments"]["command"] == "./run.sh"
    assert terminated == [True]


async def test_process_write_stdin(stub_client):
    client, stub = stub_client
    stdin_payload: list[str] = []

    async def exec_cmd(body, query):
        return 200, {
            "isError": False,
            "structuredContent": {"session_id": "s-1"},
            "content": [],
        }

    async def stdin(body, query):
        stdin_payload.append(body["data_b64"])
        return 200, {"ok": True}

    async def terminate(body, query):
        return 200, {"ok": True}

    stub.register("POST", "/api/sdk/envs/my-mac/tool/call", exec_cmd)
    stub.register("POST", "/api/sdk/processes/s-1/stdin", stdin)
    stub.register("POST", "/api/sdk/processes/s-1/terminate", terminate)

    env = make_env(client)
    async with env.spawn("./run.sh") as proc:
        await proc.write_stdin(b"hello")
        assert base64.b64decode(stdin_payload[0]) == b"hello"


async def test_process_read_output(stub_client):
    client, stub = stub_client

    async def exec_cmd(body, query):
        return 200, {
            "isError": False,
            "structuredContent": {"session_id": "s-1"},
            "content": [],
        }

    async def output(body, query):
        return 200, {
            "chunks": [
                {"stream": "stdout", "data_b64": base64.b64encode(b"hi").decode(), "seq": 1}
            ],
            "exit_code": None,
            "session_alive": True,
            "truncated": False,
            "lost_bytes": 0,
        }

    async def terminate(body, query):
        return 200, {"ok": True}

    stub.register("POST", "/api/sdk/envs/my-mac/tool/call", exec_cmd)
    stub.register("GET", "/api/sdk/processes/s-1/output", output)
    stub.register("POST", "/api/sdk/processes/s-1/terminate", terminate)

    env = make_env(client)
    async with env.spawn("./run.sh") as proc:
        out = await proc.read_output()
        assert out["chunks"][0]["seq"] == 1


async def test_process_terminate_runs_on_exception(stub_client):
    client, stub = stub_client
    terminated = []

    async def exec_cmd(body, query):
        return 200, {
            "isError": False,
            "structuredContent": {"session_id": "s"},
            "content": [],
        }

    async def terminate(body, query):
        terminated.append(True)
        return 200, {"ok": True}

    stub.register("POST", "/api/sdk/envs/my-mac/tool/call", exec_cmd)
    stub.register("POST", "/api/sdk/processes/s/terminate", terminate)

    env = make_env(client)
    with pytest.raises(RuntimeError):
        async with env.spawn("./run.sh"):
            raise RuntimeError("boom")
    assert terminated == [True]


async def test_process_lifecycle_full(stub_client):
    """Full lifecycle: exec → stdin → output → terminate (implicit on exit)."""
    client, stub = stub_client
    stdin_payload: list[str] = []
    terminated: list[bool] = []

    async def exec_cmd(body, query):
        return 200, {
            "isError": False,
            "structuredContent": {"session_id": "sid-1"},
            "content": [{"type": "text", "text": "started"}],
        }

    async def stdin(body, query):
        stdin_payload.append(body["data_b64"])
        return 200, {"ok": True}

    async def output(body, query):
        return 200, {
            "chunks": [
                {"stream": "stdout", "data_b64": base64.b64encode(b"hi").decode(), "seq": 1}
            ],
            "exit_code": None,
            "session_alive": True,
            "truncated": False,
            "lost_bytes": 0,
        }

    async def terminate(body, query):
        terminated.append(True)
        return 200, {"ok": True}

    stub.register("POST", "/api/sdk/envs/my-mac/tool/call", exec_cmd)
    stub.register("POST", "/api/sdk/processes/sid-1/stdin", stdin)
    stub.register("GET", "/api/sdk/processes/sid-1/output", output)
    stub.register("POST", "/api/sdk/processes/sid-1/terminate", terminate)

    env = make_env(client)
    async with env.spawn("some_cmd") as proc:
        assert proc.session_id == "sid-1"
        await proc.write_stdin(b"hello")
        assert base64.b64decode(stdin_payload[0]) == b"hello"
        out = await proc.read_output()
        assert out["chunks"][0]["seq"] == 1
    assert terminated == [True]
