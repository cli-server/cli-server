package bridge

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/agentserver/agentserver/internal/db"
)

const (
	maxEventsPerBatch  = 100
	maxBodySize        = 10 * 1024 * 1024 // 10MB
	sseReplayLimit     = 1000
	sseKeepaliveInterval = 15 * time.Second
	jwtTTL             = 24 * time.Hour
)

// Handler implements the CCR V2-compatible bridge API.
type Handler struct {
	DB        *db.DB
	SSE       *SSEBroker
	Epochs    *EpochCache
	JWTSecret []byte
	dedups    sync.Map // session_id → *BoundedUUIDSet
}

func NewHandler(database *db.DB, jwtSecret []byte) *Handler {
	return &Handler{
		DB:        database,
		SSE:       NewSSEBroker(),
		Epochs:    NewEpochCache(),
		JWTSecret: jwtSecret,
	}
}

func (h *Handler) getDedupSet(sessionID string) *BoundedUUIDSet {
	v, _ := h.dedups.LoadOrStore(sessionID, NewBoundedUUIDSet(2000))
	return v.(*BoundedUUIDSet)
}

// validateEpoch checks the worker epoch against the current session epoch.
// Returns nil if valid, writes HTTP 409 and returns error if mismatched.
func (h *Handler) validateEpoch(w http.ResponseWriter, sessionID string, workerEpoch int) error {
	epoch, ok := h.Epochs.Get(sessionID)
	if !ok {
		var err error
		epoch, err = h.DB.GetAgentSessionEpoch(sessionID)
		if err != nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return err
		}
		h.Epochs.Set(sessionID, epoch)
	}
	if workerEpoch != epoch {
		http.Error(w, "epoch mismatch", http.StatusConflict)
		return fmt.Errorf("epoch mismatch: want %d, got %d", epoch, workerEpoch)
	}
	return nil
}

// --- Session Lifecycle Endpoints ---

