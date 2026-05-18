"""Jupyter-friendly HTML renderers. Pure functions, no I/O."""

from __future__ import annotations

import html
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from .env import Env


def envs_table_html(envs: list[Env]) -> str:
    rows = [
        "<tr><th align='left'>name</th><th align='left'>type</th><th align='left'>tools</th></tr>"
    ]
    for e in envs:
        rows.append(
            "<tr>"
            f"<td><code>{html.escape(e.name)}</code></td>"
            f"<td>{html.escape(e.type)}</td>"
            f"<td>{len(e.tools)}</td>"
            "</tr>"
        )
    return f"<table>{''.join(rows)}</table>"
