// Package notebooksupervisor manages per-workspace Jupyter Server
// Deployments + Services in Kubernetes. EnsureRunning spawns on first
// call, returns a cached handle on subsequent calls. Touch refreshes
// lastActive so idle workspaces get reaped after Config.IdleTTL.
package notebooksupervisor
