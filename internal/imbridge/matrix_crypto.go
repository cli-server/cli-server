package imbridge

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/crypto/ssss"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// MatrixCryptoClient wraps a long-lived mautrix client with E2EE support.
type MatrixCryptoClient struct {
	client       *mautrix.Client
	cryptoHelper *cryptohelper.CryptoHelper
}

// SyncAndDecrypt performs a Matrix /sync, processes crypto key exchanges,
// and decrypts any encrypted messages. When initialSync is true, message
// decryption is skipped (only crypto state and cursor are processed).
func (cc *MatrixCryptoClient) SyncAndDecrypt(ctx context.Context, selfUserID string, since string, timeoutSec int, initialSync bool) ([]MatrixMessage, string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec+10)*time.Second)
	defer cancel()

	resp, err := cc.client.SyncRequest(ctx, timeoutSec*1000, since, "", false, event.PresenceOnline)
	if err != nil {
		return nil, "", fmt.Errorf("matrix: sync: %w", err)
	}

	// Process to-device events (key exchanges, OTK counts, device lists).
	mach := cc.cryptoHelper.Machine()
	mach.ProcessSyncResponse(ctx, resp, since)

	// Auto-join invited rooms.
	for roomID := range resp.Rooms.Invite {
		if _, joinErr := cc.client.JoinRoomByID(ctx, roomID); joinErr != nil {
			log.Printf("matrix: failed to join invited room %s: %v", roomID, joinErr)
		}
	}

	// Process room state events to keep the state store updated.
	// This is essential for: knowing which rooms are encrypted (IsEncrypted),
	// tracking room membership (for key sharing), and handling member changes.
	for roomID, joinedRoom := range resp.Rooms.Join {
		allStateEvents := append([]*event.Event(nil), joinedRoom.State.Events...)
		for _, evt := range joinedRoom.Timeline.Events {
			if evt.StateKey != nil {
				allStateEvents = append(allStateEvents, evt)
			}
		}
		for _, evt := range allStateEvents {
			evt.RoomID = id.RoomID(roomID)
			_ = evt.Content.ParseRaw(evt.Type)
			if cc.client.StateStore != nil {
				mautrix.UpdateStateStore(ctx, cc.client.StateStore, evt)
			}
			if evt.Type == event.StateMember {
				mach.HandleMemberEvent(ctx, evt)
			}
		}
	}

	// On initial sync, proactively request keys for all encrypted messages
	// we can't decrypt, so they're available when real polling starts.
	// Don't return messages — they're historical.
	if initialSync {
		for roomID, joinedRoom := range resp.Rooms.Join {
			for _, evt := range joinedRoom.Timeline.Events {
				evt.RoomID = id.RoomID(roomID)
				if evt.Type != event.EventEncrypted {
					continue
				}
				if parseErr := evt.Content.ParseRaw(evt.Type); parseErr != nil {
					continue
				}
				if _, decErr := mach.DecryptMegolmEvent(ctx, evt); decErr != nil {
					content := evt.Content.AsEncrypted()
					if content != nil {
						cc.client.Crypto.RequestSession(ctx, evt.RoomID, content.SenderKey, content.SessionID, evt.Sender, content.DeviceID)
					}
				}
			}
		}
		return nil, resp.NextBatch, nil
	}

	var messages []MatrixMessage
	for roomID, joinedRoom := range resp.Rooms.Join {
		if len(joinedRoom.Timeline.Events) > 0 {
			log.Printf("matrix: room=%s has %d timeline events", roomID, len(joinedRoom.Timeline.Events))
		}
		// Detect DM rooms (exactly 2 members) via state store.
		isDM := false
		if cc.client.StateStore != nil {
			type memberLister interface {
				GetRoomJoinedOrInvitedMembers(ctx context.Context, roomID id.RoomID) ([]id.UserID, error)
			}
			if ml, ok := cc.client.StateStore.(memberLister); ok {
				if members, err := ml.GetRoomJoinedOrInvitedMembers(ctx, id.RoomID(roomID)); err == nil {
					isDM = len(members) <= 2
				}
			}
		}

		for _, evt := range joinedRoom.Timeline.Events {
			evt.RoomID = id.RoomID(roomID)
			log.Printf("matrix: event room=%s type=%s sender=%s event_id=%s", roomID, evt.Type.Type, evt.Sender, evt.ID)

			// Decrypt encrypted events.
			if evt.Type == event.EventEncrypted {
				if parseErr := evt.Content.ParseRaw(evt.Type); parseErr != nil {
					log.Printf("matrix: encrypted event parse failed room=%s event=%s: %v", roomID, evt.ID, parseErr)
					continue
				}
				decrypted, decErr := mach.DecryptMegolmEvent(ctx, evt)
				if decErr != nil {
					// Request the missing session key and wait for it to arrive.
					content := evt.Content.AsEncrypted()
					if content != nil {
						cc.client.Crypto.RequestSession(ctx, evt.RoomID, content.SenderKey, content.SessionID, evt.Sender, content.DeviceID)
						if mach.WaitForSession(ctx, evt.RoomID, content.SenderKey, content.SessionID, 5*time.Second) {
							decrypted, decErr = mach.DecryptMegolmEvent(ctx, evt)
						}
					}
					if decErr != nil {
						log.Printf("matrix: decrypt failed room=%s event=%s: %v", roomID, evt.ID, decErr)
						continue
					}
				}
				evt = decrypted
				log.Printf("matrix: decrypted event room=%s type=%s sender=%s", roomID, evt.Type.Type, evt.Sender)
			}

			if evt.Type != event.EventMessage {
				log.Printf("matrix: skipping non-message event room=%s type=%s sender=%s", roomID, evt.Type.Type, evt.Sender)
				continue
			}
			if string(evt.Sender) == selfUserID {
				log.Printf("matrix: skipping own message room=%s event=%s", roomID, evt.ID)
				continue
			}

			// Try ParseRaw only if content hasn't been parsed yet (e.g. plaintext events).
			// Decrypted events already have their content parsed by DecryptMegolmEvent.
			if evt.Content.Parsed == nil {
				if err := evt.Content.ParseRaw(evt.Type); err != nil {
					continue
				}
			}
			msgContent := evt.Content.AsMessage()
			if msgContent == nil {
				continue
			}

			// Check if the bot was @-mentioned via m.mentions or body text.
			// DMs are always considered "mentioned" (the user is talking directly to the bot).
			mentioned := isDM
			if !mentioned && msgContent.Mentions != nil && msgContent.Mentions.Has(id.UserID(selfUserID)) {
				mentioned = true
			}
			if !mentioned && strings.Contains(msgContent.Body, string(cc.client.UserID)) {
				mentioned = true
			}

			msg := MatrixMessage{
				RoomID:    string(roomID),
				EventID:   string(evt.ID),
				SenderID:  string(evt.Sender),
				Text:      msgContent.Body,
				Timestamp: evt.Timestamp,
				Mentioned: mentioned,
				IsDM:      isDM,
			}

			// Download media for image/file/video/audio messages.
			switch msgContent.MsgType {
			case event.MsgImage, event.MsgFile, event.MsgVideo, event.MsgAudio:
				if data, mtype, fname := cc.downloadMedia(ctx, msgContent); data != nil {
					msg.MediaData = data
					msg.MediaType = mtype
					msg.MediaFilename = fname
				}
				if msg.Text == "" || msg.Text == msg.MediaFilename {
					msg.Text = "[" + string(msgContent.MsgType) + "]"
				}
			}

			messages = append(messages, msg)
		}
	}

	return messages, resp.NextBatch, nil
}

