"""Process — async context manager wrapping exec_command/stdin/output/terminate."""

from __future__ import annotations

import base64
import contextlib
from typing import TYPE_CHECKING, Any

from .errors import ToolError

if TYPE_CHECKING:
    from .env import Env


class Process:
    def __init__(self, env: Env, command: str) -> None:
        self.env = env
        self.command = command
        self.session_id: str | None = None
        self._terminated = False

    async def __aenter__(self) -> Process:
        raw = await self.env.call("exec_command", {"command": self.command})
        sc = raw.get("structuredContent") or {}
        session_id = sc.get("session_id")
        if not session_id:
            raise ToolError(
                tool="exec_command",
                env=self.env.name,
                message="exec_command did not return session_id",
                raw=raw,
            )
        self.session_id = session_id
        return self

    async def __aexit__(self, exc_type, exc, tb) -> None:
        await self.terminate()

    async def write_stdin(self, data: bytes) -> None:
        await self.env.call(
            "write_stdin",
            {
                "session_id": self.session_id,
                "data_b64": base64.b64encode(data).decode("ascii"),
            },
        )

    async def read_output(self, timeout: float | None = None) -> bytes:
        args: dict[str, Any] = {"session_id": self.session_id}
        if timeout is not None:
            args["timeout_ms"] = int(timeout * 1000)
        raw = await self.env.call("read_output", args)
        sc = raw.get("structuredContent") or {}
        b64 = sc.get("chunk_b64")
        if b64 is not None:
            return base64.b64decode(b64)
        # fallback to text content
        texts = [it.get("text", "") for it in raw.get("content", []) if it.get("type") == "text"]
        return "".join(texts).encode("utf-8")

    async def terminate(self) -> None:
        if self._terminated or self.session_id is None:
            self._terminated = True
            return
        self._terminated = True
        # best-effort; don't mask a user exception
        with contextlib.suppress(Exception):
            await self.env.call("terminate", {"session_id": self.session_id})
