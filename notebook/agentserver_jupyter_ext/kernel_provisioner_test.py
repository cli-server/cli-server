"""Tests for AgentserverKernelProvisioner."""
import asyncio
import unittest

from agentserver_jupyter_ext.kernel_provisioner import (
    AgentserverKernelProvisioner,
    _current_user_ctx,
)


class TestAgentserverKernelProvisioner(unittest.TestCase):
    def test_pre_launch_injects_user_id(self):
        prov = AgentserverKernelProvisioner()
        token = _current_user_ctx.set("u-42")
        try:
            env = {"FOO": "bar"}
            asyncio.run(prov._apply_user_env(env))
            self.assertEqual(env["AGENTSERVER_USER_ID"], "u-42")
            self.assertEqual(env["FOO"], "bar")
        finally:
            _current_user_ctx.reset(token)

    def test_pre_launch_passthrough_when_no_user(self):
        prov = AgentserverKernelProvisioner()
        env = {"X": "y"}
        asyncio.run(prov._apply_user_env(env))
        # No user set → AGENTSERVER_USER_ID stays unset
        self.assertNotIn("AGENTSERVER_USER_ID", env)


if __name__ == "__main__":
    unittest.main()
