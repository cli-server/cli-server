"""Env class — one instance per executor; wraps env-mcp tool calls."""
from __future__ import annotations

import base64
from dataclasses import dataclass, field
from typing import Any, TYPE_CHECKING

from .errors import ToolError
from .types import ShellResult, ToolMetadata

if TYPE_CHECKING:
    from .client import WSClient
    from .process import Process


_TOOL_SERVER = "env_mcp"  # all env-mcp tools live under one MCP server name


@dataclass
class Env:
    name: str
    type: str
    tools: list[ToolMetadata]
    _client: "WSClient"
    _tool_index: dict[str, ToolMetadata] = field(init=False)

    def __post_init__(self) -> None:
        self._tool_index = {t.name: t for t in self.tools}

    # ---------- generic dispatch ----------

    async def call(self, tool: str, arguments: dict[str, Any] | None = None) -> dict[str, Any]:
        """Universal MCP tool call — even tools the SDK doesn't know about.

        `environment_id` is injected automatically. Raises ToolError on
        isError=true. Returns the raw MCP result dict.
        """
        args = dict(arguments or {})
        args.setdefault("environment_id", self.name)
        raw = await self._client.mcp_tool_call(
            server=_TOOL_SERVER, tool=tool, arguments=args,
        )
        if raw.get("isError"):
            msg = _extract_error_text(raw)
            raise ToolError(tool=tool, env=self.name, message=msg, raw=raw)
        return raw

    # ---------- core typed wrappers ----------

    async def shell(self, command: str, *, timeout: int | None = None) -> ShellResult:
        args: dict[str, Any] = {"command": command}
        if timeout is not None:
            args["timeout_s"] = timeout
        raw = await self.call("shell", args)
        return ShellResult.from_mcp(raw)

    async def read_file(self, path: str) -> bytes:
        raw = await self.call("read_file", {"path": path})
        return _decode_file_content(raw)

    async def write_file(self, path: str, content: bytes) -> None:
        await self.call("write_file", {
            "path": path,
            "content_b64": base64.b64encode(content).decode("ascii"),
        })

    async def apply_patch(self, patch: str) -> None:
        await self.call("apply_patch", {"patch": patch})

    def spawn(self, command: str) -> "Process":
        """Start a long-running command. Use as `async with env.spawn(cmd) as proc:`.

        Returns a `Process`; the actual `exec_command` is sent on `__aenter__`.
        """
        from .process import Process  # avoid circular at module load
        return Process(self, command=command)


# ---------- helpers ----------

def _extract_error_text(raw: dict[str, Any]) -> str:
    items = raw.get("content", [])
    texts = [it.get("text", "") for it in items if it.get("type") == "text"]
    return " ".join(texts) or "tool reported isError=true"


def _decode_file_content(raw: dict[str, Any]) -> bytes:
    """env-mcp's read_file convention: text content is base64 if
    structuredContent.encoding == 'base64', else raw text bytes.

    v0 stub mirrors this; real env-mcp may differ — adjust if needed."""
    sc = raw.get("structuredContent") or {}
    items = raw.get("content", [])
    text = "".join(it.get("text", "") for it in items if it.get("type") == "text")
    if sc.get("encoding") == "base64":
        return base64.b64decode(text)
    return text.encode("utf-8")
