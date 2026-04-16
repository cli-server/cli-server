package ccbroker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// handleWorkerEventStream serves the SSE endpoint that CC workers connect to
// for conversation history replay and live event streaming.
func (s *Server) handleWorkerEventStream(w http.ResponseWriter, r *http.Request) {
	sessionID := SessionIDFromContext(r.Context())

	// Parse from_sequence_num (query param or Last-Event-ID header).
	var fromSeq int64
	if v := r.URL.Query().Get("from_sequence_num"); v != "" {
		fromSeq, _ = strconv.ParseInt(v, 10, 64)
	} else if v := r.Header.Get("Last-Event-ID"); v != "" {
		fromSeq, _ = strconv.ParseInt(v, 10, 64)
	}

	// Set SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// 1. Subscribe FIRST (before replay) to capture events during replay.
	sub := s.sse.Subscribe(sessionID)
	defer s.sse.Unsubscribe(sessionID, sub)

	// 2. Replay persisted events from DB.
	events, err := s.store.GetEventsSince(r.Context(), sessionID, fromSeq, 1000)
	if err != nil {
		s.logger.Error("replay events failed", "error", err)
		return
	}

	lastSeq := fromSeq
	for _, evt := range events {
		sce := StreamClientEvent{
			EventID:     evt.EventID,
			SequenceNum: evt.ID,
			EventType:   evt.EventType,
			Source:      evt.Source,
			Payload:     evt.Payload,
			CreatedAt:   evt.CreatedAt.Format(time.RFC3339Nano),
		}
		data, _ := json.Marshal(sce)
		fmt.Fprintf(w, "event: %s\nid: %d\ndata: %s\n\n", evt.EventType, evt.ID, data)
		flusher.Flush()
		lastSeq = evt.ID
	}

	// 3. Stream loop: live events + keepalive (lastSeq guard filters already-replayed events).
	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case evt := <-sub.Ch:
			if evt.SequenceNum <= lastSeq {
				continue // already replayed
			}
			data, _ := json.Marshal(evt)
			fmt.Fprintf(w, "event: %s\nid: %d\ndata: %s\n\n", evt.EventType, evt.SequenceNum, data)
			flusher.Flush()
			lastSeq = evt.SequenceNum
		case <-sub.Done():
			return
		case <-keepalive.C:
			fmt.Fprintf(w, ":keepalive\n\n")
			flusher.Flush()
		}
	}
}

// handleWorkerEvents accepts a batch of events from a CC worker, validates the
// epoch, deduplicates, persists to DB, and publishes to SSE subscribers.
func (s *Server) handleWorkerEvents(w http.ResponseWriter, r *http.Request) {
	sessionID := SessionIDFromContext(r.Context())

	var req EventBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Limit batch size.
	if len(req.Events) > 100 {
		writeError(w, http.StatusBadRequest, "max 100 events per batch")
		return
	}

	// Validate epoch.
	currentEpoch, err := s.store.GetSessionEpoch(r.Context(), sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to check epoch")
		return
	}
	if req.WorkerEpoch != currentEpoch {
		writeError(w, http.StatusConflict, fmt.Sprintf("epoch mismatch: got %d, current %d", req.WorkerEpoch, currentEpoch))
		return
	}

	// Build EventInput list with dedup.
	dedupSet := s.dedup.GetOrCreate(sessionID)
	var inputs []EventInput
	for _, evt := range req.Events {
		// Extract uuid from payload, or generate one.
		var payloadMap map[string]interface{}
		json.Unmarshal(evt.Payload, &payloadMap)

		eventUUID := ""
		if u, ok := payloadMap["uuid"].(string); ok {
			eventUUID = u
		} else {
			eventUUID = uuid.NewString()
		}

		// Dedup check.
		if !dedupSet.Add(eventUUID) {
			continue // duplicate, skip
		}

		inputs = append(inputs, EventInput{
			EventID:   eventUUID,
			Payload:   evt.Payload,
			Ephemeral: evt.Ephemeral,
		})
	}

	if len(inputs) == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Insert into DB.
	inserted, err := s.store.InsertEvents(r.Context(), sessionID, req.WorkerEpoch, inputs)
	if err != nil {
		s.logger.Error("insert events failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to persist events")
		return
	}

	// Build eventID → payload map for correct lookup
	payloadByID := make(map[string]json.RawMessage, len(inputs))
	for _, inp := range inputs {
		payloadByID[inp.EventID] = inp.Payload
	}
	for _, ins := range inserted {
		s.sse.Publish(sessionID, &StreamClientEvent{
			EventID:     ins.EventID,
			SequenceNum: ins.SeqNum,
			EventType:   "client_event",
			Source:      "worker",
			Payload:     payloadByID[ins.EventID],
			CreatedAt:   time.Now().Format(time.RFC3339Nano),
		})
	}

	w.WriteHeader(http.StatusOK)
}
