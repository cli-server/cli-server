"""Shared pytest fixtures for agentserver_sdk tests."""

import httpx
import pytest_asyncio

from agentserver_sdk.client import HTTPClient

from .stub_rest import StubRest


@pytest_asyncio.fixture
async def stub_client():
    """Yield (HTTPClient, StubRest) wired together with httpx ASGITransport.

    The stub starts empty; tests `.register(method, path, handler)` the
    routes they need.
    """
    stub = StubRest()
    client = HTTPClient("http://stub", "test-token")
    # Swap the AsyncClient out for one backed by the ASGI stub.
    await client._http.aclose()
    client._http = httpx.AsyncClient(
        transport=httpx.ASGITransport(app=stub),
        base_url="http://stub",
        headers={"Authorization": "Bearer test-token"},
    )
    yield client, stub
    await client.close()
