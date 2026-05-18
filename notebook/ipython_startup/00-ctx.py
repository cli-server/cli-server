"""Auto-injected into every ipykernel session inside the notebook image.

Exposes:
  - ctx: lazy Ctx instance bound to AGENTSERVER_* env vars
  - asyncio: convenience re-import so users don't need `import asyncio`
"""
import asyncio  # noqa: F401  (intentionally injected into kernel namespace)
from agentserver_sdk import Ctx

ctx = Ctx.from_env()
