from __future__ import annotations

import logging
from contextlib import asynccontextmanager
from typing import Any

import redis.asyncio as aioredis
from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware

from app.config import settings
from app.db import dispose_engine, get_engine
from app.services.chat import ChatService
from app.services.session_registry import SessionRegistry, session_registry
from app.services.streaming.runtime import ChatStreamRuntime

logger = logging.getLogger(__name__)


@asynccontextmanager
async def lifespan(app: FastAPI):
    # Startup
    engine = get_engine()
    logger.info("Database engine initialised")

    redis = aioredis.from_url(settings.REDIS_URL, decode_responses=True)
    logger.info("Redis connection established")

    runtime = ChatStreamRuntime(engine=engine, redis=redis, session_registry=session_registry)
    chat_service = ChatService(engine=engine, redis=redis, runtime=runtime)

    app.state.engine = engine
    app.state.redis = redis
    app.state.session_registry = session_registry
    app.state.runtime = runtime
    app.state.chat_service = chat_service

    yield

    # Shutdown
    await session_registry.terminate_all()
    await redis.aclose()
    await dispose_engine()
    logger.info("Shutdown complete")


app = FastAPI(title="CLI Server Sidecar", lifespan=lifespan)

app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)

from app.routes import router  # noqa: E402

app.include_router(router)
