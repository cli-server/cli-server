from __future__ import annotations

import json
import logging
import uuid
from typing import Any

from sqlalchemy import text
from sqlalchemy.ext.asyncio import AsyncEngine

logger = logging.getLogger(__name__)


async def create_message(
    engine: AsyncEngine,
    session_id: str,
    content: str,
    role: str,
) -> str:
    """INSERT a new message row and return its UUID as a string."""
    message_id = str(uuid.uuid4())
    query = text(
        """
        INSERT INTO messages (id, session_id, role, content_text, stream_status)
        VALUES (:id, :session_id, :role, :content_text, :stream_status)
        """
    )
    async with engine.begin() as conn:
        await conn.execute(
            query,
            {
                "id": message_id,
                "session_id": session_id,
                "role": role,
                "content_text": content,
                "stream_status": "in_progress" if role == "assistant" else "completed",
            },
        )
    return message_id


async def append_event(
    engine: AsyncEngine,
    session_id: str,
    message_id: str,
    stream_id: str,
    seq: int,
    event_type: str,
    render_payload: dict[str, Any],
) -> None:
    """INSERT a single message_event row."""
    query = text(
        """
        INSERT INTO message_events (id, session_id, message_id, stream_id, seq, event_type, render_payload)
        VALUES (:id, :session_id, :message_id, :stream_id, :seq, :event_type, :render_payload)
        """
    )
    async with engine.begin() as conn:
        await conn.execute(
            query,
            {
                "id": str(uuid.uuid4()),
                "session_id": session_id,
                "message_id": message_id,
                "stream_id": stream_id,
                "seq": seq,
                "event_type": event_type,
                "render_payload": _strip_null_bytes(json.dumps(render_payload)),
            },
        )


async def append_events_batch(
    engine: AsyncEngine,
    events_list: list[dict[str, Any]],
) -> None:
    """Batch INSERT multiple message_event rows."""
    if not events_list:
        return
    query = text(
        """
        INSERT INTO message_events (id, session_id, message_id, stream_id, seq, event_type, render_payload)
        VALUES (:id, :session_id, :message_id, :stream_id, :seq, :event_type, :render_payload)
        """
    )
    rows = []
    for evt in events_list:
        rows.append(
            {
                "id": str(uuid.uuid4()),
                "session_id": evt["session_id"],
                "message_id": evt["message_id"],
                "stream_id": evt["stream_id"],
                "seq": evt["seq"],
                "event_type": evt["event_type"],
                "render_payload": _strip_null_bytes(json.dumps(evt["render_payload"])),
            }
        )
    async with engine.begin() as conn:
        await conn.execute(query, rows)


def _strip_null_bytes(s: str) -> str:
    """Remove null bytes that PostgreSQL text/jsonb cannot store."""
    return s.replace("\x00", "")


async def update_message_snapshot(
    engine: AsyncEngine,
    message_id: str,
    content_text: str,
    content_render: dict[str, Any],
    last_seq: int,
    stream_status: str,
    total_cost_usd: float,
) -> None:
    """UPDATE a message row with accumulated snapshot data."""
    query = text(
        """
        UPDATE messages
        SET content_text = :content_text,
            content_render = :content_render,
            last_seq = :last_seq,
            stream_status = :stream_status,
            total_cost_usd = :total_cost_usd
        WHERE id = :message_id
        """
    )
    async with engine.begin() as conn:
        await conn.execute(
            query,
            {
                "message_id": message_id,
                "content_text": _strip_null_bytes(content_text),
                "content_render": _strip_null_bytes(json.dumps(content_render)),
                "last_seq": last_seq,
                "stream_status": stream_status,
                "total_cost_usd": total_cost_usd,
            },
        )


async def get_next_seq(engine: AsyncEngine, session_id: str) -> int:
    """Return MAX(seq) + 1 for a session, or 1 if no events exist."""
    query = text(
        """
        SELECT COALESCE(MAX(seq), 0) + 1 AS next_seq
        FROM message_events
        WHERE session_id = :session_id
        """
    )
    async with engine.connect() as conn:
        result = await conn.execute(query, {"session_id": session_id})
        row = result.fetchone()
        return row[0] if row else 1


async def has_messages(engine: AsyncEngine, session_id: str) -> bool:
    """Return True if the session has at least one completed assistant message."""
    query = text(
        "SELECT 1 FROM messages WHERE session_id = :session_id AND role = 'assistant' AND stream_status = 'completed' LIMIT 1"
    )
    async with engine.connect() as conn:
        result = await conn.execute(query, {"session_id": session_id})
        return result.fetchone() is not None


async def get_events_after(
    engine: AsyncEngine,
    session_id: str,
    after_seq: int,
) -> list[dict[str, Any]]:
    """SELECT message_events with seq > after_seq, ordered by seq."""
    query = text(
        """
        SELECT id, session_id, message_id, stream_id, seq, event_type, render_payload, created_at
        FROM message_events
        WHERE session_id = :session_id AND seq > :after_seq
        ORDER BY seq ASC
        """
    )
    async with engine.connect() as conn:
        result = await conn.execute(
            query,
            {"session_id": session_id, "after_seq": after_seq},
        )
        rows = result.fetchall()

    events = []
    for row in rows:
        payload = row[6]
        if isinstance(payload, str):
            payload = json.loads(payload)
        events.append(
            {
                "id": str(row[0]),
                "session_id": row[1],
                "message_id": str(row[2]),
                "stream_id": str(row[3]),
                "seq": row[4],
                "event_type": row[5],
                "render_payload": payload,
                "created_at": row[7].isoformat() if row[7] else None,
            }
        )
    return events
