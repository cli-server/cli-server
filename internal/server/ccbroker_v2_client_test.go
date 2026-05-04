package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCcbrokerV2Submit_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/turns" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if r.Method != "POST" {
			t.Errorf("method=%q", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content-type=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"turn_id":"trn_xyz","events_url":"/api/turns/trn_xyz/events"}`))
	}))
	defer srv.Close()

	tid, err := ccbrokerV2Submit(context.Background(), srv.URL, []byte(`{"session_id":"s","workspace_id":"w","user_message":"hi"}`))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if tid != "trn_xyz" {
		t.Fatalf("turn_id=%q", tid)
	}
}

func TestCcbrokerV2Submit_Returns429(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"too many"}`))
	}))
	defer srv.Close()
	_, err := ccbrokerV2Submit(context.Background(), srv.URL, []byte(`{}`))
	if err == nil {
		t.Fatal("expected error on 429")
	}
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error should mention 429: %v", err)
	}
}

func TestCcbrokerV2Submit_MalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"turn_id":""}`)) // empty turn_id
	}))
	defer srv.Close()
	_, err := ccbrokerV2Submit(context.Background(), srv.URL, []byte(`{}`))
	if err == nil {
		t.Fatal("expected error on empty turn_id")
	}
}

func TestCcbrokerOpenEventStream_StreamsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/turns/") || !strings.HasSuffix(r.URL.Path, "/events") {
			t.Errorf("path=%q", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("accept=%q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {}\n\n"))
	}))
	defer srv.Close()
	rc, err := ccbrokerOpenEventStream(context.Background(), srv.URL, "trn_x")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if !strings.Contains(string(data), "data: {}") {
		t.Fatalf("body=%q", data)
	}
}

func TestCcbrokerOpenEventStream_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"code":"not_found"}`))
	}))
	defer srv.Close()
	_, err := ccbrokerOpenEventStream(context.Background(), srv.URL, "trn_unknown")
	if err == nil {
		t.Fatal("expected error on 404")
	}
}
