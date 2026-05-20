package codexexecgateway

import (
	"context"
	"time"

	sdkpkg "github.com/agentserver/agentserver/internal/codexexecgateway/sdk"
)

// sdkConnectedAdapter bridges the gateway's existing *Store + *ConnRegistry
// into the sdk.ConnectedLister interface.  It is constructed once in
// NewServer and embedded in the sdk.Server struct field Registry.
//
// Implementation mirrors handlers.Connected but returns the slice directly
// instead of writing JSON, eliminating an HTTP round-trip.
type sdkConnectedAdapter struct {
	store    *Store
	registry *ConnRegistry
}

// Connected satisfies sdk.ConnectedLister.  It returns the intersection of
// (workspace's bound executors) ∩ (currently-connected exe_ids) in the shape
// the SDK package expects.
func (a sdkConnectedAdapter) Connected(ctx context.Context, wsID string) ([]sdkpkg.ConnectedExecutor, error) {
	ids := a.registry.ConnectedIDs()
	rows, err := a.store.ConnectedExecutorsForWorkspace(ctx, wsID, ids)
	if err != nil {
		return nil, err
	}
	out := make([]sdkpkg.ConnectedExecutor, 0, len(rows))
	for _, e := range rows {
		var lastSeen string
		if e.LastSeenAt != nil {
			lastSeen = e.LastSeenAt.UTC().Format(time.RFC3339)
		}
		out = append(out, sdkpkg.ConnectedExecutor{
			ExeID:      e.ExeID,
			Name:       e.Name,
			IsDefault:  e.IsDefault,
			LastSeenAt: lastSeen,
		})
	}
	return out, nil
}

// ListWorkspaceWithLive returns every executor bound to wsID with IsOnline
// set from the live in-memory ConnRegistry — distinct from the SDK adapter
// above, which only returns the intersection (codex env-mcp callers want
// online-only). The /workspaces/{wsID}/executors UI endpoint uses this so
// offline executors stay visible in the Connectors table with their last
// disconnect time.
func (a sdkConnectedAdapter) ListWorkspaceWithLive(ctx context.Context, wsID string) ([]ConnectedExecutor, error) {
	rows, err := a.store.ListWorkspaceExecutors(ctx, wsID)
	if err != nil {
		return nil, err
	}
	connSet := make(map[string]struct{})
	for _, id := range a.registry.ConnectedIDs() {
		connSet[id] = struct{}{}
	}
	for i := range rows {
		_, ok := connSet[rows[i].ExeID]
		rows[i].IsOnline = ok
	}
	return rows, nil
}
