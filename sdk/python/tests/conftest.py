"""Shared pytest fixtures for agentserver_sdk tests."""

import pytest_asyncio

from tests.stub_gateway import StubGateway


@pytest_asyncio.fixture
async def stub():
    g = StubGateway()
    await g.start()
    try:
        yield g
    finally:
        await g.stop()
