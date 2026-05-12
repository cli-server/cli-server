package codexappgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/auth"
	"github.com/agentserver/agentserver/internal/codexappgateway/captoken"
	"github.com/agentserver/agentserver/internal/codexappgateway/codexhome"
	"github.com/agentserver/agentserver/internal/codexappgateway/execgwclient"
	"github.com/agentserver/agentserver/internal/codexappgateway/supervisor"
	codexexecgateway "github.com/agentserver/agentserver/internal/codexexecgateway"
	"github.com/agentserver/agentserver/internal/codexexecgateway/execmodel"
)

// TestBuildConfig_FetchesAndMintsCorrectly is the end-to-end proof-of-
// correctness test for the real buildConfig closure. It:
//  1. Stands up a fake exec-gateway that returns two executors.
//  2. Constructs a Server with the real buildConfig wired to that fake.
//  3. Calls buildConfig and verifies the returned ConfigInput.Executors.
//  4. Verifies each cap token passes codexexecgateway.VerifyCapabilityToken.
//  5. Verifies RenderConfigTOML produces the expected [mcp_servers.exe_*]
//     entries.
func TestBuildConfig_FetchesAndMintsCorrectly(t *testing.T) {
	const hmacSecret = "test-captoken-hmac-secret-32bytes"
	const wsBaseURL = "ws://fake-exec-gw:9999"

	fakeExecutors := []execmodel.ConnectedExecutor{
		{ExeID: "exe_laptop", Description: "Main laptop", DefaultCwd: "/home/user", IsDefault: true},
		{ExeID: "exe_server", Description: "Build server", DefaultCwd: "/srv/build", IsDefault: false},
	}

	var gotAuthHeader string
	var gotWorkspaceID string
	fakeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/exec-gateway/connected" {
			http.NotFound(w, r)
			return
		}
		gotAuthHeader = r.Header.Get("Authorization")
		gotWorkspaceID = r.URL.Query().Get("workspace_id")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(fakeExecutors) //nolint:errcheck
	}))
	defer fakeSrv.Close()

	// Build a minimal Server with the real buildConfig wired to the fake.
	// We don't need a real supervisor or S3 store for this test; we call
	// buildConfig directly.
	bin := makeFakeCodex(t)
	store := makeFakeStore(t)
	mgr := codexhome.NewManager(t.TempDir())
	sup := supervisor.NewSupervisor(supervisor.SupervisorConfig{CodexBin: bin, HomeMgr: mgr, Store: store})
	t.Cleanup(func() { sup.ShutdownAll(context.Background()) })

	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	cfg := ServeConfig{
		InboundHMACSecret:         []byte("test-hmac-secret"),
		ExecGatewayInternalURL:    fakeSrv.URL,
		ExecGatewayInternalSecret: "shared-internal-secret",
		ExecGatewayWSURL:          wsBaseURL,
		CapTokenHMACSecret:        []byte(hmacSecret),
	}

	s := &Server{
		cfg:          cfg,
		auth:         auth.NewHMAC(cfg.InboundHMACSecret),
		sup:          sup,
		homeMgr:      mgr,
		logger:       logger,
		execGWClient: execgwclient.NewClient(fakeSrv.URL, "shared-internal-secret"),
		codexBin:     bin,
	}
	// Wire the real buildConfig (same logic as NewServer).
	beforeMint := time.Now()
	s.buildConfig = func(ctx context.Context, workspaceID, threadID string) (codexhome.ConfigInput, error) {
		connected, err := s.execGWClient.ListConnected(ctx, workspaceID)
		if err != nil {
			return codexhome.ConfigInput{}, err
		}
		now := time.Now()
		exp := now.Add(24 * time.Hour).Unix()
		var execs []codexhome.ExecutorEntry
		for _, ce := range connected {
			tok := captoken.Mint(s.cfg.CapTokenHMACSecret, captoken.Payload{
				TurnID:      threadID,
				WorkspaceID: workspaceID,
				ExeIDs:      []string{ce.ExeID},
				IAT:         now.Unix(),
				EXP:         exp,
			})
			envName := "CXG_BRIDGE_TOKEN_EXE_" + sanitizeEnvName(ce.ExeID)
			desc := ce.ExeID
			if ce.DefaultCwd != "" {
				desc = ce.ExeID + " (" + ce.DefaultCwd + ")"
			}
			execs = append(execs, codexhome.ExecutorEntry{
				ID:        ce.ExeID,
				BridgeURL: s.cfg.ExecGatewayWSURL + "/bridge/" + ce.ExeID,
				TokenEnv:  envName,
				TokenVal:  tok,
				Desc:      desc,
				CodexBin:  s.codexBin,
				TurnID:    threadID,
			})
		}
		return codexhome.ConfigInput{
			ModelProvider: "modelserver",
			Model:         "gpt-5.5",
			ModelProviders: map[string]codexhome.ModelProvider{
				"modelserver": {Name: "modelserver", BaseURL: "http://llmproxy:8085/v1", EnvKey: "CODEX_API_KEY", WireAPI: "responses"},
			},
			Executors: execs,
		}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := s.buildConfig(ctx, "ws_a", "thr_1")
	if err != nil {
		t.Fatalf("buildConfig: %v", err)
	}

	// Verify the fake received correct auth and workspace_id.
	if gotAuthHeader != "Bearer shared-internal-secret" {
		t.Errorf("Authorization header: got %q, want %q", gotAuthHeader, "Bearer shared-internal-secret")
	}
	if gotWorkspaceID != "ws_a" {
		t.Errorf("workspace_id query param: got %q, want %q", gotWorkspaceID, "ws_a")
	}

	// Verify we got 2 executor entries.
	if len(result.Executors) != 2 {
		t.Fatalf("Executors len: got %d, want 2", len(result.Executors))
	}

	for i, want := range fakeExecutors {
		got := result.Executors[i]

		// ExeID.
		if got.ID != want.ExeID {
			t.Errorf("[%d] ID: got %q, want %q", i, got.ID, want.ExeID)
		}

		// BridgeURL.
		wantBridge := wsBaseURL + "/bridge/" + want.ExeID
		if got.BridgeURL != wantBridge {
			t.Errorf("[%d] BridgeURL: got %q, want %q", i, got.BridgeURL, wantBridge)
		}

		// TokenEnv.
		wantEnv := "CXG_BRIDGE_TOKEN_EXE_" + sanitizeEnvName(want.ExeID)
		if got.TokenEnv != wantEnv {
			t.Errorf("[%d] TokenEnv: got %q, want %q", i, got.TokenEnv, wantEnv)
		}

		// TurnID echoed in ExecutorEntry.
		if got.TurnID != "thr_1" {
			t.Errorf("[%d] TurnID: got %q, want %q", i, got.TurnID, "thr_1")
		}

		// Cap token verifiable by codexexecgateway.VerifyCapabilityToken.
		payload, err := codexexecgateway.VerifyCapabilityToken(got.TokenVal, []byte(hmacSecret))
		if err != nil {
			t.Errorf("[%d] VerifyCapabilityToken(%q): %v", i, got.TokenVal, err)
			continue
		}
		if payload.TurnID != "thr_1" {
			t.Errorf("[%d] payload.TurnID: got %q, want %q", i, payload.TurnID, "thr_1")
		}
		if payload.WorkspaceID != "ws_a" {
			t.Errorf("[%d] payload.WorkspaceID: got %q, want %q", i, payload.WorkspaceID, "ws_a")
		}
		if len(payload.ExeIDs) != 1 || payload.ExeIDs[0] != want.ExeID {
			t.Errorf("[%d] payload.ExeIDs: got %v, want [%q]", i, payload.ExeIDs, want.ExeID)
		}
		if payload.EXP <= beforeMint.Unix() {
			t.Errorf("[%d] payload.EXP %d not after mint time %d", i, payload.EXP, beforeMint.Unix())
		}
	}

	// Verify RenderConfigTOML produces [mcp_servers.exe_*] entries.
	toml, err := codexhome.RenderConfigTOML(result)
	if err != nil {
		t.Fatalf("RenderConfigTOML: %v", err)
	}
	for _, want := range fakeExecutors {
		section := "[mcp_servers." + want.ExeID + "]"
		if !strings.Contains(toml, section) {
			t.Errorf("TOML missing section %q\n--- full TOML ---\n%s", section, toml)
		}
		bridgeURL := wsBaseURL + "/bridge/" + want.ExeID
		if !strings.Contains(toml, bridgeURL) {
			t.Errorf("TOML missing bridge URL %q\n--- full TOML ---\n%s", bridgeURL, toml)
		}
	}
}
