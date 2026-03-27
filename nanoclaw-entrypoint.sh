#!/bin/sh
# Write .env from NANOCLAW_CONFIG_CONTENT environment variable.
# Same pattern as openclaw config injection via shell heredoc.
if [ -n "$NANOCLAW_CONFIG_CONTENT" ]; then
    echo "$NANOCLAW_CONFIG_CONTENT" > /app/.env
fi

# Ensure data directories exist and are writable (PVC may be mounted as root).
for dir in /app/store /app/groups /app/data; do
    mkdir -p "$dir" 2>/dev/null || true
done

exec "$@"
