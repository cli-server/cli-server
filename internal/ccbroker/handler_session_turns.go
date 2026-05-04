package ccbroker

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

type sessionTurnRow struct {
	TurnID     string  `json:"turn_id"`
	State      string  `json:"state"`
	EnqueuedAt string  `json:"enqueued_at"`
	StartedAt  *string `json:"started_at,omitempty"`
	FinishedAt *string `json:"finished_at,omitempty"`
	ErrorMsg   *string `json:"error_msg,omitempty"`
}

func (s *Server) handleListSessionTurns(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "sid")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	turns, err := s.store.ListSessionTurns(r.Context(), sid, limit)
	if err != nil {
		http.Error(w, `{"code":"internal"}`, http.StatusInternalServerError)
		return
	}
	out := make([]sessionTurnRow, 0, len(turns))
	for _, t := range turns {
		row := sessionTurnRow{
			TurnID: t.ID, State: t.State,
			EnqueuedAt: t.EnqueuedAt.UTC().Format("2006-01-02T15:04:05.000000000Z"),
		}
		if t.StartedAt.Valid {
			started := t.StartedAt.Time.UTC().Format("2006-01-02T15:04:05.000000000Z")
			row.StartedAt = &started
		}
		if t.FinishedAt.Valid {
			finished := t.FinishedAt.Time.UTC().Format("2006-01-02T15:04:05.000000000Z")
			row.FinishedAt = &finished
		}
		if t.ErrorMsg.Valid {
			emsg := t.ErrorMsg.String
			row.ErrorMsg = &emsg
		}
		out = append(out, row)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"turns": out})
}
