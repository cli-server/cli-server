package imbridge

import (
	"context"
	"log"
	"strings"
	"time"
)

const (
	matrixSyncTimeoutSec     = 30
	matrixTypingTimeoutMs    = 10000
	matrixTypingKeepalive    = 5 * time.Second
	matrixTypingTotalTimeout = 5 * time.Minute
)

// MatrixProvider implements Provider and TypingProvider for the Matrix protocol.
type MatrixProvider struct{}

func (p *MatrixProvider) Name() string      { return "matrix" }
func (p *MatrixProvider) JIDSuffix() string { return "@matrix" }

func (p *MatrixProvider) Poll(ctx context.Context, creds *Credentials, cursor string) (*PollResult, error) {
	// On initial sync (no cursor), use timeout=0 for an immediate response to get next_batch.
	// On subsequent syncs, use long-polling with a 30s timeout.
	timeoutSec := matrixSyncTimeoutSec
	isInitial := cursor == ""
	if isInitial {
		timeoutSec = 0
	}

	matrixMsgs, nextBatch, err := MatrixSync(ctx, creds.BaseURL, creds.BotToken, creds.BotID, cursor, timeoutSec)
	if err != nil {
		return nil, err
	}

	// Skip all messages from the initial sync (they are historical).
	if isInitial {
		return &PollResult{NewCursor: nextBatch}, nil
	}

	var msgs []InboundMessage
	for _, m := range matrixMsgs {
		msgs = append(msgs, InboundMessage{
			FromUserID: m.RoomID + "@matrix",
			SenderName: m.SenderID,
			Text:       m.Text,
			IsGroup:    true,
			Metadata: map[string]string{
				"room_id":  m.RoomID,
				"event_id": m.EventID,
			},
		})
	}

	return &PollResult{Messages: msgs, NewCursor: nextBatch}, nil
}

func (p *MatrixProvider) Send(ctx context.Context, creds *Credentials, toUserID, text string, meta map[string]string) error {
	roomID := strings.TrimSuffix(toUserID, "@matrix")
	return MatrixSendText(ctx, creds.BaseURL, creds.BotToken, roomID, text)
}

// StartTyping implements TypingProvider for Matrix.
// It sends typing indicators every 5s until cancelled or timed out (5min).
// On timeout, it sends an error notice to the user and stops.
func (p *MatrixProvider) StartTyping(ctx context.Context, creds *Credentials, userID string, meta map[string]string,
	sendError func(text string)) (cancel func()) {

	ctx, cancelFn := context.WithTimeout(ctx, matrixTypingTotalTimeout)

	go func() {
		defer cancelFn()

		roomID := strings.TrimSuffix(userID, "@matrix")

		// Send initial typing indicator.
		if err := MatrixSendTyping(ctx, creds.BaseURL, creds.BotToken, creds.BotID, roomID, true, matrixTypingTimeoutMs); err != nil {
			log.Printf("imbridge: matrix sendTyping failed for %s: %v", roomID, err)
		}

		// Keepalive loop: send typing every 5s until cancelled or timed out.
		ticker := time.NewTicker(matrixTypingKeepalive)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				// Send typing=false (best-effort, use background context).
				bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = MatrixSendTyping(bgCtx, creds.BaseURL, creds.BotToken, creds.BotID, roomID, false, 0)
				bgCancel()

				if ctx.Err() == context.DeadlineExceeded {
					sendError("\u26a0\ufe0f Message processing timed out. Please try again later.")
				}
				return
			case <-ticker.C:
				if err := MatrixSendTyping(ctx, creds.BaseURL, creds.BotToken, creds.BotID, roomID, true, matrixTypingTimeoutMs); err != nil {
					log.Printf("imbridge: matrix typing keepalive failed for %s: %v", roomID, err)
				}
			}
		}
	}()

	return cancelFn
}
