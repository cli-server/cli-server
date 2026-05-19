package sdk

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/agentserver/agentserver/internal/envtools/processes"
	"github.com/agentserver/agentserver/internal/envtools/tools"
)

// toolDesc is the per-tool entry in envs/list responses. The SDK uses
// these to populate Env.tools. No JSON schema — server validates tool
// args; SDK trusts.
type toolDesc struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
}

// coreTools returns the fixed list of tools the SDK knows about. Kept
// hardcoded so envs/list doesn't depend on the tool registry being
// populated (which only matters for tool/call).
func coreTools() []toolDesc {
	return []toolDesc{
		{Name: "shell", Kind: "core", Description: "Run a command synchronously."},
		{Name: "read_file", Kind: "core", Description: "Read a file by path."},
		{Name: "write_file", Kind: "core", Description: "Write a file by path."},
		{Name: "apply_patch", Kind: "core", Description: "Apply a unified-diff patch."},
		{Name: "copy_path", Kind: "core", Description: "Upload or download a file."},
		{Name: "exec_command", Kind: "core", Description: "Start a long-running process (returns session_id)."},
	}
}

type envEntry struct {
	Name      string     `json:"name"`
	Type      string     `json:"type"`
	IsDefault bool       `json:"is_default"`
	Tools     []toolDesc `json:"tools"`
	LastSeen  string     `json:"last_seen,omitempty"`
}

// toolCallReq is the request body for POST /api/sdk/envs/{name}/tool/call.
type toolCallReq struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

func (s *Server) handleToolCall(w http.ResponseWriter, r *http.Request) {
	wsID := workspaceFromCtx(r.Context())
	_ = chi.URLParam(r, "name") // env name; tools are workspace-scoped by their embedded resolver/pool
	var req toolCallReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	tool, ok := s.Tools[req.Tool]
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown_tool", "no such tool: "+req.Tool)
		return
	}
	argsJSON, err := json.Marshal(req.Arguments)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_arguments", err.Error())
		return
	}
	result, err := tool.Call(r.Context(), argsJSON)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "tool_error", err.Error())
		return
	}
	// exec_command encodes session_id as JSON text in Content[0].Text.
	// Register a Session row so subsequent /processes/{sid}/* calls find it.
	if sid := extractSessionID(result); sid != "" && s.Sessions != nil {
		s.Sessions.Register(&processes.Session{
			ID:          sid,
			WorkspaceID: wsID,
		})
	}
	writeJSON(w, result)
}

// extractSessionID parses the session_id field from a tool result whose
// first content item is a JSON-encoded object (as exec_command returns).
// Returns "" if the result contains no such field.
func extractSessionID(result tools.MCPCallToolResult) string {
	if len(result.Content) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(result.Content[0].Text), &obj); err != nil {
		return ""
	}
	sid, _ := obj["session_id"].(string)
	return sid
}

func (s *Server) handleEnvsList(w http.ResponseWriter, r *http.Request) {
	wsID := workspaceFromCtx(r.Context())
	connected, err := s.Registry.Connected(r.Context(), wsID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "registry_error", err.Error())
		return
	}
	envs := make([]envEntry, 0, len(connected))
	for _, c := range connected {
		envs = append(envs, envEntry{
			Name:      c.Name,
			Type:      "executor",
			IsDefault: c.IsDefault,
			Tools:     coreTools(),
			LastSeen:  c.LastSeenAt,
		})
	}
	writeJSON(w, map[string]any{"envs": envs})
}

// stdinReq is the request body for POST /api/sdk/processes/{sid}/stdin.
type stdinReq struct {
	DataB64 string `json:"data_b64"`
}

// outputChunk is one entry in the chunks array returned by
// GET /api/sdk/processes/{sid}/output.
type outputChunk struct {
	Stream string `json:"stream"`
	Data   string `json:"data_b64"`
	Seq    int    `json:"seq"`
}

// sessionFromReq looks up the session by chi URL param "sid" and
// verifies the authenticated workspace owns it. Writes 404 or 403 and
// returns ok=false on any failure.
func (s *Server) sessionFromReq(w http.ResponseWriter, r *http.Request) (*processes.Session, bool) {
	sid := chi.URLParam(r, "sid")
	sess, ok := s.Sessions.Get(sid)
	if !ok {
		writeErr(w, http.StatusNotFound, "session_not_found", "no such session: "+sid)
		return nil, false
	}
	if sess.WorkspaceID != workspaceFromCtx(r.Context()) {
		writeErr(w, http.StatusForbidden, "forbidden", "session belongs to a different workspace")
		return nil, false
	}
	return sess, true
}

func (s *Server) handleStdin(w http.ResponseWriter, r *http.Request) {
	_, ok := s.sessionFromReq(w, r)
	if !ok {
		return
	}
	var req stdinReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if _, err := base64.StdEncoding.DecodeString(req.DataB64); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_base64", err.Error())
		return
	}
	// TODO: wire bridge.WriteStdin(session.ExeID, session.ExeSessionID, data).
	// For v0.61.0 the endpoint contract is testable; full bridge integration
	// lands in a follow-up once Session has the exe-side fields wired.
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleOutput(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFromReq(w, r)
	if !ok {
		return
	}
	sinceStr := r.URL.Query().Get("since")
	since, _ := strconv.Atoi(sinceStr)
	chunks, exit, alive := sess.OutputSince(since)
	out := make([]outputChunk, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, outputChunk{
			Stream: c.Stream,
			Data:   base64.StdEncoding.EncodeToString(c.Data),
			Seq:    c.Seq,
		})
	}
	writeJSON(w, map[string]any{
		"chunks":        out,
		"exit_code":     exit,
		"session_alive": alive,
		"truncated":     sess.LostBytes() > 0,
		"lost_bytes":    sess.LostBytes(),
	})
}

func (s *Server) handleTerminate(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessionFromReq(w, r)
	if !ok {
		return
	}
	// For v0.61.0 mark exit -1; bridge.Terminate(...) wiring lands in
	// a follow-up. The endpoint contract works for the SDK's polling
	// pattern (next GET output sees session_alive=false + exit_code=-1).
	sess.SetExit(-1)
	s.Sessions.Forget(sess.ID)
	writeJSON(w, map[string]any{"ok": true})
}
