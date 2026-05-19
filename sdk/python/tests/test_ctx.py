import pytest

from agentserver_sdk.ctx import Ctx


async def test_envs_returns_parsed_list(stub_client):
    client, stub = stub_client
    ctx = Ctx(client)

    async def envs(body, query):
        return 200, {
            "envs": [
                {
                    "name": "my-mac",
                    "type": "executor",
                    "is_default": True,
                    "tools": [{"name": "shell", "description": "...", "kind": "core"}],
                }
            ]
        }

    stub.register("POST", "/api/sdk/envs/list", envs)
    result = await ctx.envs()
    assert len(result) == 1
    assert result[0].name == "my-mac"
    assert result[0].tools[0].name == "shell"


async def test_envs_cached_until_refresh(stub_client):
    client, stub = stub_client
    ctx = Ctx(client)
    calls = {"n": 0}

    async def envs(body, query):
        calls["n"] += 1
        return 200, {"envs": []}

    stub.register("POST", "/api/sdk/envs/list", envs)
    await ctx.envs()
    await ctx.envs()
    assert calls["n"] == 1

    await ctx.refresh()
    await ctx.envs()
    assert calls["n"] == 2


async def test_env_by_name_returns_matching_env(stub_client):
    client, stub = stub_client
    ctx = Ctx(client)

    async def envs(body, query):
        return 200, {
            "envs": [
                {"name": "alpha", "type": "shell", "tools": []},
                {"name": "hpc", "type": "hpc", "tools": []},
            ]
        }

    stub.register("POST", "/api/sdk/envs/list", envs)
    alpha = await ctx.env("alpha")
    assert alpha.name == "alpha"
    assert alpha.type == "shell"


async def test_env_by_name_missing_raises_key_error(stub_client):
    client, stub = stub_client
    ctx = Ctx(client)

    async def envs(body, query):
        return 200, {"envs": []}

    stub.register("POST", "/api/sdk/envs/list", envs)
    with pytest.raises(KeyError):
        await ctx.env("nope")


async def test_from_env_reads_env_vars(monkeypatch, stub_client):
    client, stub = stub_client
    monkeypatch.setenv("AGENTSERVER_GATEWAY_URL", "http://stub")
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_TOKEN", "tok")
    ctx = Ctx.from_env()
    assert ctx._client.base_url == "http://stub"
    await ctx.close()


async def test_envs_concurrent_callers_share_cache(stub_client):
    """Two concurrent envs() calls should only trigger one /api/sdk/envs/list fetch."""
    import asyncio

    client, stub = stub_client
    ctx = Ctx(client)
    calls = {"n": 0}

    async def envs(body, query):
        calls["n"] += 1
        return 200, {
            "envs": [
                {"name": "alpha", "type": "shell", "tools": []},
                {"name": "beta", "type": "shell", "tools": []},
            ]
        }

    stub.register("POST", "/api/sdk/envs/list", envs)
    a, b = await asyncio.gather(ctx.envs(), ctx.envs())
    assert {e.name for e in a} == {e.name for e in b}
    for x, y in zip(
        sorted(a, key=lambda e: e.name), sorted(b, key=lambda e: e.name), strict=True
    ):
        assert x is y
    assert calls["n"] == 1
