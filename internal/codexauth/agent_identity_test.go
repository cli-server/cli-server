package codexauth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

func TestMintAgentIdentity_PersistsRow(t *testing.T) {
	srv := newAuthTestServer(t, "")
	uid := mustCreateTestUser(t, srv.Store.db)

	res, err := srv.MintAgentIdentity(context.Background(), MintAgentIdentityArgs{
		AgentRuntimeID: "exe_test_mint",
		UserID:         uid,
		Email:          "u@test",
	})
	if err != nil {
		t.Fatalf("MintAgentIdentity: %v", err)
	}
	if !strings.HasPrefix(res.JWT, "eyJ") {
		t.Errorf("JWT looks bogus: %.30s...", res.JWT)
	}
	got, _ := srv.Store.GetAgentIdentity(context.Background(), "exe_test_mint")
	if got == nil || len(got.PublicKey) != 32 {
		t.Errorf("agent identity row missing or bad pubkey: %+v", got)
	}
}

func TestTaskRegister_VerifiesEd25519AndIssuesTaskID(t *testing.T) {
	srv := newAuthTestServer(t, "")
	r := chi.NewRouter()
	srv.Mount(r)
	uid := mustCreateTestUser(t, srv.Store.db)

	mint, err := srv.MintAgentIdentity(context.Background(), MintAgentIdentityArgs{
		AgentRuntimeID: "exe_task_reg",
		UserID:         uid,
		Email:          "u@test",
	})
	if err != nil {
		t.Fatalf("MintAgentIdentity: %v", err)
	}

	ts := time.Now().UTC().Format(time.RFC3339)
	sig := ed25519.Sign(mint.privKey, []byte("exe_task_reg:"+ts))
	body, _ := json.Marshal(map[string]string{
		"timestamp": ts,
		"signature": base64.StdEncoding.EncodeToString(sig),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/agent/exe_task_reg/task/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		TaskID string `json:"task_id"`
	}
	json.NewDecoder(rr.Body).Decode(&resp)
	if resp.TaskID == "" {
		t.Error("missing task_id")
	}
}

func TestTaskRegister_BadSignatureRejected(t *testing.T) {
	srv := newAuthTestServer(t, "")
	r := chi.NewRouter()
	srv.Mount(r)
	uid := mustCreateTestUser(t, srv.Store.db)
	if _, err := srv.MintAgentIdentity(context.Background(), MintAgentIdentityArgs{
		AgentRuntimeID: "exe_bad_sig", UserID: uid, Email: "u@test",
	}); err != nil {
		t.Fatalf("MintAgentIdentity: %v", err)
	}

	body, _ := json.Marshal(map[string]string{
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"signature": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0}, 64)),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/agent/exe_bad_sig/task/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rr.Code)
	}
}
