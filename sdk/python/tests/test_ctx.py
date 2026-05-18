import pytest

from agentserver_sdk.ctx import Ctx


def _setup_stub_envs(stub):
    """Make stub return two envs (alpha + hpc) via env/capabilities + envs/list."""
    stub.on(
        "envs/list",
        lambda p: {
            "envs": [
                {"name": "alpha", "type": "shell"},
                {"name": "hpc", "type": "hpc"},
            ]
        },
    )

    def caps(p):
        env_id = p["env_id"]
        if env_id == "alpha":
            return {
                "tools": [
                    {"name": "shell", "description": "run sh", "inputSchema": {}},
                    {"name": "read_file", "description": "read", "inputSchema": {}},
                ]
            }
        if env_id == "hpc":
            return {
                "tools": [
                    {"name": "shell", "description": "run sh", "inputSchema": {}},
                    {"name": "submit_task", "description": "submit HPC", "inputSchema": {}},
                ]
            }
        return {"error": {"code": -32602, "message": "unknown env"}}

    stub.on("env/capabilities", caps)


async def test_ctx_from_env_reads_env_vars(monkeypatch, stub):
    monkeypatch.setenv("AGENTSERVER_GATEWAY_URL", stub.url)
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_TOKEN", "tok")
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_ID", "ws")
    monkeypatch.setenv("AGENTSERVER_USER_ID", "u")
    ctx = Ctx.from_env()
    assert ctx.gateway_url == stub.url
    assert ctx.workspace_id == "ws"


async def test_ctx_envs_returns_env_objects(monkeypatch, stub):
    _setup_stub_envs(stub)
    monkeypatch.setenv("AGENTSERVER_GATEWAY_URL", stub.url)
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_TOKEN", "tok")
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_ID", "ws")
    monkeypatch.setenv("AGENTSERVER_USER_ID", "u")
    ctx = Ctx.from_env()
    try:
        envs = await ctx.envs()
        assert {e.name for e in envs} == {"alpha", "hpc"}
        hpc = next(e for e in envs if e.name == "hpc")
        assert hasattr(hpc, "submit_task")  # custom tool surfaced
    finally:
        await ctx.close()


async def test_ctx_env_by_name(monkeypatch, stub):
    _setup_stub_envs(stub)
    monkeypatch.setenv("AGENTSERVER_GATEWAY_URL", stub.url)
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_TOKEN", "tok")
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_ID", "ws")
    monkeypatch.setenv("AGENTSERVER_USER_ID", "u")
    ctx = Ctx.from_env()
    try:
        alpha = await ctx.env("alpha")
        assert alpha.name == "alpha"
        assert alpha.type == "shell"
    finally:
        await ctx.close()


async def test_ctx_env_missing_raises(monkeypatch, stub):
    _setup_stub_envs(stub)
    monkeypatch.setenv("AGENTSERVER_GATEWAY_URL", stub.url)
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_TOKEN", "tok")
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_ID", "ws")
    monkeypatch.setenv("AGENTSERVER_USER_ID", "u")
    ctx = Ctx.from_env()
    try:
        with pytest.raises(KeyError):
            await ctx.env("nope")
    finally:
        await ctx.close()


async def test_ctx_copy_uses_copy_path_tool(monkeypatch, stub):
    _setup_stub_envs(stub)
    stub.on("mcpServer/tool/call", lambda p: {"content": [], "isError": False})
    monkeypatch.setenv("AGENTSERVER_GATEWAY_URL", stub.url)
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_TOKEN", "tok")
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_ID", "ws")
    monkeypatch.setenv("AGENTSERVER_USER_ID", "u")
    ctx = Ctx.from_env()
    try:
        alpha = await ctx.env("alpha")
        hpc = await ctx.env("hpc")
        await ctx.copy(src=(alpha, "/a/x"), dst=(hpc, "/b/x"))
        call = next(m for m in stub.received if m.get("method") == "mcpServer/tool/call")
        assert call["params"]["tool"] == "copy_path"
        args = call["params"]["arguments"]
        # env-mcp's actual copy_path contract uses these argument keys
        # (verified at internal/codexappgateway/envmcp/tool_copy_path.go)
        assert args["source_environment_id"] == "alpha"
        assert args["source_path"] == "/a/x"
        assert args["destination_environment_id"] == "hpc"
        assert args["destination_path"] == "/b/x"
    finally:
        await ctx.close()


async def test_ctx_history_returns_records(monkeypatch, stub):
    stub.on("envs/list", lambda p: {"envs": []})
    stub.on(
        "operations/list",
        lambda p: {
            "operations": [
                {
                    "id": "op_1",
                    "env_id": "alpha",
                    "tool": "shell",
                    "is_error": False,
                    "started_at": "2026-05-18T10:00:00Z",
                    "duration_ms": 5,
                    "user_id": "u",
                    "source": "sdk",
                },
            ]
        },
    )
    monkeypatch.setenv("AGENTSERVER_GATEWAY_URL", stub.url)
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_TOKEN", "tok")
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_ID", "ws")
    monkeypatch.setenv("AGENTSERVER_USER_ID", "u")
    ctx = Ctx.from_env()
    try:
        ops = await ctx.history(limit=10)
        assert len(ops) == 1
        assert ops[0].tool == "shell"
        # Ensure filter forwarded
        call = next(m for m in stub.received if m.get("method") == "operations/list")
        assert call["params"]["limit"] == 10
    finally:
        await ctx.close()


async def test_ctx_envs_concurrent_callers_share_cache(monkeypatch, stub):
    """Two concurrent envs() calls should only trigger one envs/list fetch."""
    _setup_stub_envs(stub)
    monkeypatch.setenv("AGENTSERVER_GATEWAY_URL", stub.url)
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_TOKEN", "tok")
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_ID", "ws")
    monkeypatch.setenv("AGENTSERVER_USER_ID", "u")
    ctx = Ctx.from_env()
    try:
        import asyncio

        a, b = await asyncio.gather(ctx.envs(), ctx.envs())
        # Both lists must reference the same Env instances (cache shared)
        assert {e.name for e in a} == {e.name for e in b}
        for x, y in zip(
            sorted(a, key=lambda e: e.name), sorted(b, key=lambda e: e.name), strict=True
        ):
            assert x is y
        # Only ONE envs/list was issued, regardless of concurrent callers
        list_calls = [m for m in stub.received if m.get("method") == "envs/list"]
        assert len(list_calls) == 1
    finally:
        await ctx.close()


async def test_ctx_refresh_replaces_cache_atomically(monkeypatch, stub):
    """refresh() should not transiently expose a None cache to concurrent readers."""
    _setup_stub_envs(stub)
    monkeypatch.setenv("AGENTSERVER_GATEWAY_URL", stub.url)
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_TOKEN", "tok")
    monkeypatch.setenv("AGENTSERVER_WORKSPACE_ID", "ws")
    monkeypatch.setenv("AGENTSERVER_USER_ID", "u")
    ctx = Ctx.from_env()
    try:
        # prime cache
        first = await ctx.envs()
        # refresh and concurrent reader race
        import asyncio

        _, refreshed = await asyncio.gather(ctx.refresh(), ctx.envs())
        # The concurrent reader either gets the old cache or waits for the new one,
        # never a half-populated state. Both invariants:
        assert len(refreshed) == len(first)
    finally:
        await ctx.close()
