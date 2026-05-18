// Package oplog publishes per-call operation records from codex-app-gateway
// to agentserver's /internal/operations POST endpoint. Submit is async and
// fire-and-forget; tool calls never block on log delivery.
package oplog
