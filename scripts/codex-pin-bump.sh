#!/usr/bin/env bash
# Bump the upstream codex pin to a new tag.
# Usage: scripts/codex-pin-bump.sh rust-v0.132.0-alpha.1
set -euo pipefail

TAG="${1:?usage: $0 <upstream-tag>}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "Cloning openai/codex@$TAG..." >&2
git clone --depth 1 --branch "$TAG" https://github.com/openai/codex.git "$tmp/codex" >&2

upstream_sha="$(cd "$tmp/codex" && git rev-parse HEAD)"

sha_of() { sha256sum "$1" | awk '{print $1}'; }

relay_proto_upstream="$tmp/codex/codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto"
relay_sha="$(sha_of "$relay_proto_upstream")"
v1_sha="$(sha_of "$tmp/codex/codex-rs/app-server-protocol/src/protocol/v1.rs")"
item_sha="$(sha_of "$tmp/codex/codex-rs/app-server-protocol/src/protocol/v2/item.rs")"
mcp_sha="$(sha_of "$tmp/codex/codex-rs/app-server-protocol/src/protocol/v2/mcp.rs")"

# Overwrite our relay.proto with upstream content + inject `option go_package` directive.
# The directive goes immediately after the `package ...;` line (matching the existing layout).
{
  awk '
    /^package codex\.exec_server\.relay\.v1;$/ {
      print
      print ""
      print "option go_package = \"github.com/agentserver/agentserver/internal/relaypb;relaypb\";"
      next
    }
    { print }
  ' "$relay_proto_upstream"
} > "$REPO_ROOT/internal/relaypb/relay.proto"

# Compute normalized sha (must match what verify.go's normalize() produces).
# normalize() does:
#   1. Convert CRLF to LF
#   2. Strip lines matching: ^option go_package[[:space:]]*=.*;\s*$
#   3. Replace runs of 2+ newlines with exactly 2 newlines (one blank line)
normalized_sha="$(
  cat "$REPO_ROOT/internal/relaypb/relay.proto" | \
  tr -d '\r' | \
  sed -E '/^option[[:space:]]+go_package[[:space:]]*=.*;[[:space:]]*$/d' | \
  perl -0777 -pe 's/\n{2,}/\n\n/g' | \
  sha256sum | awk '{print $1}'
)"

cat > "$REPO_ROOT/codex-pin.json" <<EOF
{
  "upstream_repo": "openai/codex",
  "tag": "$TAG",
  "sha": "$upstream_sha",
  "tracked_files": {
    "codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto": "$relay_sha",
    "codex-rs/app-server-protocol/src/protocol/v1.rs": "$v1_sha",
    "codex-rs/app-server-protocol/src/protocol/v2/item.rs": "$item_sha",
    "codex-rs/app-server-protocol/src/protocol/v2/mcp.rs": "$mcp_sha"
  },
  "normalized_equivalent_files": {
    "internal/relaypb/relay.proto": {
      "upstream_path": "codex-rs/exec-server/src/proto/codex.exec_server.relay.v1.proto",
      "normalized_sha256": "$normalized_sha",
      "comment": "Strip Go-only \`option go_package = \"...\";\` directive and collapse runs of 2+ consecutive newlines to one blank line, then compare schemas. Required because agentserver's protoc-gen-go needs this directive; upstream's prost (Rust) doesn't. Must stay in sync with cmd/check-codex-pin/verify.go::normalize()."
    }
  },
  "approval_methods": [
    "item/commandExecution/requestApproval",
    "item/fileChange/requestApproval",
    "item/permissions/requestApproval",
    "item/tool/requestUserInput",
    "mcpServer/elicitation/request"
  ]
}
EOF

echo "Bumped codex-pin.json and internal/relaypb/relay.proto to $TAG." >&2
echo "Next: run 'make codex-pin-check' to verify the new pin." >&2
