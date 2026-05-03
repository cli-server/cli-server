package workspace

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeS3 serves the minimal subset of the S3 API that S3Store touches.
//
//	GET  /<bucket>/<key>  → bytes the test pre-loaded, or 404 NoSuchKey
//	PUT  /<bucket>/<key>  → captures bytes into uploads
type fakeS3 struct {
	bucket  string
	objects map[string][]byte // key → content (pre-loaded responses)
	uploads map[string][]byte // key → content (captured PUTs)
	failPUT bool              // if true, PUT returns 500 (used by Teardown failure tests)
}

func newFakeS3(bucket string) *fakeS3 {
	return &fakeS3{
		bucket:  bucket,
		objects: make(map[string][]byte),
		uploads: make(map[string][]byte),
	}
}

func (f *fakeS3) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Path-style: /<bucket>/<key>
		path := strings.TrimPrefix(r.URL.Path, "/")
		path, _ = url.PathUnescape(path)
		path = strings.TrimPrefix(path, f.bucket+"/")

		switch r.Method {
		case http.MethodGet, http.MethodHead:
			data, ok := f.objects[path]
			if !ok {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`<?xml version="1.0"?><Error><Code>NoSuchKey</Code></Error>`))
				return
			}
			// minio-go requires Last-Modified in RFC 822 format or it returns an error.
			w.Header().Set("Last-Modified", "Mon, 1 Jan 2024 00:00:00 GMT")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
		case http.MethodPut:
			if f.failPUT {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			body, _ := io.ReadAll(r.Body)
			f.uploads[path] = body
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

// makeTarGz builds an in-memory tar.gz from the given path→content map.
func makeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = gw.Close()
	return buf.Bytes()
}

func newTestStore(t *testing.T, fake *fakeS3) (*S3Store, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(fake.handler())
	u, _ := url.Parse(srv.URL)
	store, err := NewS3Store(S3Config{
		Endpoint:        u.Host,
		Region:          "us-east-1",
		Bucket:          fake.bucket,
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		UseSSL:          false,
		PathStyle:       true,
	})
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}
	return store, srv
}

func TestDownloadTarGz_HappyPath(t *testing.T) {
	fake := newFakeS3("ccbroker")
	fake.objects["workspaces/ws1/claude-home.tar.gz"] = makeTarGz(t, map[string]string{
		"CLAUDE.md":               "global-instructions",
		"projects/p/session.jsonl": "line1\nline2\n",
	})

	store, srv := newTestStore(t, fake)
	defer srv.Close()

	dest := t.TempDir()
	if err := store.DownloadTarGz(context.Background(), "workspaces/ws1/claude-home.tar.gz", dest); err != nil {
		t.Fatalf("DownloadTarGz: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "CLAUDE.md"))
	if err != nil || string(got) != "global-instructions" {
		t.Fatalf("CLAUDE.md mismatch: got=%q err=%v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(dest, "projects/p/session.jsonl"))
	if err != nil || string(got) != "line1\nline2\n" {
		t.Fatalf("session.jsonl mismatch: got=%q err=%v", got, err)
	}
}

func TestDownloadTarGz_NotFoundIsEmpty(t *testing.T) {
	fake := newFakeS3("ccbroker")
	// no objects pre-loaded → every GET returns 404
	store, srv := newTestStore(t, fake)
	defer srv.Close()

	dest := t.TempDir()
	if err := store.DownloadTarGz(context.Background(), "workspaces/missing/claude-home.tar.gz", dest); err != nil {
		t.Fatalf("DownloadTarGz on missing key: want nil, got %v", err)
	}
	// destDir should remain empty
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("destDir should be empty, got %d entries", len(entries))
	}
}

func TestDownloadTarGz_RejectsPathTraversal(t *testing.T) {
	fake := newFakeS3("ccbroker")
	fake.objects["workspaces/ws1/claude-home.tar.gz"] = makeTarGz(t, map[string]string{
		"../escape.txt":   "should-not-write",
		"/abs/escape.txt": "should-not-write",
		"safe.txt":        "ok",
	})

	store, srv := newTestStore(t, fake)
	defer srv.Close()

	dest := t.TempDir()
	parent := filepath.Dir(dest)

	if err := store.DownloadTarGz(context.Background(), "workspaces/ws1/claude-home.tar.gz", dest); err != nil {
		t.Fatalf("DownloadTarGz: %v", err)
	}

	// Safe entry written
	if _, err := os.Stat(filepath.Join(dest, "safe.txt")); err != nil {
		t.Fatalf("safe.txt missing: %v", err)
	}
	// Escape attempts must NOT have written outside dest
	if _, err := os.Stat(filepath.Join(parent, "escape.txt")); !os.IsNotExist(err) {
		t.Fatalf("traversal write succeeded; want IsNotExist, got %v", err)
	}
	if _, err := os.Stat("/abs/escape.txt"); !os.IsNotExist(err) {
		t.Fatalf("absolute-path write succeeded; want IsNotExist, got %v", err)
	}
}

func TestUploadTarGz_RoundTrip(t *testing.T) {
	fake := newFakeS3("ccbroker")
	store, srv := newTestStore(t, fake)
	defer srv.Close()

	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "CLAUDE.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "skills", "foo.md"), []byte("a skill"), 0o644); err != nil {
		t.Fatal(err)
	}

	key := "workspaces/ws1/claude-home.tar.gz"
	if err := store.UploadTarGz(context.Background(), src, key); err != nil {
		t.Fatalf("UploadTarGz: %v", err)
	}

	// Round-trip: stage the captured upload as if it were a pre-existing object,
	// then download into a fresh dir and compare.
	uploaded, ok := fake.uploads[key]
	if !ok {
		t.Fatalf("no upload captured; uploads=%v", keysOf(fake.uploads))
	}
	fake.objects[key] = uploaded

	dest := t.TempDir()
	if err := store.DownloadTarGz(context.Background(), key, dest); err != nil {
		t.Fatalf("DownloadTarGz: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dest, "CLAUDE.md"))
	if string(got) != "hi" {
		t.Fatalf("CLAUDE.md round-trip mismatch: %q", got)
	}
	got, _ = os.ReadFile(filepath.Join(dest, "skills", "foo.md"))
	if string(got) != "a skill" {
		t.Fatalf("skills/foo.md round-trip mismatch: %q", got)
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestUploadTarGz_SkipsSymlinks(t *testing.T) {
	fake := newFakeS3("ccbroker")
	store, srv := newTestStore(t, fake)
	defer srv.Close()

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "real.txt"), []byte("real"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	key := "workspaces/ws1/claude-home.tar.gz"
	if err := store.UploadTarGz(context.Background(), src, key); err != nil {
		t.Fatalf("UploadTarGz: %v", err)
	}

	// Inspect the captured upload: walk the tar entries by name.
	uploaded := fake.uploads[key]
	gr, err := gzip.NewReader(bytes.NewReader(uploaded))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)
	names := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		names[hdr.Name] = true
	}
	if !names["real.txt"] {
		t.Fatalf("real.txt missing from upload; names=%v", names)
	}
	if names["link"] {
		t.Fatalf("symlink entry should have been skipped; names=%v", names)
	}
}
