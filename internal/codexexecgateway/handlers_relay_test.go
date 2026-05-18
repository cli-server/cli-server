package codexexecgateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// newRelayTestServer spins up an httptest.Server with the relay routes
// wired and a non-empty PublicHTTPSBaseURL so the registry is enabled.
// Uses the no-store testing constructor (which means handleRelayCreate's
// ownership check is skipped — fine for relay surface tests).
func newRelayTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	cfg := Config{
		CapTokenHMACSecret:   []byte("test-hmac"),
		InternalSharedSecret: "test-secret",
		PublicHTTPSBaseURL:   "https://test.example/", // value doesn't matter for tests
		RelayDefaultTTL:      time.Second,
		RelayMaxPerWorkspace: 4,
	}
	srv, err := newServerNoStoreForTesting(cfg)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts, srv.config.InternalSharedSecret
}

// mintTicket is a small helper that POSTs to the internal create endpoint
// and returns the ticket from the response.
func mintTestTicket(t *testing.T, ts *httptest.Server, secret, ws, src, dst string) string {
	t.Helper()
	body, _ := json.Marshal(relayCreateRequest{
		WorkspaceID: ws, SourceExeID: src, DestExeID: dst,
	})
	req, _ := http.NewRequest("POST", ts.URL+"/api/exec-gateway/relay/create", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create status=%d body=%s", resp.StatusCode, b)
	}
	var got relayCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Ticket == "" || got.UploadURL == "" || got.DownloadURL == "" {
		t.Fatalf("create response missing fields: %+v", got)
	}
	return got.Ticket
}

func TestHandleRelay_RoundTrip(t *testing.T) {
	ts, secret := newRelayTestServer(t)
	ticket := mintTestTicket(t, ts, secret, "ws", "exe_src", "exe_dst")

	payload := bytes.Repeat([]byte("ABCDEFGH"), 16*1024) // 128 KiB
	url := ts.URL + "/relay/" + ticket

	// GET in parallel with PUT — order doesn't matter, pairing handles it.
	var (
		wg      sync.WaitGroup
		putErr  error
		putBody []byte
		getErr  error
		getBody []byte
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		req, _ := http.NewRequest("PUT", url, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+ticket)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			putErr = err
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			putErr = &httpStatusErr{code: resp.StatusCode, body: string(b)}
			return
		}
		putBody, _ = io.ReadAll(resp.Body)
	}()
	go func() {
		defer wg.Done()
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer "+ticket)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			getErr = err
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			getErr = &httpStatusErr{code: resp.StatusCode, body: string(b)}
			return
		}
		getBody, _ = io.ReadAll(resp.Body)
	}()
	wg.Wait()

	if putErr != nil {
		t.Fatalf("PUT: %v", putErr)
	}
	if getErr != nil {
		t.Fatalf("GET: %v", getErr)
	}
	if !bytes.Equal(getBody, payload) {
		t.Errorf("body mismatch: got %d bytes, want %d", len(getBody), len(payload))
	}
	if !strings.Contains(string(putBody), `"status":"ok"`) {
		t.Errorf("put body lacks ok status: %s", putBody)
	}
}

func TestHandleRelay_TicketNotFound(t *testing.T) {
	ts, _ := newRelayTestServer(t)
	url := ts.URL + "/relay/rly_doesnotexist"
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer rly_doesnotexist")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Errorf("status = %d, want 410 Gone", resp.StatusCode)
	}
}

func TestHandleRelay_TicketMismatch(t *testing.T) {
	ts, secret := newRelayTestServer(t)
	ticket := mintTestTicket(t, ts, secret, "ws", "a", "b")
	req, _ := http.NewRequest("GET", ts.URL+"/relay/"+ticket, nil)
	req.Header.Set("Authorization", "Bearer rly_other")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestHandleRelay_CreateAuthRequired(t *testing.T) {
	ts, _ := newRelayTestServer(t)
	body, _ := json.Marshal(relayCreateRequest{
		WorkspaceID: "ws", SourceExeID: "a", DestExeID: "b",
	})
	resp, err := http.Post(ts.URL+"/api/exec-gateway/relay/create",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-auth status = %d, want 401", resp.StatusCode)
	}
}

func TestHandleRelay_CreateBadJSON(t *testing.T) {
	ts, secret := newRelayTestServer(t)
	req, _ := http.NewRequest("POST",
		ts.URL+"/api/exec-gateway/relay/create",
		strings.NewReader("not json"))
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad-json status = %d, want 400", resp.StatusCode)
	}
}

func TestHandleRelay_CreateMissingFields(t *testing.T) {
	ts, secret := newRelayTestServer(t)
	body, _ := json.Marshal(relayCreateRequest{WorkspaceID: "ws"}) // no src/dst
	req, _ := http.NewRequest("POST",
		ts.URL+"/api/exec-gateway/relay/create",
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// Server with PublicHTTPSBaseURL unset disables the relay surface.
func TestHandleRelay_Disabled(t *testing.T) {
	cfg := Config{
		CapTokenHMACSecret:   []byte("test-hmac"),
		InternalSharedSecret: "test-secret",
		// PublicHTTPSBaseURL deliberately unset
	}
	srv, err := newServerNoStoreForTesting(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	// /relay/<x> should 404.
	req, _ := http.NewRequest("GET", ts.URL+"/relay/anything", nil)
	req.Header.Set("Authorization", "Bearer anything")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("disabled GET status = %d, want 404", resp.StatusCode)
	}

	// create should 503.
	body, _ := json.Marshal(relayCreateRequest{WorkspaceID: "w", SourceExeID: "a", DestExeID: "b"})
	req, _ = http.NewRequest("POST",
		ts.URL+"/api/exec-gateway/relay/create",
		bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("disabled create status = %d, want 503", resp.StatusCode)
	}
}

type httpStatusErr struct {
	code int
	body string
}

func (e *httpStatusErr) Error() string {
	return "http " + http.StatusText(e.code) + ": " + e.body
}