// SendText sends a text message to a room, auto-encrypting if the room is E2EE.
func (cc *MatrixCryptoClient) SendText(ctx context.Context, roomID, text string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	content := &event.MessageEventContent{
		MsgType:       event.MsgText,
		Body:          text,
		Format:        event.FormatHTML,
		FormattedBody: renderMarkdown(text),
	}
	_, err := cc.client.SendMessageEvent(ctx, id.RoomID(roomID), event.EventMessage, content)
	if err != nil {
		return fmt.Errorf("matrix: send text: %w", err)
	}
	return nil
}

// SendImage uploads an image and sends it to a room, auto-encrypting if E2EE.
func (cc *MatrixCryptoClient) SendImage(ctx context.Context, roomID string, imageData []byte, caption string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	contentType := http.DetectContentType(imageData)
	uploadResp, err := cc.client.UploadBytes(ctx, imageData, contentType)
	if err != nil {
		return fmt.Errorf("matrix: upload image: %w", err)
	}

	body := caption
	if body == "" {
		body = "image"
	}

	content := &event.MessageEventContent{
		MsgType: event.MsgImage,
		Body:    body,
		URL:     uploadResp.ContentURI.CUString(),
		Info: &event.FileInfo{
			MimeType: contentType,
			Size:     len(imageData),
		},
	}
	_, err = cc.client.SendMessageEvent(ctx, id.RoomID(roomID), event.EventMessage, content)
	if err != nil {
		return fmt.Errorf("matrix: send image: %w", err)
	}
	return nil
}

