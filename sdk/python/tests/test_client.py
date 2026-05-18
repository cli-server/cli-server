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


async def test_connect_cleans_up_on_handshake_failure(stub):
    """If thread/start errors, ws + reader task must be cleaned up."""
    stub.on("thread/start", lambda p: {"error": {"code": -32603, "message": "boom"}})
    c = WSClient(stub.url, token="t", workspace_id="ws", user_id="u")
    with pytest.raises(SdkConnectionError):
        await c.connect()
    # ws closed, no reader task left running, not "connected"
    assert c._ws is None
    assert c._reader_task is None
    assert c.thread_id is None
    assert not c.is_connected


async def test_connect_missing_thread_id_raises_sdk_error(stub):
    """thread/start returning a result without thread_id should raise SdkConnectionError, not KeyError."""
    stub.on("thread/start", lambda p: {})  # success, but no thread_id field
    c = WSClient(stub.url, token="t", workspace_id="ws", user_id="u")
    with pytest.raises(SdkConnectionError, match="missing thread_id"):
        await c.connect()
    assert c._ws is None
    assert not c.is_connected


async def test_mcp_tool_call_round_trip(stub):
    stub.on(
        "mcpServer/tool/call",
        lambda p: {
            "content": [{"type": "text", "text": "hello"}],
            "structuredContent": {"k": "v"},
            "isError": False,
        },
    )
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
