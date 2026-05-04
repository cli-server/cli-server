package tui

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// SSEEvent is one parsed Server-Sent Event frame.
type SSEEvent struct {
	Type        string
	Data        []byte
	LastEventID string
}

// SSEConfig configures the consumer's connection + reconnect behavior.
type SSEConfig struct {
	SessionID      string
	InitialBackoff time.Duration // default 1s
	MaxBackoff     time.Duration // default 30s
	HTTP           *http.Client  // optional; defaults to no timeout (SSE is long-lived)
}

// SSEConsumer streams events from the agentserver SSE endpoint with
// automatic reconnection. The server-side replay mechanism (Last-Event-ID)
// resumes from the last received event after a transient failure.
type SSEConsumer struct {
	bus    *Bus
	cfg    SSEConfig
	lastID string
}

func NewSSEConsumer(bus *Bus, cfg SSEConfig) *SSEConsumer {
	if cfg.InitialBackoff == 0 {
		cfg.InitialBackoff = time.Second
	}
	if cfg.MaxBackoff == 0 {
		cfg.MaxBackoff = 30 * time.Second
	}
	return &SSEConsumer{bus: bus, cfg: cfg}
}

// Run starts the consumer. The returned channel emits one event per parsed
// frame. The channel closes when ctx is cancelled. Reconnection is automatic
// and transparent to the consumer.
func (s *SSEConsumer) Run(ctx context.Context) <-chan SSEEvent {
	out := make(chan SSEEvent, 64)
	go func() {
		defer close(out)
		backoff := s.cfg.InitialBackoff
		for {
			if ctx.Err() != nil {
				return
			}
			connectedAt := time.Now()
			err := s.connectOnce(ctx, out)
			if ctx.Err() != nil {
				return
			}
			_ = err
			// If we stayed connected for a while, reset backoff so the next
			// transient failure doesn't immediately wait the maximum.
			if time.Since(connectedAt) > 30*time.Second {
				backoff = s.cfg.InitialBackoff
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > s.cfg.MaxBackoff {
				backoff = s.cfg.MaxBackoff
			}
		}
	}()
	return out
}

func (s *SSEConsumer) connectOnce(ctx context.Context, out chan<- SSEEvent) error {
	tk, err := s.bus.AccessToken(ctx)
	if err != nil {
		return err
	}
	url := s.bus.ServerURL() + "/api/agents/sessions/" + s.cfg.SessionID + "/events"
	// First connect: pull a backlog so events emitted between session
	// creation and SSE subscribe (cc-broker is async — a fast turn can
	// finish before the consumer is wired up) are not lost. Reconnects
	// use Last-Event-ID instead.
	if s.lastID == "" {
		url += "?tail=500"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+tk)
	if s.lastID != "" {
		req.Header.Set("Last-Event-ID", s.lastID)
	}
	client := s.cfg.HTTP
	if client == nil {
		// SSE is long-lived; no Timeout. Cancellation comes from ctx.
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sse status %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var (
		ev   SSEEvent
		data []byte
	)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			// End of event.
			if ev.Type != "" || len(data) > 0 {
				ev.Data = data
				if ev.LastEventID != "" {
					s.lastID = ev.LastEventID
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			ev = SSEEvent{}
			data = nil
		case strings.HasPrefix(line, ":"):
			// Comment / keepalive — ignore.
		case strings.HasPrefix(line, "event: "):
			ev.Type = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			ev.LastEventID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, []byte(strings.TrimPrefix(line, "data: "))...)
		case strings.HasPrefix(line, "retry: "):
			// Server-suggested retry duration — v1 ignores; we use backoff config.
		}
	}
	return sc.Err()
}
