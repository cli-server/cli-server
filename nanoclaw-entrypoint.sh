#!/bin/sh
# Write .env from NANOCLAW_CONFIG_CONTENT environment variable.
# Same pattern as openclaw config injection via shell heredoc.
if [ -n "$NANOCLAW_CONFIG_CONTENT" ]; then
    echo "$NANOCLAW_CONFIG_CONTENT" > /app/.env
fi

# Write MCP config so Claude Code (spawned by NanoClaw) discovers agentserver
# tools (discover_agents, delegate_task, check_task, send_message, read_inbox).
if [ -n "$AGENTSERVER_URL" ]; then
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

# Ensure data directories exist and are writable (PVC may be mounted as root).
for dir in /app/store /app/groups /app/data; do
    mkdir -p "$dir" 2>/dev/null || true
done

exec "$@"
