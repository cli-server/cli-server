package codexhome

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrObjectNotFound is returned by ObjectStore.Get when a key is absent.
var ErrObjectNotFound = errors.New("codexhome: object not found")

// ObjectStore is the seam between codexhome and the S3 client. Real
// callers wire in a thin wrapper around aws-sdk-go-v2; tests use a
// map-backed fake.
type ObjectStore interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
}

// S3Backend round-trips a single (workspace, thread) CODEX_HOME tree.
type S3Backend struct {
	store       ObjectStore
	workspaceID string
	threadID    string
}

func NewS3Backend(store ObjectStore, workspaceID, threadID string) *S3Backend {
	return &S3Backend{store: store, workspaceID: workspaceID, threadID: threadID}
}

// Key is the S3 key. Exposed so callers (and tests) can introspect.
func (b *S3Backend) Key() string {
	return fmt.Sprintf("codex-app-gateway/%s/%s.tar.gz", b.workspaceID, b.threadID)
}

// Upload tars+gzips the directory tree at src and writes it to S3.
func (b *S3Backend) Upload(ctx context.Context, src string) error {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		_ = f.Close()
		return copyErr
	})
	if err != nil {
		return fmt.Errorf("tar walk: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("tar close: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("gz close: %w", err)
	}
	return b.store.Put(ctx, b.Key(), buf.Bytes())
}

// Download fetches the tarball and untars into dst (which must exist
// and be owned by the caller).
func (b *S3Backend) Download(ctx context.Context, dst string) error {
	data, err := b.store.Get(ctx, b.Key())
	if err != nil {
		return err
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}
		if strings.Contains(hdr.Name, "..") {
			return fmt.Errorf("untrusted path: %s", hdr.Name)
		}
		target := filepath.Join(dst, hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			mode := fs.FileMode(hdr.Mode) & 0o700
			if mode == 0 {
				mode = 0o700
			}
			if err := os.MkdirAll(target, mode); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return fmt.Errorf("mkdir parent of %s: %w", target, err)
			}
			mode := fs.FileMode(hdr.Mode) & 0o600
			if mode == 0 {
				mode = 0o600
			}
			f, err := os.OpenFile(target, os.O_RDWR|os.O_CREATE|os.O_TRUNC, mode)
			if err != nil {
				return fmt.Errorf("open %s: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return fmt.Errorf("copy %s: %w", target, err)
			}
			_ = f.Close()
		default:
			// Skip symlinks / fifo / devices — codex doesn't write them.
		}
	}
	return nil
}