// SendTyping sends a typing indicator to a room.
func (cc *MatrixCryptoClient) SendTyping(ctx context.Context, roomID string, typing bool, timeoutMs int) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, err := cc.client.UserTyping(ctx, id.RoomID(roomID), typing, time.Duration(timeoutMs)*time.Millisecond)
	if err != nil {
		return fmt.Errorf("matrix: send typing: %w", err)
	}
	return nil
}

// downloadMedia downloads media from a Matrix message (handles both E2EE and plaintext).
func (cc *MatrixCryptoClient) downloadMedia(ctx context.Context, content *event.MessageEventContent) (data []byte, mediaType, filename string) {
	switch content.MsgType {
	case event.MsgImage:
		mediaType = "image"
	case event.MsgFile:
		mediaType = "file"
	case event.MsgVideo:
		mediaType = "video"
	case event.MsgAudio:
		mediaType = "audio"
	default:
		return
	}
	filename = content.Body

	// E2EE: encrypted file (has File field with encryption info).
	if content.File != nil {
		mxc, err := content.File.URL.Parse()
		if err != nil {
			log.Printf("matrix: invalid encrypted file URL: %v", err)
			return nil, "", ""
		}
		ciphertext, err := cc.client.DownloadBytes(ctx, mxc)
		if err != nil {
			log.Printf("matrix: download encrypted media failed: %v", err)
			return nil, "", ""
		}
		plaintext, err := content.File.Decrypt(ciphertext)
		if err != nil {
			log.Printf("matrix: decrypt media failed: %v", err)
			return nil, "", ""
		}
		return plaintext, mediaType, filename
	}

	// Plaintext: direct mxc:// URL.
	if content.URL != "" {
		mxc, err := content.URL.Parse()
		if err != nil {
			log.Printf("matrix: invalid media URL: %v", err)
			return nil, "", ""
		}
		downloaded, err := cc.client.DownloadBytes(ctx, mxc)
		if err != nil {
			log.Printf("matrix: download media failed: %v", err)
			return nil, "", ""
		}
		return downloaded, mediaType, filename
	}

	return nil, "", ""
}

// MatrixCryptoManager manages long-lived E2EE Matrix clients per device.
type MatrixCryptoManager struct {
	clients     map[string]*MatrixCryptoClient // keyed by botID:deviceID
	mu          sync.Mutex
	cryptoDBURL string
	encKey      []byte
}

// NewMatrixCryptoManager creates a manager for E2EE Matrix clients.
// mainDBURL is the PostgreSQL connection URL for the main database.
// The crypto database (agentserver_matrix) is derived from it and created if it doesn't exist.
func NewMatrixCryptoManager(mainDBURL string, encKey []byte) *MatrixCryptoManager {
	cryptoDBURL := deriveCryptoDBURL(mainDBURL)
	ensureCryptoDB(mainDBURL)
	return &MatrixCryptoManager{
		clients:     make(map[string]*MatrixCryptoClient),
		cryptoDBURL: cryptoDBURL,
		encKey:      encKey,
	}
}

