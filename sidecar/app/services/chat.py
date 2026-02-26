from __future__ import annotations

import asyncio
import json
import logging
from collections.abc import AsyncGenerator
from typing import Any

import redis.asyncio as aioredis
from sqlalchemy.ext.asyncio import AsyncEngine

from app.services.message import create_message, get_events_after
from app.services.session_registry import session_registry
from app.services.streaming.runtime import REDIS_CHANNEL_PREFIX, ChatStreamRuntime
from app.services.streaming.types import ChatStreamRequest

logger = logging.getLogger(__name__)


class ChatService:
    def __init__(
        self,
        engine: AsyncEngine,
        redis: aioredis.Redis,
        runtime: ChatStreamRuntime,
    ) -> None:
        self._engine = engine
        self._redis = redis
        self._runtime = runtime

    async def initiate_chat_completion(
        self,
        session_id: str,
        sandbox_name: str,
        prompt: str,
    ) -> dict[str, Any]:
        """Create user + assistant messages, kick off background streaming task."""
        # 1. Persist the user message
        await create_message(
            self._engine,
            session_id=session_id,
            content=prompt,
            role="user",
        )

        # 2. Create a placeholder assistant message
        assistant_message_id = await create_message(
            self._engine,
            session_id=session_id,
            content="",
            role="assistant",
        )

        # 3. Build stream request
        request = ChatStreamRequest(
            prompt=prompt,
            session_id=session_id,
            sandbox_name=sandbox_name,
            assistant_message_id=assistant_message_id,
        )

        # 4. Start background streaming task
        task = self._runtime.start_background_chat(request)

        # Store the task reference on the session (if it exists)
        chat_session = session_registry.get_session(session_id)
        if chat_session is not None:
            chat_session.active_generation_task = task

        return {
            "message_id": assistant_message_id,
            "session_id": session_id,
        }

    async def create_event_stream(
        self,
        session_id: str,
        after_seq: int = 0,
    ) -> AsyncGenerator[dict[str, Any], None]:
        """Replay backlog from DB, then subscribe to live Redis events."""
        # 1. Replay persisted events from the database
        backlog = await get_events_after(self._engine, session_id, after_seq)
        max_seq = after_seq

        for event in backlog:
            seq = event.get("seq", 0)
            if seq > max_seq:
                max_seq = seq
            envelope = {
                "sessionId": event["session_id"],
                "messageId": event["message_id"],
                "streamId": event["stream_id"],
                "seq": seq,
                "kind": event["event_type"],
                "payload": event["render_payload"],
            }
            yield {"event": "stream", "data": json.dumps(envelope)}

        # 2. Subscribe to live Redis channel for new events
        channel_name = f"{REDIS_CHANNEL_PREFIX}{session_id}"
        pubsub = self._redis.pubsub()
        await pubsub.subscribe(channel_name)

        try:
            while True:
                message = await pubsub.get_message(
                    ignore_subscribe_messages=True,
                    timeout=30.0,
                )

                if message is None:
                    # Send a keepalive comment to prevent SSE timeout
                    yield {"event": "ping", "data": ""}
                    continue

                if message["type"] != "message":
                    continue

                raw_data = message["data"]
                if isinstance(raw_data, bytes):
                    raw_data = raw_data.decode("utf-8")

                try:
                    envelope = json.loads(raw_data)
                except json.JSONDecodeError:
                    logger.warning("Invalid JSON from Redis pubsub: %s", raw_data)
                    continue

                event_seq = envelope.get("seq", 0)
                if event_seq <= max_seq:
                    # Skip events we already replayed from the backlog
                    continue

                max_seq = event_seq

                yield {"event": "stream", "data": json.dumps(envelope)}

                # If this is a terminal event, close the stream
                kind = envelope.get("kind", "")
                if kind in ("complete", "cancelled", "error"):
                    break

        except asyncio.CancelledError:
            pass
        finally:
            await pubsub.unsubscribe(channel_name)
            await pubsub.aclose()

    async def stop_stream(self, session_id: str) -> None:
        """Signal the session registry to cancel the active generation."""
        await session_registry.cancel_generation(session_id)
