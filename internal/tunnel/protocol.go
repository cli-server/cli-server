package tunnel

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
)

// FrameType indicates the kind of tunnel message.
const (
	FrameTypeRequest = "request"
	FrameTypeStream  = "stream"
)

// RequestHeader is the JSON metadata for a request frame (server → agent).
type RequestHeader struct {
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
}

// StreamHeader is the JSON metadata for a stream frame (agent → server).
type StreamHeader struct {
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	Status  int               `json:"status,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Done    bool              `json:"done"`
}

// IncomingHeader is used for initial JSON unmarshaling to determine frame type.
type IncomingHeader struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Wire format for binary WebSocket messages:
//   [4 bytes: JSON header length (big-endian uint32)]
//   [JSON header bytes]
//   [raw binary payload (may be empty)]

// EncodeFrame encodes a JSON header and binary payload into a single binary message.
func EncodeFrame(header interface{}, payload []byte) ([]byte, error) {
	jsonBytes, err := json.Marshal(header)
	if err != nil {
		return nil, fmt.Errorf("marshal header: %w", err)
	}
	msg := make([]byte, 4+len(jsonBytes)+len(payload))
	binary.BigEndian.PutUint32(msg[:4], uint32(len(jsonBytes)))
	copy(msg[4:4+len(jsonBytes)], jsonBytes)
	copy(msg[4+len(jsonBytes):], payload)
	return msg, nil
}

// DecodeFrameHeader decodes only the JSON header and returns the payload offset.
func DecodeFrameHeader(msg []byte) (headerJSON []byte, payload []byte, err error) {
	if len(msg) < 4 {
		return nil, nil, fmt.Errorf("message too short: %d bytes", len(msg))
	}
	hdrLen := binary.BigEndian.Uint32(msg[:4])
	if uint32(len(msg)) < 4+hdrLen {
		return nil, nil, fmt.Errorf("message truncated: header says %d but only %d bytes remain", hdrLen, len(msg)-4)
	}
	return msg[4 : 4+hdrLen], msg[4+hdrLen:], nil
}
