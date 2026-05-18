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
