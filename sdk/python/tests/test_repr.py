from agentserver_sdk._repr import envs_table_html
from agentserver_sdk.env import Env
from agentserver_sdk.errors import ToolError
from agentserver_sdk.types import ShellResult, ToolMetadata


class _FakeClient:
    pass


def _env(name, tools):
    return Env(
        name=name,
        type="shell",
        tools=[
            ToolMetadata(
                name=t, description="", input_schema={}, kind="core" if t in {"shell"} else "custom"
            )
            for t in tools
        ],
        _client=_FakeClient(),
    )


def test_env_repr_html_contains_name_and_tool_count():
    e = _env("alpha", ["shell", "submit_task"])
    html = e._repr_html_()
    assert "alpha" in html
    assert "2" in html  # tool count


def test_envs_table_html_has_one_row_per_env():
    envs = [_env("alpha", ["shell"]), _env("beta", ["shell", "submit_task"])]
    html = envs_table_html(envs)
    assert html.count("<tr") >= 3  # header + 2 envs
    assert "alpha" in html
    assert "beta" in html


def test_shell_result_repr_html_shows_exit_code():
    r = ShellResult(stdout="hi", stderr="", exit_code=0)
    assert "0" in r._repr_html_()
    assert "hi" in r._repr_html_()
    r2 = ShellResult(stdout="", stderr="boom", exit_code=1)
    assert "boom" in r2._repr_html_()


def test_tool_error_repr_html():
    e = ToolError(tool="shell", env="alpha", message="bad", raw={"isError": True})
    html = e._repr_html_()
    assert "alpha" in html
    assert "shell" in html
    assert "bad" in html
