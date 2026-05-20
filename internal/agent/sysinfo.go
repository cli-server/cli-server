// Package agent holds the small bits of state shared between the
// surviving agentserver-agent subcommands (mcp-server, version) after
// the stateless-cc removal. Originally a large package powering the
// claudecode/executor/tui modes, now collapsed to a single Version
// constant.
package agent

// Version is set at build time via -ldflags.
var Version = "dev"
