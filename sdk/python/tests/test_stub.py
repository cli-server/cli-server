import json

import websockets


async def test_stub_responds_to_initialize(stub):
    """Stub answers initialize with a default reply and records the frame."""
    async with websockets.connect(stub.url) as ws:
        await ws.send(
            json.dumps(
                {
                    "jsonrpc": "2.0",
                    "id": 1,
                    "method": "initialize",
                    "params": {
                        "clientInfo": {"name": "t", "title": "t", "version": "0"},
                        "capabilities": {},
                    },
                }
            )
        )
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
