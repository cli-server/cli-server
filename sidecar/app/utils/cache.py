from __future__ import annotations

import redis.asyncio as aioredis

from app.config import settings


def get_redis() -> aioredis.Redis:
    """Create and return a Redis async client from settings."""
    return aioredis.from_url(
        settings.REDIS_URL,
        decode_responses=True,
    )
