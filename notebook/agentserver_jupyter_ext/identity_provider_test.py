"""Tests for AgentserverIdentityProvider.

Run via: python -m unittest identity_provider_test.py
Requires jupyter-server installed.
"""
import asyncio
import unittest
from unittest.mock import MagicMock

from agentserver_jupyter_ext.identity_provider import AgentserverIdentityProvider
from agentserver_jupyter_ext.kernel_provisioner import _current_user_ctx


class TestAgentserverIdentityProvider(unittest.TestCase):
    def test_returns_user_from_x_forwarded_user(self):
        ip = AgentserverIdentityProvider()
        handler = MagicMock()
        handler.request.headers = {"X-Forwarded-User": "u-1"}
        user = asyncio.run(ip.get_user(handler))
        self.assertIsNotNone(user)
        self.assertEqual(user.username, "u-1")

    def test_returns_anonymous_when_header_missing(self):
        ip = AgentserverIdentityProvider()
        handler = MagicMock()
        handler.request.headers = {}
        user = asyncio.run(ip.get_user(handler))
        self.assertIsNone(user)

    def test_sets_ctxvar_for_provisioner(self):
        ip = AgentserverIdentityProvider()
        handler = MagicMock()
        handler.request.headers = {"X-Forwarded-User": "u-2"}
        # Reset ctxvar before test
        token = _current_user_ctx.set("")
        try:
            asyncio.run(ip.get_user(handler))
            # asyncio.run creates its own context, so the value set
            # inside doesn't leak out — verify via a coroutine that
            # both sets and reads in the same context.
            async def check():
                await ip.get_user(handler)
                return _current_user_ctx.get()
            got = asyncio.run(check())
            self.assertEqual(got, "u-2")
        finally:
            _current_user_ctx.reset(token)


if __name__ == "__main__":
    unittest.main()
