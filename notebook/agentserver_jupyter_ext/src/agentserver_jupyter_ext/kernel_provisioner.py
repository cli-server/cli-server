"""KernelProvisioner that stamps AGENTSERVER_USER_ID into kernel env.

Reads the user from a ContextVar set upstream by the IdentityProvider
(per-request). Jupyter's kernel-start path runs inside that request
context, so the ContextVar resolves the per-call user even with shared
KernelProvisioner instances.
"""
from __future__ import annotations

import contextvars
from typing import Any

from jupyter_client.provisioning import LocalProvisioner


# Set by the request-scope IdentityProvider in identity_provider.py.
# Default empty string means "no user attribution" → no env injected.
_current_user_ctx: contextvars.ContextVar[str] = contextvars.ContextVar(
    "agentserver_current_user", default=""
)


class AgentserverKernelProvisioner(LocalProvisioner):
    """Wraps LocalProvisioner; injects AGENTSERVER_USER_ID into kernel env."""

    async def pre_launch(self, **kwargs: Any) -> dict[str, Any]:
        result = await super().pre_launch(**kwargs)
        env = result.get("env", {})
        await self._apply_user_env(env)
        result["env"] = env
        return result

    async def _apply_user_env(self, env: dict[str, str]) -> None:
        user = _current_user_ctx.get()
        if user:
            env["AGENTSERVER_USER_ID"] = user
