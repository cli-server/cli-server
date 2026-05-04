package ccbroker

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
)

func (s *Server) handleTurnEvents(w http.ResponseWriter, r *http.Request) {
	tid := chi.URLParam(r, "tid")

	turn, err := s.store.GetTurn(r.Context(), tid)
	if err != nil {
		http.Error(w, `{"code":"internal"}`, http.StatusInternalServerError)
		return
	}
	if turn == nil {
		http.Error(w, `{"code":"not_found"}`, http.StatusNotFound)
		return
	}

	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Subscribe FIRST so we don't miss events that fire between catch-up and tail.
	sub := s.sse.Subscribe(turn.SessionID)
	defer s.sse.Unsubscribe(turn.SessionID, sub)

	// Catch-up from DB.
	seenSeqs := map[int64]struct{}{}
	highestSeq := since
	if past, err := s.store.GetTurnEvents(r.Context(), tid, since); err == nil {
		for _, e := range past {
			evt := &StreamClientEvent{
				EventID: e.EventID, SequenceNum: e.SeqNum, EventType: e.EventType,
				Source: "catchup", TurnID: tid, Payload: e.Payload,
				CreatedAt: e.CreatedAt.Format(time.RFC3339Nano),
			}
			data, _ := json.Marshal(evt)
			fmt.Fprintf(w, "data: %s\n\n", data)
			seenSeqs[e.SeqNum] = struct{}{}
			if e.SeqNum > highestSeq {
				highestSeq = e.SeqNum
			}
			if isTerminalEventType(e.EventType) {
				fmt.Fprintf(w, "data: {\"event_type\":\"done\",\"turn_id\":%q}\n\n", tid)
				flusher.Flush()
				return
			}
		}
		flusher.Flush()
	} else {
		s.logger.Warn("turn events catch-up failed; continuing with live tail only", "turn_id", tid, "error", err)
	}
	_ = highestSeq

	// If turn was already terminal at request time and DB had no events past
	// since, end here. Otherwise tail.
	if isTerminalTurnState(turn.State) && len(seenSeqs) == 0 {
		fmt.Fprintf(w, "data: {\"event_type\":\"done\",\"turn_id\":%q}\n\n", tid)
		flusher.Flush()
		return
	}

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.Done():
			return
		case evt := <-sub.Ch:
			if evt.TurnID != tid {
				continue
			}
			if evt.SequenceNum != 0 {
				if _, dup := seenSeqs[evt.SequenceNum]; dup {
					continue
				}
				seenSeqs[evt.SequenceNum] = struct{}{}
			}
			data, _ := json.Marshal(evt)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
			if isTerminalEventType(evt.EventType) {
				fmt.Fprintf(w, "data: {\"event_type\":\"done\",\"turn_id\":%q}\n\n", tid)
				flusher.Flush()
				return
			}
		case <-keepalive.C:
			fmt.Fprintf(w, ":keepalive\n\n")
			flusher.Flush()
		}
	}
}

func isTerminalTurnState(state string) bool {
	switch state {
	case "done", "cancelled", "failed":
		return true
	}
	return false
}
