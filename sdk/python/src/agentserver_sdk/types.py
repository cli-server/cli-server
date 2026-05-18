"""SDK result + metadata types. Pure data; no I/O.

`from_mcp` / `from_dict` constructors take the JSON-shape returned by the
gateway and produce typed Python objects. Wrappers are minimal — most
fields are exposed as-is.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

CORE_TOOLS = frozenset(
    {
        "shell",
        "read_file",
        "write_file",
        "apply_patch",
        "exec_command",
        "write_stdin",
        "read_output",
        "terminate",
        "copy_path",
    }
)


@dataclass
class ShellResult:
    stdout: str
    stderr: str
    exit_code: int
    raw: dict[str, Any] = field(default_factory=dict)

    @classmethod
    def from_mcp(cls, raw: dict[str, Any]) -> ShellResult:
        sc = raw.get("structuredContent") or {}
        if sc:
            return cls(
                stdout=sc.get("stdout", ""),
                stderr=sc.get("stderr", ""),
                exit_code=int(sc.get("exit_code", 0)),
                raw=raw,
            )
        # Fallback: join text content as stdout
        text = "".join(
            item.get("text", "") for item in raw.get("content", []) if item.get("type") == "text"
        )
        return cls(stdout=text, stderr="", exit_code=0, raw=raw)

    def _repr_html_(self) -> str:
        import html as _html

        colour = "green" if self.exit_code == 0 else "red"
        return (
            f"<div>exit_code: <b style='color:{colour}'>{self.exit_code}</b></div>"
            f"<details open><summary>stdout</summary>"
            f"<pre>{_html.escape(self.stdout)}</pre></details>"
            + (
                f"<details><summary>stderr</summary><pre>{_html.escape(self.stderr)}</pre></details>"
                if self.stderr
                else ""
            )
        )


@dataclass
class ToolMetadata:
    name: str
    description: str
    input_schema: dict[str, Any]
    kind: str  # "core" | "custom"

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> ToolMetadata:
        name = d["name"]
        return cls(
            name=name,
            description=d.get("description", ""),
            input_schema=d.get("inputSchema", {}),
            kind="core" if name in CORE_TOOLS else "custom",
        )


@dataclass
class OperationRecord:
    id: str
    env_id: str
    tool: str
    is_error: bool
    started_at: str
    duration_ms: int
    user_id: str | None
    source: str
    arguments: dict[str, Any] | None = None
    result_summary: str | None = None

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> OperationRecord:
        return cls(
            id=d["id"],
            env_id=d["env_id"],
            tool=d["tool"],
            is_error=bool(d.get("is_error", False)),
            started_at=d.get("started_at", ""),
            duration_ms=int(d.get("duration_ms", 0)),
            user_id=d.get("user_id"),
            source=d.get("source", ""),
            arguments=d.get("arguments"),
            result_summary=d.get("result_summary"),
        )
