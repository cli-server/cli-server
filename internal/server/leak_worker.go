package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/agentserver/agentserver/internal/bridge"
)

// LeakWorkerConfig holds configuration for the leak worker.
type LeakWorkerConfig struct {
	StaleTurnAfter time.Duration // default 5m
	ResponderTTL   time.Duration // default 90s
	Period         time.Duration // default 1m
}

// LeakWorker is a background goroutine that cleans up stale active turns and responders.
type LeakWorker struct {
	s   *Server
	cfg LeakWorkerConfig
}

// NewLeakWorker creates a new LeakWorker with defaults applied to zero values.
func NewLeakWorker(s *Server, cfg LeakWorkerConfig) *LeakWorker {
	if cfg.StaleTurnAfter == 0 {
		cfg.StaleTurnAfter = 5 * time.Minute
	}
	if cfg.ResponderTTL == 0 {
		cfg.ResponderTTL = 90 * time.Second
	}
	if cfg.Period == 0 {
		cfg.Period = time.Minute
	}
	return &LeakWorker{s: s, cfg: cfg}
}

// Run starts the ticker loop until ctx is cancelled.
func (l *LeakWorker) Run(ctx context.Context) {
	t := time.NewTicker(l.cfg.Period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.RunOnce(ctx)
		}
	}
}

// RunOnce runs both cleanup sweeps once. Exposed for testing.
func (l *LeakWorker) RunOnce(ctx context.Context) {
	l.cleanStaleActiveTurns(ctx)
	l.cleanStaleResponders(ctx)
}

func (l *LeakWorker) cleanStaleActiveTurns(ctx context.Context) {
	cutoff := time.Now().Add(-l.cfg.StaleTurnAfter)
	pairs, err := l.s.DB.ListStaleActiveTurns(ctx, cutoff)
	if err != nil {
		log.Printf("leak: list stale active turns: %v", err)
		return
	}
	for _, p := range pairs {
		if l.s.CCBrokerURL == "" {
			continue
		}
		rq, _ := http.NewRequestWithContext(ctx, "GET",
			l.s.CCBrokerURL+"/api/sessions/"+p.SessionID+"/turns/active", nil)
		resp, err := http.DefaultClient.Do(rq)
		if err != nil {
			log.Printf("leak: cc-broker query session=%s: %v", p.SessionID, err)
			continue
		}
		var respBody struct {
			TurnID *string `json:"turn_id"`
		}
		json.NewDecoder(resp.Body).Decode(&respBody)
		resp.Body.Close()
		if respBody.TurnID == nil || *respBody.TurnID != p.TurnID {
			_ = l.s.DB.ClearActiveTurn(ctx, p.SessionID, p.TurnID)
			log.Printf("leak: cleared stale active_turn_id session=%s turn=%s",
				p.SessionID, p.TurnID)
		}
	}
}

func (l *LeakWorker) cleanStaleResponders(ctx context.Context) {
	cutoff := time.Now().Add(-l.cfg.ResponderTTL)
	ids, err := l.s.DB.ListStaleResponders(ctx, cutoff)
	if err != nil {
		log.Printf("leak: list stale responders: %v", err)
		return
	}
	for _, sid := range ids {
		if err := l.s.DB.ClearResponder(ctx, sid); err != nil {
			log.Printf("leak: clear responder %s: %v", sid, err)
			continue
		}
		log.Printf("leak: cleared stale responder for session=%s", sid)
		if l.s.BridgeHandler != nil && l.s.BridgeHandler.SSE != nil {
			l.s.BridgeHandler.SSE.Publish(sid, &bridge.StreamClientEvent{
				EventType: "permission_responder_lost",
				Payload:   []byte(`{"reason":"ttl_expired"}`),
			})
		}
	}
}
