#!/bin/bash
set -e

# Write MCP config so Claude Code discovers agentserver tools
# (discover_agents, delegate_task, check_task)
if [ -n "$AGENTSERVER_URL" ]; then
    mkdir -p "$HOME/.claude"
    cat > "$HOME/.mcp.json" <<MCPEOF
{
  "mcpServers": {
    "agentserver": {
      "command": "/usr/local/bin/agentserver",
      "args": ["mcp-server"],
      "env": {
        "AGENTSERVER_URL": "${AGENTSERVER_URL}",
        "AGENTSERVER_TOKEN": "${AGENTSERVER_TOKEN}",
        "AGENTSERVER_WORKSPACE_ID": "${AGENTSERVER_WORKSPACE_ID}",
        "AGENTSERVER_SANDBOX_ID": "${AGENTSERVER_SANDBOX_ID}"
      }
    }
  }
}
MCPEOF
fi

# ttyd wraps claude CLI in a web terminal accessible via HTTP/WebSocket
exec ttyd \
    --writable \
    --port 7681 \
    --base-path / \
    -t fontSize=14 \
    -t fontFamily="'Menlo','Monaco','Courier New',monospace" \
    claude
