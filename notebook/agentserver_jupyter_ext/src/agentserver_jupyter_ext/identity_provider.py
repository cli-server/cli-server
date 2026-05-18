"""Trust X-Forwarded-User from the agentserver web proxy.

The proxy validates a short-lived HMAC JWT before forwarding, so by the
time the request reaches Jupyter the header is trusted. Any request
arriving WITHOUT X-Forwarded-User is rejected (None return) — this
makes mis-deployment fail closed.

Sets a ContextVar (defined in kernel_provisioner) so the kernel
provisioner can inject AGENTSERVER_USER_ID into spawned kernels.
"""
from __future__ import annotations

from jupyter_server.auth import IdentityProvider, User

from .kernel_provisioner import _current_user_ctx


class AgentserverIdentityProvider(IdentityProvider):
    """Reads X-Forwarded-User; returns None for unauthenticated requests."""

    async def get_user(self, handler):
        username = handler.request.headers.get("X-Forwarded-User", "")
        if not username:
            return None
        _current_user_ctx.set(username)
        return User(
            username=username,
            name=username,
            display_name=username,
            initials=username[:2].upper() if username else "",
            color=None,
            avatar_url=None,
        )
