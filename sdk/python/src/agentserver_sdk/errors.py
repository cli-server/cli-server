"""SDK exception hierarchy."""
from __future__ import annotations

from typing import Any


class SdkError(Exception):
    """Base class for all SDK errors."""


class ConnectionError(SdkError):
    """Failure to establish or maintain the WS connection to the gateway."""


class NotConnectedError(SdkError):
    """Operation attempted before the client was connected."""


class ToolError(SdkError):
    """An env-mcp tool returned isError=true or the RPC errored."""

    def __init__(self, tool: str, env: str | None, message: str, raw: Any = None):
        super().__init__(f"{env or '?'}/{tool}: {message}")
        self.tool = tool
        self.env = env
        self.message = message
        self.raw = raw
