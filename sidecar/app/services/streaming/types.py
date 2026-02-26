from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, Literal, TypedDict
from uuid import UUID


StreamEventType = Literal[
    "assistant_text",
    "assistant_thinking",
    "tool_started",
    "tool_completed",
    "tool_failed",
    "user_text",
    "system",
    "prompt_suggestions",
]


@dataclass(kw_only=True)
class ChatStreamRequest:
    prompt: str
    session_id: str
    sandbox_name: str
    assistant_message_id: str


class ToolPayload(TypedDict, total=False):
    id: str
    name: str
    title: str
    status: Literal["started", "completed", "failed"]
    parent_id: str | None
    input: dict[str, Any] | None
    result: Any
    error: str


class StreamEvent(TypedDict, total=False):
    type: StreamEventType
    text: str
    thinking: str
    tool: ToolPayload
    data: dict[str, Any]
    suggestions: list[str]


@dataclass
class ActiveToolState:
    id: str
    name: str
    title: str
    parent_id: str | None
    input: dict[str, Any] | None

    def to_payload(self) -> ToolPayload:
        payload: ToolPayload = {
            "id": self.id,
            "name": self.name,
            "title": self.title,
            "parent_id": self.parent_id,
            "input": self.input or None,
        }
        return payload


@dataclass
class StreamSnapshotAccumulator:
    events: list[dict[str, Any]] = field(default_factory=list)
    text_parts: list[str] = field(default_factory=list)

    def add_event(self, kind: str, payload: dict[str, Any]) -> None:
        if kind == "assistant_text":
            text = payload.get("text")
            if isinstance(text, str) and text:
                self.text_parts.append(text)

        self.events.append({"type": kind, **payload})

    def to_render(self) -> dict[str, Any]:
        return {"events": self.events}

    @property
    def content_text(self) -> str:
        return "".join(self.text_parts)


class StreamEnvelope:
    @staticmethod
    def build(
        *,
        session_id: str,
        message_id: str,
        stream_id: str,
        seq: int,
        kind: str,
        payload: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        return {
            "sessionId": session_id,
            "messageId": message_id,
            "streamId": stream_id,
            "seq": seq,
            "kind": kind,
            "payload": payload or {},
            "ts": datetime.now(timezone.utc).isoformat(),
        }
