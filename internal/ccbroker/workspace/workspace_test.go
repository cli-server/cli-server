package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeViking serves the minimum OpenViking surface Setup/Teardown need.
// Tracks uploads so tests can assert what got pushed back.
//
// Handles:
//   - GET  /api/v1/fs/ls?uri=...          — DownloadTree listing
//   - GET  /api/v1/content/read?uri=...   — DownloadTree file fetch
//   - POST /api/v1/content/write          — UploadFile (modified files)
//   - POST /api/v1/resources/temp_upload  — CreateFile step 1 (new files)
//   - POST /api/v1/resources              — CreateFile step 2 (new files)
type fakeViking struct {
	uploads map[string]string // vikingURI → content (populated by both upload paths)
	tree    map[string]string // vikingURI → content (initial state served by ls/read)

	// internal state for the two-step CreateFile protocol
	nextTempID int
	tempFiles  map[string][]byte // tempFileID → raw bytes
}

func newFakeViking() *fakeViking {
	return &fakeViking{
		uploads:   make(map[string]string),
		tree:      make(map[string]string),
		tempFiles: make(map[string][]byte),
	}
}

func (f *fakeViking) handler() http.Handler {
	mux := http.NewServeMux()

	// DownloadTree: list files under a URI prefix
	mux.HandleFunc("/api/v1/fs/ls", func(w http.ResponseWriter, r *http.Request) {
		uri, _ := url.QueryUnescape(r.URL.Query().Get("uri"))
		var entries []map[string]any
		for u := range f.tree {
			if !strings.HasPrefix(u, uri) {
				continue
			}
			rel := strings.TrimPrefix(u, uri)
			entries = append(entries, map[string]any{
				"name":     filepath.Base(rel),
				"isDir":    false,
				"uri":      u,
				"rel_path": rel,
			})
		}
		writeViking(w, entries)
	})

	// DownloadTree: read a single file's content
	mux.HandleFunc("/api/v1/content/read", func(w http.ResponseWriter, r *http.Request) {
		uri, _ := url.QueryUnescape(r.URL.Query().Get("uri"))
		writeViking(w, f.tree[uri])
	})

	// UploadFile: write to an existing URI (modified files in Teardown)
	mux.HandleFunc("/api/v1/content/write", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			URI     string `json:"uri"`
			Content string `json:"content"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.uploads[body.URI] = body.Content
		writeViking(w, "ok")
	})

	// CreateFile step 1: receive raw bytes, assign a temp_file_id
	mux.HandleFunc("/api/v1/resources/temp_upload", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(32 << 20)
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		defer file.Close()

		buf := make([]byte, 1<<20)
		n, _ := file.Read(buf)
		f.nextTempID++
		id := fmt.Sprintf("tmp-%d", f.nextTempID)
		f.tempFiles[id] = buf[:n]

		writeViking(w, map[string]any{"temp_file_id": id})
	})

	// CreateFile step 2: associate temp file with its final URI
	mux.HandleFunc("/api/v1/resources", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			TempFileID string `json:"temp_file_id"`
			To         string `json:"to"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if data, ok := f.tempFiles[body.TempFileID]; ok {
			f.uploads[body.To] = string(data)
			delete(f.tempFiles, body.TempFileID)
		}
		writeViking(w, "ok")
	})

	return mux
}

func writeViking(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "result": result})
}

func TestSetupAndTeardown(t *testing.T) {
	fv := newFakeViking()
	// Pre-populate one file so DownloadTree has something to fetch.
	fv.tree["viking://resources/workspace_ws1/claude-home/CLAUDE.md"] = "global-claude"

	srv := httptest.NewServer(fv.handler())
	defer srv.Close()

	vc := NewVikingClient(srv.URL, "")
	ctx := context.Background()

	ws, err := Setup(ctx, "ws1", "cse_abc", vc)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

	// Downloaded file should now be in ClaudeDir.
	got, err := os.ReadFile(filepath.Join(ws.ClaudeDir, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("read downloaded: %v", err)
	}
	if string(got) != "global-claude" {
		t.Fatalf("downloaded content mismatch: %q", got)
	}

	// Memory dir created at the deterministic path.
	wantMem := filepath.Join(ws.ClaudeDir, "projects", "ws_ws1", "memory")
	if _, err := os.Stat(wantMem); err != nil {
		t.Fatalf("memory dir missing: %v", err)
	}

	// Mutate one tracked file (modified) + add a new one (added).
	// Both should be uploaded on Teardown.
	if err := os.WriteFile(filepath.Join(ws.ClaudeDir, "CLAUDE.md"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws.MemoryDir, "MEMORY.md"), []byte("note"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Teardown(ctx, ws, vc); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	// TempDir must be gone after Teardown.
	if _, err := os.Stat(ws.TempDir); !os.IsNotExist(err) {
		t.Fatalf("TempDir should be removed; err=%v", err)
	}

	// Exactly 2 uploads: CLAUDE.md (modified) and memory/MEMORY.md (added).
	if len(fv.uploads) != 2 {
		t.Fatalf("expected 2 uploads, got %d: %v", len(fv.uploads), keys(fv.uploads))
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
