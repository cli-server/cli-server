from __future__ import annotations

import logging
from collections.abc import Callable

from claude_agent_sdk.types import ClaudeAgentOptions

from app.config import settings
from app.services.transports.base import BaseSandboxTransport
from app.services.transports.docker import DockerConfig, DockerSandboxTransport
from app.services.transports.k8s import K8sSandboxTransport

logger = logging.getLogger(__name__)


def build_options(session_id: str, sandbox_name: str, *, continue_conversation: bool = False) -> ClaudeAgentOptions:
    """Build ClaudeAgentOptions for a chat session."""
    env: dict[str, str] = {}

    if settings.ANTHROPIC_API_KEY:
        env["ANTHROPIC_API_KEY"] = settings.ANTHROPIC_API_KEY
    if settings.ANTHROPIC_BASE_URL:
        env["ANTHROPIC_BASE_URL"] = settings.ANTHROPIC_BASE_URL

    options = ClaudeAgentOptions(
        permission_mode="bypassPermissions",
        env=env,
        cwd="/home/agent",
        system_prompt={"type": "preset", "name": "claude_code"},
        model=settings.MODEL or None,
        continue_conversation=continue_conversation,
    )

    return options


def create_transport_factory(
    sandbox_name: str,
    options: ClaudeAgentOptions | None = None,
) -> Callable[[], BaseSandboxTransport]:
    """Return a factory function that creates a sandbox transport
    targeting the container/pod identified by sandbox_name."""

    def factory() -> BaseSandboxTransport:
        opts = options if options is not None else build_options("", sandbox_name)
        if settings.SANDBOX_BACKEND == "k8s":
            return K8sSandboxTransport(
                sandbox_id=sandbox_name,
                options=opts,
            )
        docker_config = DockerConfig(host=None)
        return DockerSandboxTransport(
            sandbox_id=sandbox_name,
            docker_config=docker_config,
            options=opts,
        )

    return factory