// GetOrCreate returns an existing crypto client or creates a new one.
// Clients are keyed by botID:deviceID so each access token (with its
// unique device) gets its own Olm account. Sandboxes using the same
// token share the same client.
// If recoveryKey is non-empty, it's used to self-verify the device via SSSS cross-signing.
func (m *MatrixCryptoManager) GetOrCreate(ctx context.Context, creds *Credentials, recoveryKey string) (*MatrixCryptoClient, error) {
	// Fast path: check if any client exists for this bot (avoid Whoami on every poll).
	m.mu.Lock()
	for key, c := range m.clients {
		if strings.HasPrefix(key, creds.BotID+":") {
			m.mu.Unlock()
			// If recovery key is provided, run self-verify on the existing client.
			if recoveryKey != "" {
				m.selfVerifyAndFetchBackup(ctx, c, recoveryKey)
			}
			return c, nil
		}
	}
	m.mu.Unlock()

	// Slow path: no client yet — call Whoami to get device ID and create one.
	client, err := mautrix.NewClient(creds.BaseURL, id.UserID(creds.BotID), creds.BotToken)
	if err != nil {
		return nil, fmt.Errorf("matrix crypto: create client: %w", err)
	}
	resp, err := client.Whoami(ctx)
	if err != nil {
		return nil, fmt.Errorf("matrix crypto: whoami: %w", err)
	}
	client.DeviceID = resp.DeviceID

	key := creds.BotID + ":" + string(resp.DeviceID)

	// Double-check after Whoami.
	m.mu.Lock()
	if c, ok := m.clients[key]; ok {
		m.mu.Unlock()
		if recoveryKey != "" {
			m.selfVerifyAndFetchBackup(ctx, c, recoveryKey)
		}
		return c, nil
	}
	m.mu.Unlock()

	cc, err := m.initCryptoHelper(ctx, client, key)
	if err != nil {
		if strings.Contains(err.Error(), "not marked as shared, but there are keys on the server") {
			return nil, fmt.Errorf("matrix crypto: crypto database is out of sync with the server — the access token's device (%s) has keys on the server but no matching Olm account locally. Please generate a new access token and reconfigure the Matrix channel", client.DeviceID)
		}
		return nil, err
	}

	if recoveryKey != "" {
		m.selfVerifyAndFetchBackup(ctx, cc, recoveryKey)
	}

	m.mu.Lock()
	if existing, ok := m.clients[key]; ok {
		m.mu.Unlock()
		cc.cryptoHelper.Close()
		if recoveryKey != "" {
			m.selfVerifyAndFetchBackup(ctx, existing, recoveryKey)
		}
		return existing, nil
	}
	m.clients[key] = cc
	m.mu.Unlock()

	log.Printf("matrix crypto: initialized E2EE client for %s device=%s", creds.BotID, client.DeviceID)
	return cc, nil
}

// Remove closes and removes all crypto clients for a bot (all devices).
func (m *MatrixCryptoManager) Remove(sandboxID, botID string) {
	prefix := botID + ":"

	m.mu.Lock()
	var toClose []*MatrixCryptoClient
	for key, cc := range m.clients {
		if strings.HasPrefix(key, prefix) {
			toClose = append(toClose, cc)
			delete(m.clients, key)
		}
	}
	m.mu.Unlock()

	for _, cc := range toClose {
		if cc.cryptoHelper != nil {
			cc.cryptoHelper.Close()
		}
	}
}

// selfVerifyAndFetchBackup runs self-verification and key backup download using the recovery key.
func (m *MatrixCryptoManager) selfVerifyAndFetchBackup(ctx context.Context, cc *MatrixCryptoClient, recoveryKey string) {
	mach := cc.cryptoHelper.Machine()
	if err := mach.VerifyWithRecoveryKey(ctx, recoveryKey); err != nil {
		log.Printf("matrix crypto: self-verify failed (continuing anyway): %v", err)
	} else {
		log.Printf("matrix crypto: device %s self-verified successfully", cc.client.DeviceID)
	}
	if err := fetchAndStoreKeyBackup(ctx, cc.client, mach, recoveryKey); err != nil {
		log.Printf("matrix crypto: key backup download failed (continuing anyway): %v", err)
	}
}

