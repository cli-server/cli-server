package ccbroker

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/agentserver/agentserver/internal/ccbroker/tools"
)

func (s *Server) handleCancelTurn(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "sid")
	tid := chi.URLParam(r, "tid")

	turn, err := s.store.GetTurn(r.Context(), tid)
	if err != nil {
		http.Error(w, `{"code":"internal"}`, http.StatusInternalServerError)
		return
	}
	if turn == nil || turn.SessionID != sid {
		http.Error(w, `{"code":"not_found"}`, http.StatusNotFound)
		return
	}

	switch turn.State {
	case "queued":
		if err := s.store.MarkTurnCancelled(r.Context(), tid); err != nil {
			http.Error(w, `{"code":"internal"}`, http.StatusInternalServerError)
			return
		}
		s.gate.CancelTurn(tid)
		s.broadcastTurnCancelled(sid, tid)
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"cancelled":true,"was":"queued"}`))
	case "running":
		s.activeTurns.Cancel(sid, tid)
		s.gate.CancelTurn(tid)
		s.broadcastTurnCancelled(sid, tid)
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"cancelled":true,"was":"running"}`))
	default:
		w.WriteHeader(http.StatusGone)
		w.Write([]byte(`{"code":"already_terminal","state":"` + turn.State + `"}`))
	}
}

func (s *Server) broadcastTurnCancelled(sid, tid string) {
	payload, _ := json.Marshal(map[string]string{"turn_id": tid})
	s.sse.Publish(sid, &StreamClientEvent{
		EventID:   "evt_" + uuid.NewString(),
		EventType: "turn_cancelled",
		Source:    "broker",
		TurnID:    tid,
		Payload:   payload,
		CreatedAt: time.Now().Format(time.RFC3339Nano),
	})
}

func (s *Server) handleDecidePermission(w http.ResponseWriter, r *http.Request) {
	pid := chi.URLParam(r, "pid")
	var body struct {
		Verdict string `json:"verdict"`
		Scope   string `json:"scope"`
		By      string `json:"by"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"code":"invalid"}`, http.StatusBadRequest)
		return
	}
	if err := s.gate.Resolve(pid, tools.Decision{Verdict: body.Verdict, Scope: body.Scope, By: body.By}); err != nil {
		http.Error(w, `{"code":"already_resolved"}`, http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"accepted":true}`))
}

func (s *Server) handleCompactNow(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "sid")
	s.compactQueue.Set(sid)
	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte(`{"queued":true}`))
}

func (s *Server) handleGetActiveTurn(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "sid")
	w.Header().Set("Content-Type", "application/json")
	if tid, ok := s.activeTurns.Get(sid); ok {
		json.NewEncoder(w).Encode(map[string]string{"turn_id": tid})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"turn_id": nil})
}
