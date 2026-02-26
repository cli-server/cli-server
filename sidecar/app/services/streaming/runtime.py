from __future__ import annotations

import asyncio
import json
import logging
import time
import uuid
from dataclasses import dataclass, field
from typing import Any

import redis.asyncio as aioredis
from sqlalchemy.ext.asyncio import AsyncEngine

from app.services.message import (
    append_event,
    append_events_batch,
    get_next_seq,
    has_messages,
    update_message_snapshot,
)
from app.services.session_registry import ChatSession, SessionRegistry
from app.services.streaming.processor import StreamProcessor
from app.services.streaming.types import (
    ChatStreamRequest,
    StreamEnvelope,
    StreamSnapshotAccumulator,
)
from app.services.tool_handler import ToolHandlerRegistry
from app.services.claude_agent import build_options, create_transport_factory

logger = logging.getLogger(__name__)

REDIS_CHANNEL_PREFIX = "chat:stream:live:"
SNAPSHOT_FLUSH_INTERVAL = 0.2  # seconds
SNAPSHOT_FLUSH_EVENT_COUNT = 24


@dataclass
class StreamContext:
    """Holds mutable state for a single streaming run."""

    session_id: str
    message_id: str
    stream_id: str = field(default_factory=lambda: str(uuid.uuid4()))
    seq: int = 0
    snapshot: StreamSnapshotAccumulator = field(
        default_factory=StreamSnapshotAccumulator
    )
    cancel_event: asyncio.Event = field(default_factory=asyncio.Event)
    last_flush_at: float = field(default_factory=time.monotonic)
    events_since_flush: int = 0
    pending_events: list[dict[str, Any]] = field(default_factory=list)


