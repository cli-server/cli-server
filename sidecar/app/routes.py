from __future__ import annotations

import logging
from typing import Any

from fastapi import APIRouter, Header, Query, Request
from fastapi.responses import JSONResponse
from sse_starlette.sse import EventSourceResponse
from starlette.responses import Response

from app.services.chat import ChatService

logger = logging.getLogger(__name__)
router = APIRouter()


@router.get("/health")
async def health() -> dict[str, str]:
    return {"status": "ok"}


def _get_chat_service(request: Request) -> ChatService:
    return request.app.state.chat_service


@router.post("/chat")
async def post_chat(
    request: Request,
    x_session_id: str = Header(..., alias="X-Session-ID"),
    x_sandbox_name: str = Header("", alias="X-Sandbox-Name"),
) -> JSONResponse:
    body: dict[str, Any] = await request.json()
    prompt: str = body.get("prompt", "")
    if not prompt:
        return JSONResponse({"error": "prompt is required"}, status_code=400)

    chat_service = _get_chat_service(request)
    result = await chat_service.initiate_chat_completion(
        session_id=x_session_id,
        sandbox_name=x_sandbox_name,
        prompt=prompt,
    )
    return JSONResponse(result)


@router.get("/stream/{session_id}")
async def get_stream(
    request: Request,
    session_id: str,
    after_seq: int = Query(0),
) -> EventSourceResponse:
    chat_service = _get_chat_service(request)
    event_generator = chat_service.create_event_stream(
        session_id=session_id,
        after_seq=after_seq,
    )
    return EventSourceResponse(event_generator, media_type="text/event-stream")


@router.delete("/stream/{session_id}")
async def delete_stream(
    request: Request,
    session_id: str,
) -> Response:
    chat_service = _get_chat_service(request)
    await chat_service.stop_stream(session_id=session_id)
    return Response(status_code=204)
