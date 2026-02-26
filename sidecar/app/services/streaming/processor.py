from __future__ import annotations

import json
import logging
import re
from collections.abc import Iterable
from typing import Any

from claude_agent_sdk.types import (
    AssistantMessage,
    ResultMessage,
    SystemMessage,
    TextBlock,
    ThinkingBlock,
    ToolResultBlock,
    ToolUseBlock,
    UserMessage,
)

from app.services.streaming.types import StreamEvent
from app.services.tool_handler import ToolHandlerRegistry

logger = logging.getLogger(__name__)

PROMPT_SUGGESTIONS_RE = re.compile(
    r"<prompt_suggestions>\s*(.*?)\s*</prompt_suggestions>", re.DOTALL
)
LOCAL_COMMAND_STDOUT_RE = re.compile(
    r"<local-command-stdout>(.*?)</local-command-stdout>", re.DOTALL
)


class StreamProcessor:
    def __init__(
        self,
        tool_registry: ToolHandlerRegistry,
        *,
        on_session_init: Any | None = None,
    ) -> None:
        self._tool_registry = tool_registry
        self._on_session_init = on_session_init
        self.total_cost_usd: float = 0.0
        self.usage: dict[str, Any] = {}

    def emit_events_for_message(
        self, message: Any
    ) -> Iterable[StreamEvent]:
        if isinstance(message, SystemMessage):
            yield from self._emit_system_events(message)
        elif isinstance(message, AssistantMessage):
            yield from self._emit_assistant_events(message)
        elif isinstance(message, UserMessage):
            yield from self._emit_user_events(message)
        elif isinstance(message, ResultMessage):
            yield from self._emit_result_events(message)

    def _emit_system_events(self, message: SystemMessage) -> Iterable[StreamEvent]:
        if self._on_session_init and hasattr(message, "session_id"):
            self._on_session_init(message.session_id)

        event: StreamEvent = {
            "type": "system",
            "data": {"subtype": "session_init"},
        }
        yield event

    def _emit_assistant_events(
        self, message: AssistantMessage
    ) -> Iterable[StreamEvent]:
        parent_tool_id: str | None = getattr(message, "parent_tool_use_id", None)

        for block in message.content:
            yield from self._emit_block_events(block, parent_tool_id=parent_tool_id)

    def _emit_block_events(
        self, block: Any, *, parent_tool_id: str | None = None
    ) -> Iterable[StreamEvent]:
        if isinstance(block, TextBlock):
            yield from self._emit_text_block(block)
        elif isinstance(block, ThinkingBlock):
            yield from self._emit_thinking_block(block)
        elif isinstance(block, ToolUseBlock):
            yield from self._emit_tool_start(block, parent_tool_id=parent_tool_id)
        elif isinstance(block, ToolResultBlock):
            yield from self._emit_tool_result(block)

    def _emit_text_block(self, block: TextBlock) -> Iterable[StreamEvent]:
        text = block.text if hasattr(block, "text") else str(block)

        # Extract and emit prompt suggestions if present
        suggestions_match = PROMPT_SUGGESTIONS_RE.search(text)
        if suggestions_match:
            raw = suggestions_match.group(1).strip()
            try:
                suggestions = json.loads(raw)
                if isinstance(suggestions, list):
                    event: StreamEvent = {
                        "type": "prompt_suggestions",
                        "suggestions": suggestions,
                    }
                    yield event
            except json.JSONDecodeError:
                pass
            # Remove prompt suggestions from the text
            text = PROMPT_SUGGESTIONS_RE.sub("", text).strip()

        if text:
            event = StreamEvent(type="assistant_text", text=text)
            yield event

    def _emit_thinking_block(self, block: ThinkingBlock) -> Iterable[StreamEvent]:
        thinking = block.thinking if hasattr(block, "thinking") else str(block)
        if thinking:
            event: StreamEvent = {
                "type": "assistant_thinking",
                "thinking": thinking,
            }
            yield event

    def _emit_tool_start(
        self, block: ToolUseBlock, *, parent_tool_id: str | None = None
    ) -> Iterable[StreamEvent]:
        event = self._tool_registry.start_tool(
            block, parent_tool_id=parent_tool_id
        )
        if event:
            yield event

    def _emit_tool_result(self, block: ToolResultBlock) -> Iterable[StreamEvent]:
        tool_use_id = getattr(block, "tool_use_id", None)
        is_error = getattr(block, "is_error", False)
        content = getattr(block, "content", None)

        event = self._tool_registry.finish_tool(
            tool_use_id, content, is_error=is_error
        )
        if event:
            yield event

    def _emit_user_events(self, message: UserMessage) -> Iterable[StreamEvent]:
        for block in message.content:
            if isinstance(block, TextBlock):
                text = block.text if hasattr(block, "text") else str(block)

                # Try extracting command stdout
                match = LOCAL_COMMAND_STDOUT_RE.search(text)
                if match:
                    text = match.group(1).strip()

                if text:
                    event: StreamEvent = {
                        "type": "user_text",
                        "text": text,
                    }
                    yield event
            elif isinstance(block, ToolResultBlock):
                yield from self._emit_tool_result(block)

    def _emit_result_events(self, message: ResultMessage) -> Iterable[StreamEvent]:
        if hasattr(message, "cost_usd") and message.cost_usd is not None:
            self.total_cost_usd += message.cost_usd

        if hasattr(message, "usage") and message.usage is not None:
            self.usage = message.usage if isinstance(message.usage, dict) else {}

        # Result messages don't emit stream events directly
        return
        yield  # Make this a generator
