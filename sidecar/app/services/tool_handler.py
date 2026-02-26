from __future__ import annotations

import json
import logging
from copy import deepcopy
from typing import Any, Literal, cast

from claude_agent_sdk.types import ToolUseBlock

from app.services.streaming.types import ActiveToolState, StreamEvent

logger = logging.getLogger(__name__)

_MAX_DESC_LEN = 60


def _truncate(s: str, max_len: int = _MAX_DESC_LEN) -> str:
    s = s.strip().split("\n", 1)[0].strip()
    if len(s) > max_len:
        return s[:max_len - 1] + "\u2026"
    return s


def _extract_tool_description(tool_name: str, tool_input: dict[str, Any]) -> str:
    """Extract a short human-readable description from tool input parameters."""
    # Bash / command execution
    if tool_name in ("Bash", "bash"):
        desc = tool_input.get("description") or tool_input.get("command")
        return _truncate(desc) if desc else ""

    # Task / subagent
    if tool_name in ("Task", "task"):
        return _truncate(tool_input["description"]) if tool_input.get("description") else ""

    # File reads
    if tool_name in ("Read", "read"):
        path = tool_input.get("file_path", "")
        return _truncate(path) if path else ""

    # File writes
    if tool_name in ("Write", "write"):
        path = tool_input.get("file_path", "")
        return _truncate(path) if path else ""

    # Edit
    if tool_name in ("Edit", "edit"):
        path = tool_input.get("file_path", "")
        return _truncate(path) if path else ""

    # Glob / file search
    if tool_name in ("Glob", "glob"):
        pattern = tool_input.get("pattern", "")
        return _truncate(pattern) if pattern else ""

    # Grep / content search
    if tool_name in ("Grep", "grep"):
        pattern = tool_input.get("pattern", "")
        return _truncate(pattern) if pattern else ""

    # WebFetch
    if tool_name in ("WebFetch", "web_fetch"):
        url = tool_input.get("url", "")
        return _truncate(url) if url else ""

    # WebSearch
    if tool_name in ("WebSearch", "web_search"):
        query = tool_input.get("query", "")
        return _truncate(query) if query else ""

    # TodoWrite / TodoRead / TaskCreate / TaskUpdate etc.
    if tool_name in ("TodoWrite", "TaskCreate"):
        subject = tool_input.get("subject", "")
        return _truncate(subject) if subject else ""

    # Generic: try common fields
    for key in ("description", "prompt", "query", "file_path", "pattern", "command"):
        val = tool_input.get(key)
        if isinstance(val, str) and val.strip():
            return _truncate(val)

    return ""


class ToolHandlerRegistry:
    def __init__(self) -> None:
        self._active: dict[str, ActiveToolState] = {}

    @staticmethod
    def _format_tool_title(tool_name: str, tool_input: dict[str, Any] | None) -> str:
        """Generate a descriptive title like Claude Code CLI does, e.g. Bash(Build sidecar image)."""
        base = tool_name
        if tool_name.startswith("mcp__"):
            parts = tool_name.split("__", maxsplit=2)
            if len(parts) == 3:
                base = parts[2].replace("_", " ")

        if not tool_input:
            return base

        # Extract a short description from tool input based on tool type
        desc = _extract_tool_description(tool_name, tool_input)
        if desc:
            return f"{base}({desc})"
        return base

    def start_tool(
        self,
        content_block: ToolUseBlock,
        *,
        parent_tool_id: str | None = None,
    ) -> StreamEvent | None:
        if not content_block.id:
            return None

        input_copy = None
        if hasattr(content_block, "input") and isinstance(content_block.input, dict):
            try:
                input_copy = deepcopy(content_block.input)
            except Exception:
                input_copy = dict(content_block.input)

        tool_state = ActiveToolState(
            id=content_block.id,
            name=content_block.name,
            title=self._format_tool_title(content_block.name, input_copy),
            parent_id=parent_tool_id,
            input=input_copy,
        )

        self._active[content_block.id] = tool_state

        payload = tool_state.to_payload()
        payload["status"] = "started"
        event: StreamEvent = {
            "type": "tool_started",
            "tool": payload,
        }
        return event

    def finish_tool(
        self,
        tool_use_id: str | None,
        raw_result: Any,
        *,
        is_error: bool = False,
    ) -> StreamEvent | None:
        if not tool_use_id:
            return None

        state = self._active.pop(tool_use_id, None)
        if not state:
            state = ActiveToolState(
                id=tool_use_id,
                name="unknown",
                title="Unknown tool",
                parent_id=None,
                input=None,
            )

        payload = state.to_payload()
        payload["status"] = "failed" if is_error else "completed"

        if is_error:
            payload["error"] = self._stringify_result(raw_result)
        else:
            normalized = self._normalize_result(raw_result)
            payload["result"] = normalized

        event_type: Literal["tool_failed", "tool_completed"] = (
            "tool_failed" if is_error else "tool_completed"
        )
        event: StreamEvent = {"type": event_type, "tool": payload}
        return event

    def _normalize_result(self, result: Any) -> Any:
        if result is None:
            return None

        if isinstance(result, list):
            return [self._normalize_result(item) for item in result]

        if isinstance(result, dict):
            return {key: self._normalize_result(value) for key, value in result.items()}

        if isinstance(result, str):
            text = result.strip()
            if not text:
                return ""
            try:
                return cast(Any, json.loads(text))
            except json.JSONDecodeError:
                return text

        return result

    def _stringify_result(self, result: Any) -> str:
        if isinstance(result, str):
            return result
        try:
            return json.dumps(result, ensure_ascii=False)
        except TypeError:
            return str(result)
