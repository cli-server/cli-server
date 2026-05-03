// internal/ccbroker/tools/permission_export_test.go
package tools

import "time"

// AddPendingForTest is a test-only seam that injects a pending request into
// the gate's internal map. Used by handler tests that need to exercise the
// Resolve/CancelTurn paths without going through Check first. Compiled only
// in test builds because of the _test.go suffix.
func (g *Gate) AddPendingForTest(pid, sessionID, turnID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pending[pid] = &pendingReq{
		pid:       pid,
		sessionID: sessionID,
		turnID:    turnID,
		decided:   make(chan Decision, 1),
		deadline:  time.Now().Add(time.Hour),
	}
}
