import pytest

from agentserver_sdk.errors import SdkConnectionError, SdkUnauthorized


async def test_post_decode_ok(stub_client):
    client, stub = stub_client

    async def echo(body, query):
        return 200, {"echoed": body}

    stub.register("POST", "/echo", echo)
    resp = await client.post("/echo", {"hello": "world"})
    assert resp == {"echoed": {"hello": "world"}}
    assert stub.calls[0] == ("POST", "/echo", {"hello": "world"}, {})


async def test_get_with_params(stub_client):
    client, stub = stub_client

    async def echo(body, query):
        return 200, {"q": query}

    stub.register("GET", "/echo", echo)
    resp = await client.get("/echo", params={"since": "5"})
    assert resp["q"] == {"since": ["5"]}


async def test_401_raises_unauthorized(stub_client):
    client, stub = stub_client

    async def deny(body, query):
        return 401, {"error": "nope"}

    stub.register("POST", "/x", deny)
    with pytest.raises(SdkUnauthorized):
        await client.post("/x", {})


async def test_500_raises_connection_error(stub_client):
    client, stub = stub_client

    async def boom(body, query):
        return 500, {"error": "boom"}

    stub.register("POST", "/x", boom)
    with pytest.raises(SdkConnectionError):
        await client.post("/x", {})
