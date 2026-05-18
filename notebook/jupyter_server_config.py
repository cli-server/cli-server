"""Plan 1 minimum config — no IdentityProvider or KernelProvisioner yet
(those land with notebook hosting plan). Disable jupyter's own token
auth so the smoke run is just-open-and-go. Real deployment will plug
in agentserver auth in Plan 3."""

c = get_config()  # type: ignore[name-defined]  # noqa: F821 (provided by jupyter at runtime)

c.ServerApp.ip = "0.0.0.0"
c.ServerApp.port = 8888
c.ServerApp.open_browser = False
c.ServerApp.token = ""       # SECURITY: only safe inside the local smoke env / agentserver-proxied prod
c.ServerApp.password = ""
c.ServerApp.disable_check_xsrf = True
c.ServerApp.allow_origin = "*"
c.ServerApp.root_dir = "/workspace"
c.ServerApp.allow_root = True