// initCryptoHelper creates a DB connection, crypto helper, and initializes it.
// Returns a MatrixCryptoClient on success.
func (m *MatrixCryptoManager) initCryptoHelper(ctx context.Context, client *mautrix.Client, accountID string) (*MatrixCryptoClient, error) {
	sqlDB, err := sql.Open("postgres", m.cryptoDBURL)
	if err != nil {
		return nil, fmt.Errorf("matrix crypto: open db: %w", err)
	}
	sqlDB.SetMaxOpenConns(3)
	sqlDB.SetMaxIdleConns(1)

	cryptoDB, err := dbutil.NewWithDB(sqlDB, "postgres")
	if err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("matrix crypto: wrap db: %w", err)
	}

	helper, err := cryptohelper.NewCryptoHelper(client, m.encKey, cryptoDB)
	if err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("matrix crypto: new helper: %w", err)
	}
	helper.DBAccountID = accountID

	if err := helper.Init(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("matrix crypto: init: %w", err)
	}

	// Enable auto-encryption for outgoing messages.
	client.Crypto = helper

	return &MatrixCryptoClient{client: client, cryptoHelper: helper}, nil
}

// fetchAndStoreKeyBackup downloads the key backup from the server using the recovery key.
// This allows the bot to decrypt historical messages from before its device was created.
func fetchAndStoreKeyBackup(ctx context.Context, client *mautrix.Client, mach *crypto.OlmMachine, recoveryKey string) error {
	// Get SSSS key from recovery key.
	ssssMachine := ssss.NewSSSSMachine(client)
	keyID, keyData, err := ssssMachine.GetDefaultKeyData(ctx)
	if err != nil {
		return fmt.Errorf("get SSSS key data: %w", err)
	}
	ssssKey, err := keyData.VerifyRecoveryKey(keyID, recoveryKey)
	if err != nil {
		return fmt.Errorf("verify recovery key: %w", err)
	}

	// Decrypt the megolm backup key from SSSS.
	backupKeyBytes, err := ssssMachine.GetDecryptedAccountData(ctx, event.AccountDataMegolmBackupKey, ssssKey)
	if err != nil {
		return fmt.Errorf("get megolm backup key from SSSS: %w", err)
	}

	megolmBackupKey, err := backup.MegolmBackupKeyFromBytes(backupKeyBytes)
	if err != nil {
		return fmt.Errorf("parse megolm backup key: %w", err)
	}

	// Download and store the key backup.
	version, err := mach.DownloadAndStoreLatestKeyBackup(ctx, megolmBackupKey)
	if err != nil {
		return fmt.Errorf("download key backup: %w", err)
	}
	log.Printf("matrix crypto: downloaded key backup version %s", version)
	return nil
}

// deriveCryptoDBURL derives the agentserver_matrix database URL from the main database URL.
func deriveCryptoDBURL(mainDBURL string) string {
	u, err := url.Parse(mainDBURL)
	if err != nil {
		return strings.Replace(mainDBURL, "/agentserver", "/agentserver_matrix", 1)
	}
	u.Path = "/agentserver_matrix"
	return u.String()
}

// ensureCryptoDB creates the agentserver_matrix database if it doesn't exist.
func ensureCryptoDB(mainDBURL string) {
	db, err := sql.Open("postgres", mainDBURL)
	if err != nil {
		log.Printf("matrix crypto: cannot open main db to create crypto db: %v", err)
		return
	}
	defer db.Close()

	var exists bool
	err = db.QueryRow("SELECT true FROM pg_database WHERE datname = 'agentserver_matrix'").Scan(&exists)
	if err == nil && exists {
		return
	}

	// CREATE DATABASE cannot run inside a transaction.
	_, err = db.Exec("CREATE DATABASE agentserver_matrix")
	if err != nil {
		log.Printf("matrix crypto: create database agentserver_matrix: %v", err)
	} else {
		log.Println("matrix crypto: created database agentserver_matrix")
	}
}
