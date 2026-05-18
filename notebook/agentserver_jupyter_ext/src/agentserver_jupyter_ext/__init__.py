"""Agentserver Jupyter extensions: IdentityProvider + KernelProvisioner.

The KernelProvisioner is registered via the
`jupyter_client.kernel_provisioners` entry-point (see pyproject.toml)
as "agentserver-local" so jupyter_client 8.x can discover it by name.
"""
from .identity_provider import AgentserverIdentityProvider  # noqa: F401
from .kernel_provisioner import (  # noqa: F401
    AgentserverKernelProvisioner,
    _current_user_ctx,
)