// HandleCreateSession handles POST /v1/agent/sessions
func (h *Handler) HandleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title     string   `json:"title"`
		Tags      []string `json:"tags,omitempty"`
		SandboxID string   `json:"sandbox_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// sandbox_id from proxy_token auth takes precedence over body.
	sandboxID := SandboxIDFromContext(r.Context())
	if sandboxID == "" {
		sandboxID = req.SandboxID
	}

	// Resolve sandboxID to a pointer (nil when empty for stateless CC sessions).
	var sandboxIDPtr *string
	if sandboxID != "" {
		sandboxIDPtr = &sandboxID
	}

	workspaceID := WorkspaceIDFromContext(r.Context())
	if workspaceID == "" {
		if sandboxID == "" {
			http.Error(w, "workspace_id or sandbox_id required", http.StatusBadRequest)
			return
		}
		// Look up workspace from sandbox.
		sbx, err := h.DB.GetSandbox(sandboxID)
		if err != nil || sbx == nil {
			http.Error(w, "sandbox not found", http.StatusNotFound)
			return
		}
		workspaceID = sbx.WorkspaceID
	}

	sessionID := "cse_" + uuid.New().String()
	if err := h.DB.CreateAgentSession(sessionID, sandboxIDPtr, workspaceID, req.Title, req.Tags); err != nil {
		log.Printf("bridge: create session error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{
		"session": map[string]string{"id": sessionID},
	})
}

// HandleBridge handles POST /v1/agent/sessions/{sessionId}/bridge
func (h *Handler) HandleBridge(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionId")
	session, err := h.DB.GetAgentSession(sessionID)
	if err != nil || session == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	if session.Status != "active" {
		http.Error(w, "session archived", http.StatusGone)
		return
	}

	// Verify caller owns this session's sandbox.
	callerSandbox := SandboxIDFromContext(r.Context())
	if callerSandbox != "" && (session.SandboxID == nil || callerSandbox != *session.SandboxID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Bump epoch atomically.
	newEpoch, err := h.DB.BumpAgentSessionEpoch(sessionID)
	if err != nil {
		log.Printf("bridge: bump epoch error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.Epochs.Invalidate(sessionID)

	// Upsert worker record.
	if err := h.DB.UpsertAgentSessionWorker(sessionID, newEpoch); err != nil {
		log.Printf("bridge: upsert worker error: %v", err)
	}

	// Issue worker JWT.
	sandboxIDStr := ""
	if session.SandboxID != nil {
		sandboxIDStr = *session.SandboxID
	}
	token, err := IssueWorkerJWT(h.JWTSecret, sessionID, sandboxIDStr, session.WorkspaceID, newEpoch, jwtTTL)
	if err != nil {
		log.Printf("bridge: issue jwt error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Build api_base_url from request host.
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	if fwd := r.Header.Get("X-Forwarded-Proto"); fwd == "http" || fwd == "https" {
		scheme = fwd
	}
	host := r.Host
	apiBaseURL := fmt.Sprintf("%s://%s/v1/agent/sessions/%s", scheme, host, sessionID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"worker_jwt":   token,
		"api_base_url": apiBaseURL,
		"expires_in":   int(jwtTTL.Seconds()),
		"worker_epoch": newEpoch,
	})
}

// HandleArchive handles POST /v1/agent/sessions/{sessionId}/archive
func (h *Handler) HandleArchive(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionId")

	session, err := h.DB.GetAgentSession(sessionID)
	if err != nil || session == nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	// Verify caller owns this session.
	callerSandbox := SandboxIDFromContext(r.Context())
	if callerSandbox != "" && (session.SandboxID == nil || callerSandbox != *session.SandboxID) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	if err := h.DB.ArchiveAgentSession(sessionID); err != nil {
		log.Printf("bridge: archive error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.Epochs.Invalidate(sessionID)
	h.dedups.Delete(sessionID)
	w.WriteHeader(http.StatusOK)
}

// --- Worker Endpoints ---

// HandleWorkerEventStream handles GET .../worker/events/stream (SSE)
func (h *Handler) HandleWorkerEventStream(w http.ResponseWriter, r *http.Request) {
	sessionID := SessionIDFromContext(r.Context())
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Parse from_sequence_num.
	var fromSeq int64
	if v := r.URL.Query().Get("from_sequence_num"); v != "" {
		fromSeq, _ = strconv.ParseInt(v, 10, 64)
	}
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
			fromSeq = parsed
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Subscribe FIRST, then replay from DB.
	// This prevents the race where events published between DB query and
	// subscribe are lost. Events from replay that also appear in the
	// subscriber channel are deduplicated by sequence number tracking.
	sub := h.SSE.Subscribe(sessionID)
	defer h.SSE.Unsubscribe(sessionID, sub)

	// Replay missed events from DB.
	events, err := h.DB.GetAgentSessionEventsSince(sessionID, fromSeq, sseReplayLimit)
	if err != nil {
		log.Printf("bridge: sse replay error: %v", err)
	}
	for _, e := range events {
		sce := &StreamClientEvent{
			EventID:     e.EventID,
			SequenceNum: e.ID,
			EventType:   e.EventType,
			Source:       e.Source,
			Payload:     e.Payload,
			CreatedAt:   e.CreatedAt.Format(time.RFC3339Nano),
		}
		writeSSEFrame(w, sce)
		fromSeq = e.ID
	}
	flusher.Flush()

	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()

	for {
		select {
		case event := <-sub.Ch:
			// Skip events already sent during replay.
			if event.SequenceNum <= fromSeq {
				continue
			}
			fromSeq = event.SequenceNum
			writeSSEFrame(w, event)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ":keepalive\n\n")
			flusher.Flush()
		case <-sub.Done():
			return
		case <-r.Context().Done():
			return
		}
	}
}

func writeSSEFrame(w http.ResponseWriter, event *StreamClientEvent) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\nid: %d\ndata: %s\n\n", event.EventType, event.SequenceNum, data)
}

// HandleWorkerEvents handles POST .../worker/events
func (h *Handler) HandleWorkerEvents(w http.ResponseWriter, r *http.Request) {
	sessionID := SessionIDFromContext(r.Context())

	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	var req struct {
		WorkerEpoch int `json:"worker_epoch"`
		Events      []struct {
			Payload   json.RawMessage `json:"payload"`
			Ephemeral bool            `json:"ephemeral,omitempty"`
		} `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if len(req.Events) > maxEventsPerBatch {
		http.Error(w, fmt.Sprintf("too many events (max %d)", maxEventsPerBatch), http.StatusBadRequest)
		return
	}

	if err := h.validateEpoch(w, sessionID, req.WorkerEpoch); err != nil {
		return
	}

	// Build DB events, dedup.
	dedup := h.getDedupSet(sessionID)
	var dbEvents []db.AgentSessionEvent
	for _, e := range req.Events {
		// Extract uuid from payload.
		var envelope struct {
			UUID string `json:"uuid"`
		}
		json.Unmarshal(e.Payload, &envelope)
		eventID := envelope.UUID
		if eventID == "" {
			eventID = uuid.New().String()
		}

		if !dedup.Add(eventID) {
			continue // duplicate
		}

		dbEvents = append(dbEvents, db.AgentSessionEvent{
			EventID:   eventID,
			EventType: "client_event",
			Source:     "worker",
			Epoch:     req.WorkerEpoch,
			Payload:   e.Payload,
			Ephemeral: e.Ephemeral,
		})
	}

	if len(dbEvents) == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}

	inserted, err := h.DB.InsertAgentSessionEvents(sessionID, dbEvents)
	if err != nil {
		log.Printf("bridge: insert events error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Publish to SSE subscribers.
	for _, ie := range inserted {
		h.SSE.Publish(sessionID, &StreamClientEvent{
			EventID:     ie.Event.EventID,
			SequenceNum: ie.SeqNum,
			EventType:   ie.Event.EventType,
			Source:       ie.Event.Source,
			Payload:     ie.Event.Payload,
			CreatedAt:   time.Now().Format(time.RFC3339Nano),
		})
	}

	w.WriteHeader(http.StatusOK)
}

// HandleWorkerInternalEvents handles POST .../worker/internal-events
func (h *Handler) HandleWorkerInternalEvents(w http.ResponseWriter, r *http.Request) {
	sessionID := SessionIDFromContext(r.Context())

	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)
	var req struct {
		WorkerEpoch int `json:"worker_epoch"`
		Events      []struct {
			Payload      json.RawMessage `json:"payload"`
			IsCompaction bool            `json:"is_compaction,omitempty"`
			AgentID      string          `json:"agent_id,omitempty"`
		} `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if len(req.Events) > maxEventsPerBatch {
		http.Error(w, fmt.Sprintf("too many events (max %d)", maxEventsPerBatch), http.StatusBadRequest)
		return
	}

	if err := h.validateEpoch(w, sessionID, req.WorkerEpoch); err != nil {
		return
	}

	// Convert to DB format.
	dbEvents := make([]struct {
		EventType    string
		Payload      json.RawMessage
		IsCompaction bool
		AgentID      string
	}, len(req.Events))
	for i, e := range req.Events {
		var envelope struct {
			Type string `json:"type"`
		}
		json.Unmarshal(e.Payload, &envelope)
		dbEvents[i] = struct {
			EventType    string
			Payload      json.RawMessage
			IsCompaction bool
			AgentID      string
		}{
			EventType:    envelope.Type,
			Payload:      e.Payload,
			IsCompaction: e.IsCompaction,
			AgentID:      e.AgentID,
		}
	}

	if err := h.DB.InsertAgentSessionInternalEvents(sessionID, dbEvents); err != nil {
		log.Printf("bridge: insert internal events error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// HandleWorkerDelivery handles POST .../worker/events/delivery
func (h *Handler) HandleWorkerDelivery(w http.ResponseWriter, r *http.Request) {
	// Delivery ACKs are informational — log and accept.
	var req struct {
		WorkerEpoch int `json:"worker_epoch"`
		Updates     []struct {
			EventID string `json:"event_id"`
			Status  string `json:"status"`
		} `json:"updates"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// HandleWorkerState handles PUT .../worker
func (h *Handler) HandleWorkerState(w http.ResponseWriter, r *http.Request) {
	sessionID := SessionIDFromContext(r.Context())
	epoch := EpochFromContext(r.Context())

	var req struct {
		WorkerStatus         string          `json:"worker_status"`
		WorkerEpoch          int             `json:"worker_epoch"`
		ExternalMetadata     json.RawMessage `json:"external_metadata,omitempty"`
		RequiresActionDetails json.RawMessage `json:"requires_action_details,omitempty"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	checkEpoch := req.WorkerEpoch
	if checkEpoch == 0 {
		checkEpoch = epoch
	}
	if err := h.validateEpoch(w, sessionID, checkEpoch); err != nil {
		return
	}

	state := req.WorkerStatus
	if state == "" {
		state = "idle"
	}

	if err := h.DB.UpdateAgentSessionWorkerState(sessionID, checkEpoch, state, req.ExternalMetadata, req.RequiresActionDetails); err != nil {
		log.Printf("bridge: update worker state error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// HandleWorkerHeartbeat handles POST .../worker/heartbeat
func (h *Handler) HandleWorkerHeartbeat(w http.ResponseWriter, r *http.Request) {
	sessionID := SessionIDFromContext(r.Context())
	epoch := EpochFromContext(r.Context())

	var req struct {
		WorkerEpoch int `json:"worker_epoch"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	checkEpoch := req.WorkerEpoch
	if checkEpoch == 0 {
		checkEpoch = epoch
	}
	if err := h.validateEpoch(w, sessionID, checkEpoch); err != nil {
		return
	}

	if err := h.DB.UpdateAgentSessionWorkerHeartbeat(sessionID, checkEpoch); err != nil {
		log.Printf("bridge: heartbeat error: %v", err)
	}

	w.WriteHeader(http.StatusOK)
}

// HandleGetWorker handles GET .../worker
func (h *Handler) HandleGetWorker(w http.ResponseWriter, r *http.Request) {
	sessionID := SessionIDFromContext(r.Context())
	epoch := EpochFromContext(r.Context())

	worker, err := h.DB.GetAgentSessionWorker(sessionID, epoch)
	if err != nil {
		log.Printf("bridge: get worker error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if worker == nil {
		http.Error(w, "worker not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"worker": map[string]any{
			"state":             worker.State,
			"external_metadata": worker.ExternalMetadata,
			"last_heartbeat_at": worker.LastHeartbeatAt.Format(time.RFC3339),
		},
	})
}

// HandleGetInternalEvents handles GET .../worker/internal-events
func (h *Handler) HandleGetInternalEvents(w http.ResponseWriter, r *http.Request) {
	sessionID := SessionIDFromContext(r.Context())

	var fromSeq int64
	if v := r.URL.Query().Get("from_sequence_num"); v != "" {
		fromSeq, _ = strconv.ParseInt(v, 10, 64)
	}

	events, err := h.DB.GetAgentSessionInternalEventsSince(sessionID, fromSeq, 1000)
	if err != nil {
		log.Printf("bridge: get internal events error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": events})
}
