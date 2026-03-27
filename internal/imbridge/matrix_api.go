package imbridge

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// MatrixMessage represents a message received from a Matrix room.
type MatrixMessage struct {
	RoomID    string
	EventID   string
	SenderID  string
	Text      string
	Timestamp int64
}

// newMatrixClient creates a short-lived mautrix client for a single API call.
func newMatrixClient(homeserverURL, accessToken string) (*mautrix.Client, error) {
	client, err := mautrix.NewClient(homeserverURL, "", accessToken)
	if err != nil {
		return nil, fmt.Errorf("matrix: create client: %w", err)
	}
	return client, nil
}

// MatrixWhoami validates a Matrix access token and returns the authenticated user ID.
func MatrixWhoami(ctx context.Context, homeserverURL, accessToken string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	client, err := newMatrixClient(homeserverURL, accessToken)
	if err != nil {
		return "", err
	}

	resp, err := client.Whoami(ctx)
	if err != nil {
		return "", fmt.Errorf("matrix: whoami: %w", err)
	}
	return string(resp.UserID), nil
}

// MatrixSync performs a single /sync request, returns messages from joined rooms
// and the next_batch token. It automatically joins any rooms the bot is invited to.
// Messages sent by selfUserID are filtered out. selfUserID should be the bot's
// own Matrix user ID (e.g. "@bot:example.com"), available from creds.BotID.
func MatrixSync(ctx context.Context, homeserverURL, accessToken, selfUserID, since string, timeoutSec int) ([]MatrixMessage, string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec+10)*time.Second)
	defer cancel()

	client, err := newMatrixClient(homeserverURL, accessToken)
	if err != nil {
		return nil, "", err
	}

	resp, err := client.SyncRequest(ctx, timeoutSec*1000, since, "", false, event.PresenceOnline)
	if err != nil {
		return nil, "", fmt.Errorf("matrix: sync: %w", err)
	}

	// Auto-join invited rooms.
	for roomID := range resp.Rooms.Invite {
		_, joinErr := client.JoinRoomByID(ctx, roomID)
		if joinErr != nil {
			log.Printf("matrix: failed to join invited room %s: %v", roomID, joinErr)
			continue
		}
	}

	var messages []MatrixMessage
	for roomID, joinedRoom := range resp.Rooms.Join {
		for _, evt := range joinedRoom.Timeline.Events {
			if evt.Type != event.EventMessage {
				continue
			}
			if string(evt.Sender) == selfUserID {
				continue
			}

			err := evt.Content.ParseRaw(evt.Type)
			if err != nil {
				continue
			}
			msgContent := evt.Content.AsMessage()
			if msgContent == nil {
				continue
			}

			messages = append(messages, MatrixMessage{
				RoomID:    string(roomID),
				EventID:   string(evt.ID),
				SenderID:  string(evt.Sender),
				Text:      msgContent.Body,
				Timestamp: evt.Timestamp,
			})
		}
	}

	return messages, resp.NextBatch, nil
}

// MatrixSendText sends a text message to a Matrix room.
func MatrixSendText(ctx context.Context, homeserverURL, accessToken, roomID, text string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	client, err := newMatrixClient(homeserverURL, accessToken)
	if err != nil {
		return err
	}

	content := &event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    text,
	}

	_, err = client.SendMessageEvent(ctx, id.RoomID(roomID), event.EventMessage, content)
	if err != nil {
		return fmt.Errorf("matrix: send text: %w", err)
	}
	return nil
}

// MatrixSendImage uploads an image to the Matrix content repository and sends
// an m.image event to a room. If caption is non-empty, it is used as the body
// text of the image event.
func MatrixSendImage(ctx context.Context, homeserverURL, accessToken, roomID string, imageData []byte, caption string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	client, err := newMatrixClient(homeserverURL, accessToken)
	if err != nil {
		return err
	}

	// Detect the content type of the image.
	contentType := http.DetectContentType(imageData)

	uploadResp, err := client.UploadBytes(ctx, imageData, contentType)
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

	_, err = client.SendMessageEvent(ctx, id.RoomID(roomID), event.EventMessage, content)
	if err != nil {
		return fmt.Errorf("matrix: send image event: %w", err)
	}
	return nil
}

// MatrixSendTyping sends a typing indicator to a Matrix room.
// userID is the bot's own Matrix user ID (needed for the typing endpoint URL path).
func MatrixSendTyping(ctx context.Context, homeserverURL, accessToken, userID, roomID string, typing bool, timeoutMs int) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	client, err := newMatrixClient(homeserverURL, accessToken)
	if err != nil {
		return err
	}

	client.UserID = id.UserID(userID)

	_, err = client.UserTyping(ctx, id.RoomID(roomID), typing, time.Duration(timeoutMs)*time.Millisecond)
	if err != nil {
		return fmt.Errorf("matrix: send typing: %w", err)
	}
	return nil
}
