package imbridge

import (
	"context"
	"log"
	"os"
	"strings"
	"time"
)

const (
	matrixSyncTimeoutSec     = 30
	matrixTypingTimeoutMs    = 10000
	matrixTypingKeepalive    = 5 * time.Second
	matrixTypingTotalTimeout = 5 * time.Minute
)

// MatrixProvider implements Provider, TypingProvider, ImageSendProvider,
// ConfigurableProvider, and DisconnectProvider for the Matrix protocol.
type MatrixProvider struct {
	CryptoManager *MatrixCryptoManager
}

func (p *MatrixProvider) Name() string      { return "matrix" }
func (p *MatrixProvider) JIDSuffix() string { return "@matrix" }

// InitProvider implements InitializableProvider — sets up the E2EE crypto manager.
func (p *MatrixProvider) InitProvider(dbURL string) error {
	encKey := []byte(os.Getenv("MATRIX_ENCRYPTION_KEY"))
	if len(encKey) == 0 {
		encKey = []byte("agentserver-matrix-default-key-01")
	}
	p.CryptoManager = NewMatrixCryptoManager(dbURL, encKey)
	return nil
}

// ValidateCredentials implements ConfigurableProvider.
// Calls Matrix /whoami and initializes E2EE if CryptoManager is set.
// The token parameter is the access token; baseURL is the homeserver URL.
// options may contain "recovery_key" for E2EE self-verification.
func (p *MatrixProvider) ValidateCredentials(ctx context.Context, baseURL, token string) (string, error) {
	return MatrixWhoami(ctx, baseURL, token)
}

// ConfigureE2EE initializes E2EE with the given recovery key for self-verification.
func (p *MatrixProvider) ConfigureE2EE(ctx context.Context, creds *Credentials, recoveryKey string) error {
	if p.CryptoManager == nil {
		return nil
	}
	_, err := p.CryptoManager.GetOrCreate(ctx, creds, recoveryKey)
	return err
}

// Disconnect implements DisconnectProvider — closes the E2EE client when a binding is disconnected.
func (p *MatrixProvider) Disconnect(sandboxID, botID string) {
	if p.CryptoManager != nil {
		p.CryptoManager.Remove(sandboxID, botID)
	}
}

func (p *MatrixProvider) Poll(ctx context.Context, creds *Credentials, cursor string) (*PollResult, error) {
	timeoutSec := matrixSyncTimeoutSec
	isInitial := cursor == ""
	if isInitial {
		timeoutSec = 0
	}

	// Use E2EE crypto client if available.
	if p.CryptoManager != nil {
		return p.pollWithCrypto(ctx, creds, cursor, timeoutSec, isInitial)
	}

	// Fallback to non-E2EE polling.
	matrixMsgs, nextBatch, err := MatrixSync(ctx, creds.BaseURL, creds.BotToken, creds.BotID, cursor, timeoutSec)
	if err != nil {
		return nil, err
	}

	if isInitial {
		return &PollResult{NewCursor: nextBatch}, nil
	}

	return &PollResult{Messages: matrixMsgsToInbound(matrixMsgs), NewCursor: nextBatch}, nil
}

func (p *MatrixProvider) pollWithCrypto(ctx context.Context, creds *Credentials, cursor string, timeoutSec int, isInitial bool) (*PollResult, error) {
	cc, err := p.CryptoManager.GetOrCreate(ctx, creds, "")
	if err != nil {
		return nil, err
	}

	matrixMsgs, nextBatch, err := cc.SyncAndDecrypt(ctx, creds.BotID, cursor, timeoutSec)
	if err != nil {
		return nil, err
	}

	if isInitial {
		return &PollResult{NewCursor: nextBatch}, nil
	}

	return &PollResult{Messages: matrixMsgsToInbound(matrixMsgs), NewCursor: nextBatch}, nil
}

func matrixMsgsToInbound(matrixMsgs []MatrixMessage) []InboundMessage {
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
	return msgs
}

func (p *MatrixProvider) Send(ctx context.Context, creds *Credentials, toUserID, text string, meta map[string]string) error {
	roomID := strings.TrimSuffix(toUserID, "@matrix")

	if p.CryptoManager != nil {
		cc, err := p.CryptoManager.GetOrCreate(ctx, creds, "")
		if err != nil {
			return err
		}
		return cc.SendText(ctx, roomID, text)
	}

	return MatrixSendText(ctx, creds.BaseURL, creds.BotToken, roomID, text)
}

// SendImage implements ImageSendProvider for Matrix.
func (p *MatrixProvider) SendImage(ctx context.Context, creds *Credentials, toUserID string, imageData []byte, caption string) error {
	roomID := strings.TrimSuffix(toUserID, "@matrix")

	if p.CryptoManager != nil {
		cc, err := p.CryptoManager.GetOrCreate(ctx, creds, "")
		if err != nil {
			return err
		}
		return cc.SendImage(ctx, roomID, imageData, caption)
	}

	return MatrixSendImage(ctx, creds.BaseURL, creds.BotToken, roomID, imageData, caption)
}

// StartTyping implements TypingProvider for Matrix.
func (p *MatrixProvider) StartTyping(ctx context.Context, creds *Credentials, userID string, meta map[string]string,
	sendError func(text string)) {

	ctx, cancelFn := context.WithTimeout(ctx, matrixTypingTotalTimeout)

	go func() {
		defer cancelFn()

		roomID := strings.TrimSuffix(userID, "@matrix")

		sendTyping := func(typing bool, timeout int) error {
			if p.CryptoManager != nil {
				cc, err := p.CryptoManager.GetOrCreate(ctx, creds, "")
				if err != nil {
					return err
				}
				return cc.SendTyping(ctx, roomID, typing, timeout)
			}
			return MatrixSendTyping(ctx, creds.BaseURL, creds.BotToken, creds.BotID, roomID, typing, timeout)
		}

		if err := sendTyping(true, matrixTypingTimeoutMs); err != nil {
			log.Printf("imbridge: matrix sendTyping failed for %s: %v", roomID, err)
		}

		ticker := time.NewTicker(matrixTypingKeepalive)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				bgCtx, bgCancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = MatrixSendTyping(bgCtx, creds.BaseURL, creds.BotToken, creds.BotID, roomID, false, 0)
				bgCancel()

				if ctx.Err() == context.DeadlineExceeded {
					sendError("\u26a0\ufe0f Message processing timed out. Please try again later.")
				}
				return
			case <-ticker.C:
				if err := sendTyping(true, matrixTypingTimeoutMs); err != nil {
					log.Printf("imbridge: matrix typing keepalive failed for %s: %v", roomID, err)
				}
			}
		}
	}()
}
