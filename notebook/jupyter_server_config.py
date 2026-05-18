"""Plan 3b: plug AgentserverIdentityProvider + AgentserverKernelProvisioner.
Provisioner registered via entry-point in agentserver_jupyter_ext.
"""
import os

c = get_config()  # type: ignore[name-defined]  # noqa: F821

c.ServerApp.ip = "0.0.0.0"
c.ServerApp.port = 8888
c.ServerApp.open_browser = False
c.ServerApp.disable_check_xsrf = True
c.ServerApp.allow_origin = "*"
c.ServerApp.root_dir = "/workspace"
c.ServerApp.allow_root = True
c.ServerApp.base_url = os.environ.get("NOTEBOOK_BASE_URL", "/")

# Auth — trust X-Forwarded-User from agentserver web proxy.
# Class resolves via the pip-installed agentserver_jupyter_ext package.
c.ServerApp.identity_provider_class = "agentserver_jupyter_ext.identity_provider.AgentserverIdentityProvider"

# Kernel provisioner — registered as entry-point "agentserver-local"
# via agentserver_jupyter_ext/pyproject.toml.
c.MultiKernelManager.default_kernel_provisioner_name = "agentserver-local"
