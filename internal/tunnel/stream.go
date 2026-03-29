package tunnel

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Stream types identify the purpose of each yamux stream.
const (
	StreamTypeHTTP     byte = 0x01 // HTTP proxy request (server → agent)
	StreamTypeTerminal byte = 0x02 // Terminal bidirectional stream (server → agent)
	StreamTypeControl  byte = 0x03 // Control message: agent info, etc. (agent → server)
)

// WriteStreamHeader writes the stream header: [1 byte type][4 bytes metadata len][metadata].
func WriteStreamHeader(w io.Writer, streamType byte, metadata []byte) error {
	header := make([]byte, 5)
	header[0] = streamType
	binary.BigEndian.PutUint32(header[1:5], uint32(len(metadata)))
	if _, err := w.Write(header); err != nil {
		return fmt.Errorf("write stream header: %w", err)
	}
	if len(metadata) > 0 {
		if _, err := w.Write(metadata); err != nil {
			return fmt.Errorf("write stream metadata: %w", err)
		}
	}
	return nil
}

// ReadStreamHeader reads the stream header and returns the type and metadata.
func ReadStreamHeader(r io.Reader) (streamType byte, metadata []byte, err error) {
	header := make([]byte, 5)
	if _, err = io.ReadFull(r, header); err != nil {
		return 0, nil, fmt.Errorf("read stream header: %w", err)
	}
	streamType = header[0]
	metaLen := binary.BigEndian.Uint32(header[1:5])
	if metaLen > 0 {
		if metaLen > 1<<20 { // 1MB sanity limit
			return 0, nil, fmt.Errorf("stream metadata too large: %d bytes", metaLen)
		}
		metadata = make([]byte, metaLen)
		if _, err = io.ReadFull(r, metadata); err != nil {
			return 0, nil, fmt.Errorf("read stream metadata: %w", err)
		}
	}
	return streamType, metadata, nil
}

// HTTPStreamMeta is the metadata for an HTTP proxy stream (server → agent).
// BodyLen indicates the number of request body bytes that follow the stream header.
type HTTPStreamMeta struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers"`
	BodyLen int               `json:"body_len"`
}

// HTTPResponseMeta is the response header written by the agent on an HTTP stream.
type HTTPResponseMeta struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
}

// MarshalStreamMeta marshals metadata to JSON for WriteStreamHeader.
func MarshalStreamMeta(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

// UnmarshalStreamMeta unmarshals metadata JSON from ReadStreamHeader.
func UnmarshalStreamMeta(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}
