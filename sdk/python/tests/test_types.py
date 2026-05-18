from agentserver_sdk.types import OperationRecord, ShellResult, ToolMetadata


def test_shell_result_from_mcp_text_content():
    raw = {
        "content": [{"type": "text", "text": "hi"}],
        "structuredContent": {"stdout": "hi", "stderr": "", "exit_code": 0},
        "isError": False,
    }
    r = ShellResult.from_mcp(raw)
    assert r.stdout == "hi"
    assert r.stderr == ""
    assert r.exit_code == 0


def test_shell_result_from_mcp_fallback_when_no_structured():
    raw = {"content": [{"type": "text", "text": "fallback"}], "isError": False}
    r = ShellResult.from_mcp(raw)
    assert r.stdout == "fallback"
    assert r.stderr == ""
    assert r.exit_code == 0


def test_shell_result_exit_code_nonzero():
    raw = {
        "content": [{"type": "text", "text": ""}],
        "structuredContent": {"stdout": "", "stderr": "boom", "exit_code": 1},
        "isError": False,
    }
    r = ShellResult.from_mcp(raw)
    assert r.exit_code == 1
    assert r.stderr == "boom"


def test_tool_metadata_from_dict():
    m = ToolMetadata.from_dict(
        {
            "name": "submit_task",
            "description": "submit HPC job",
            "inputSchema": {"type": "object"},
        }
    )
    assert m.name == "submit_task"
    assert m.description == "submit HPC job"
    assert m.kind == "custom"  # default for non-core


def test_tool_metadata_core_marker():
    m = ToolMetadata.from_dict({"name": "shell", "description": "x", "inputSchema": {}})
    assert m.kind == "core"


def test_operation_record_from_dict():
    o = OperationRecord.from_dict(
        {
            "id": "op_1",
            "env_id": "alpha",
            "tool": "shell",
            "is_error": False,
            "started_at": "2026-05-18T10:00:00Z",
            "duration_ms": 42,
            "user_id": "u",
            "source": "sdk",
        }
    )
    assert o.id == "op_1"
    assert o.is_error is False
    assert o.duration_ms == 42


def test_operation_record_from_dict_minimal():
    """Optional fields (user_id, arguments, result_summary) default to None;
    is_error / duration_ms default to safe values when missing."""
    o = OperationRecord.from_dict({"id": "op_2", "env_id": "a", "tool": "shell"})
    assert o.id == "op_2"
    assert o.env_id == "a"
    assert o.tool == "shell"
    assert o.user_id is None
    assert o.arguments is None
    assert o.result_summary is None
    assert o.is_error is False
    assert o.duration_ms == 0
    assert o.source == ""
    assert o.started_at == ""
