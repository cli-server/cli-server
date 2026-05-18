"""Ctx — workspace handle, entry point for the SDK.

`Ctx.from_env()` constructs a lazy handle (no I/O). The first `await`
on any method triggers WS connect + handshake. One thread per Ctx;
cached internally.
"""

from __future__ import annotations

import asyncio
import os
from dataclasses import dataclass, field
from typing import Any

from .client import WSClient
from .env import Env
from .types import OperationRecord, ToolMetadata


@dataclass
class Ctx:
    gateway_url: str
    workspace_id: str
    user_id: str | None
    _client: WSClient
    _envs_cache: list[Env] | None = field(init=False, default=None)
    _envs_lock: asyncio.Lock = field(init=False, default_factory=asyncio.Lock)

    @classmethod
    def from_env(cls) -> Ctx:
        url = os.environ.get("AGENTSERVER_GATEWAY_URL", "ws://localhost:8086/notebook/ws")
        token = os.environ.get("AGENTSERVER_WORKSPACE_TOKEN", "")
        workspace_id = os.environ.get("AGENTSERVER_WORKSPACE_ID", "")
        user_id = os.environ.get("AGENTSERVER_USER_ID")
        client = WSClient(url, token=token, workspace_id=workspace_id, user_id=user_id)
        return cls(gateway_url=url, workspace_id=workspace_id, user_id=user_id, _client=client)

    async def _fetch_envs(self) -> list[Env]:
        await self._client.connect()
        listing = await self._client._request("envs/list", {})
        envs: list[Env] = []
        for e in listing.get("envs", []):
            caps = await self._client._request(
                "env/capabilities",
                {"env_id": e["name"]},
            )
            tools = [ToolMetadata.from_dict(t) for t in caps.get("tools", [])]
            envs.append(
                Env(name=e["name"], type=e.get("type", ""), tools=tools, _client=self._client)
            )
        return envs

    async def envs(self) -> list[Env]:
        """List envs in the workspace. Caches inside Ctx for the kernel
        lifetime — call `refresh()` to refetch."""
        if self._envs_cache is not None:
            return list(self._envs_cache)
        async with self._envs_lock:
            if self._envs_cache is None:
                self._envs_cache = await self._fetch_envs()
        return list(self._envs_cache)

    async def env(self, name: str) -> Env:
        for e in await self.envs():
            if e.name == name:
                return e
        raise KeyError(f"env not found: {name}")

    async def refresh(self) -> None:
        async with self._envs_lock:
            self._envs_cache = None
            self._envs_cache = await self._fetch_envs()

    async def copy(self, *, src: tuple[Env, str], dst: tuple[Env, str]) -> None:
        src_env, src_path = src
        dst_env, dst_path = dst
        await self._client.mcp_tool_call(
            server="env_mcp",
            tool="copy_path",
            arguments={
                "source_environment_id": src_env.name,
                "source_path": src_path,
                "destination_environment_id": dst_env.name,
                "destination_path": dst_path,
            },
        )

    async def history(
        self,
        *,
        limit: int = 100,
        env: str | None = None,
        tool: str | None = None,
        is_error: bool | None = None,
        since: str | None = None,
        id: str | None = None,  # noqa: A002 (shadow of builtin is fine here)
    ) -> list[OperationRecord]:
        await self._client.connect()
        params: dict[str, Any] = {"limit": limit}
        if env is not None:
            params["env_id"] = env
        if tool is not None:
            params["tool"] = tool
        if is_error is not None:
            params["is_error"] = is_error
        if since is not None:
            params["since"] = since
        if id is not None:
            params["id"] = id
        resp = await self._client._request("operations/list", params)
        return [OperationRecord.from_dict(o) for o in resp.get("operations", [])]

    async def close(self) -> None:
        await self._client.close()
