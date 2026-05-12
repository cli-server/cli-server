package execgwclient_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/agentserver/agentserver/internal/codexappgateway/execgwclient"
	"github.com/agentserver/agentserver/internal/codexexecgateway/execmodel"
)

func TestListConnected_HappyPath(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	want := []execmodel.ConnectedExecutor{
		{ExeID: "exe_a", Description: "Laptop A", DefaultCwd: "/home/user", IsDefault: true, LastSeenAt: &now},
		{ExeID: "exe_b", Description: "Server B", DefaultCwd: "/srv", IsDefault: false},
	}

	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/exec-gateway/connected" {
			http.NotFound(w, r)
			return
		}
		gotHeader = r.Header.Get("Authorization")
		wid := r.URL.Query().Get("workspace_id")
		if wid == "" {
			http.Error(w, "workspace_id required", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(want) //nolint:errcheck
	}))
	defer srv.Close()

	c := execgwclient.NewClient(srv.URL, "my-shared-secret")
	got, err := c.ListConnected(context.Background(), "ws_test")
	if err != nil {
		t.Fatalf("ListConnected: %v", err)
	}
	if gotHeader != "Bearer my-shared-secret" {
		t.Errorf("Authorization header: got %q, want %q", gotHeader, "Bearer my-shared-secret")
	}
	if len(got) != len(want) {
		t.Fatalf("len: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ExeID != want[i].ExeID {
			t.Errorf("[%d] ExeID: got %q, want %q", i, got[i].ExeID, want[i].ExeID)
		}
		if got[i].Description != want[i].Description {
			t.Errorf("[%d] Description: got %q, want %q", i, got[i].Description, want[i].Description)
		}
		if got[i].DefaultCwd != want[i].DefaultCwd {
			t.Errorf("[%d] DefaultCwd: got %q, want %q", i, got[i].DefaultCwd, want[i].DefaultCwd)
		}
	}
}

func TestListConnected_AuthRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := execgwclient.NewClient(srv.URL, "wrong-secret")
	_, err := c.ListConnected(context.Background(), "ws_test")
	if err == nil {
		t.Fatal("expected error from 401 response, got nil")
	}
	// Error should mention the status code.
	errStr := err.Error()
	if errStr == "" {
		t.Error("error string is empty")
	}
	// The error must describe the 401 status.
	want := "401"
	found := false
	for i := 0; i < len(errStr)-len(want)+1; i++ {
		if errStr[i:i+len(want)] == want {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("error %q does not mention status 401", errStr)
	}
}

func TestListConnected_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`not-valid-json{{{`)) //nolint:errcheck
	}))
	defer srv.Close()

	c := execgwclient.NewClient(srv.URL, "secret")
	_, err := c.ListConnected(context.Background(), "ws_test")
	if err == nil {
		t.Fatal("expected error from malformed JSON, got nil")
	}
}