class ChatStreamRuntime:
    def __init__(
        self,
        engine: AsyncEngine,
        redis: aioredis.Redis,
        session_registry: SessionRegistry,
    ) -> None:
        self._engine = engine
        self._redis = redis
        self._session_registry = session_registry
        self._background_task_chat_ids: set[str] = set()

    def start_background_chat(
        self, request: ChatStreamRequest
    ) -> asyncio.Task[None]:
        """Spawn a background task that drives the full chat stream."""

        async def _run_wrapper() -> None:
            try:
                await self.execute_chat(request)
            except Exception:
                logger.exception(
                    "Background chat task failed for session %s",
                    request.session_id,
                )
            finally:
                self._background_task_chat_ids.discard(request.session_id)

        self._background_task_chat_ids.add(request.session_id)
        task = asyncio.create_task(_run_wrapper(), name=f"chat-{request.session_id}")
        return task

    async def execute_chat(self, request: ChatStreamRequest) -> None:
        """Obtain or create a session via the registry, then run the stream."""
        has_history = await has_messages(self._engine, request.session_id)
        options = build_options(request.session_id, request.sandbox_name, continue_conversation=has_history)
        transport_factory = create_transport_factory(request.sandbox_name, options=options)

        session: ChatSession = await self._session_registry.get_or_create(
            chat_id=request.session_id,
            sandbox_id=request.sandbox_name,
            options=options,
            transport_factory=transport_factory,
        )

        # Wire the cancel event from the session registry
        session.cancel_event.clear()

        await self.run(session.client, request, session)

    async def run(
        self,
        client: Any,
        request: ChatStreamRequest,
        session: ChatSession,
    ) -> None:
        """Main streaming loop: send prompt, consume responses, emit events."""
        ctx = StreamContext(
            session_id=request.session_id,
            message_id=request.assistant_message_id,
            cancel_event=session.cancel_event,
        )

        # Get the starting sequence number
        ctx.seq = await get_next_seq(self._engine, request.session_id)

        processor = StreamProcessor(ToolHandlerRegistry())

        try:
            # Send the user prompt to the Claude CLI
            await client.query(request.prompt)

            # Iterate over streaming response messages
            async for message in client.receive_response():
                # Check for cancellation
                if ctx.cancel_event.is_set():
                    logger.info(
                        "Stream cancelled for session %s", request.session_id
                    )
                    await self._emit_event("cancelled", {}, ctx)
                    break

                # Process each message into stream events
                for event in processor.emit_events_for_message(message):
                    kind = event.get("type", "unknown")
                    payload: dict[str, Any] = {
                        k: v for k, v in event.items() if k != "type"
                    }
                    await self._emit_event(kind, payload, ctx)

            # On normal completion, emit a complete event
            if not ctx.cancel_event.is_set():
                complete_payload: dict[str, Any] = {
                    "total_cost_usd": processor.total_cost_usd,
                }
                if processor.usage:
                    complete_payload["usage"] = processor.usage
                await self._emit_event("complete", complete_payload, ctx)

        except asyncio.CancelledError:
            logger.info("Stream task cancelled for session %s", request.session_id)
            await self._emit_event("cancelled", {}, ctx)
        except Exception as exc:
            logger.exception(
                "Error during streaming for session %s", request.session_id
            )
            await self._emit_event(
                "error",
                {"message": str(exc), "type": type(exc).__name__},
                ctx,
            )
        finally:
            # Flush any remaining snapshot data
            await self._flush_snapshot(ctx, processor, force=True)

    async def _emit_event(
        self,
        kind: str,
        payload: dict[str, Any],
        ctx: StreamContext,
    ) -> None:
        """Persist event to DB, publish to Redis, accumulate in snapshot."""
        seq = ctx.seq
        ctx.seq += 1

        # Add to snapshot accumulator
        ctx.snapshot.add_event(kind, payload)

        # Build the envelope for Redis/SSE
        envelope = StreamEnvelope.build(
            session_id=ctx.session_id,
            message_id=ctx.message_id,
            stream_id=ctx.stream_id,
            seq=seq,
            kind=kind,
            payload=payload,
        )

        # Queue the event for batch persistence
        ctx.pending_events.append(
            {
                "session_id": ctx.session_id,
                "message_id": ctx.message_id,
                "stream_id": ctx.stream_id,
                "seq": seq,
                "event_type": kind,
                "render_payload": payload,
            }
        )

        # Publish to Redis for live subscribers
        channel = f"{REDIS_CHANNEL_PREFIX}{ctx.session_id}"
        try:
            await self._redis.publish(channel, json.dumps(envelope))
        except Exception as exc:
            logger.warning("Failed to publish event to Redis: %s", exc)

        ctx.events_since_flush += 1

        # Throttled flush to DB
        now = time.monotonic()
        if (
            ctx.events_since_flush >= SNAPSHOT_FLUSH_EVENT_COUNT
            or (now - ctx.last_flush_at) >= SNAPSHOT_FLUSH_INTERVAL
        ):
            await self._flush_snapshot(ctx, None, force=False)

    async def _flush_snapshot(
        self,
        ctx: StreamContext,
        processor: StreamProcessor | None = None,
        *,
        force: bool = False,
    ) -> None:
        """Write pending events to DB and update the message snapshot."""
        if not ctx.pending_events and not force:
            return

        # Batch insert pending events
        if ctx.pending_events:
            try:
                await append_events_batch(self._engine, ctx.pending_events)
            except Exception as exc:
                logger.error("Failed to batch insert events: %s", exc)
                # Fall back to individual inserts
                for evt in ctx.pending_events:
                    try:
                        await append_event(
                            self._engine,
                            evt["session_id"],
                            evt["message_id"],
                            evt["stream_id"],
                            evt["seq"],
                            evt["event_type"],
                            evt["render_payload"],
                        )
                    except Exception as inner_exc:
                        logger.error("Failed to insert event: %s", inner_exc)
            ctx.pending_events.clear()

        # Update message snapshot
        total_cost = 0.0
        if processor:
            total_cost = processor.total_cost_usd

        # Determine stream status
        stream_status = "in_progress"
        if force:
            # Final flush - check if cancelled or completed
            if ctx.cancel_event.is_set():
                stream_status = "interrupted"
            else:
                stream_status = "completed"

        try:
            await update_message_snapshot(
                self._engine,
                ctx.message_id,
                content_text=ctx.snapshot.content_text,
                content_render=ctx.snapshot.to_render(),
                last_seq=ctx.seq - 1 if ctx.seq > 0 else 0,
                stream_status=stream_status,
                total_cost_usd=total_cost,
            )
        except Exception as exc:
            logger.error("Failed to update message snapshot: %s", exc)

        ctx.last_flush_at = time.monotonic()
        ctx.events_since_flush = 0
