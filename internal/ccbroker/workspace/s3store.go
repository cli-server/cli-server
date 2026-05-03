package workspace

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config carries the S3-compatible endpoint configuration. Endpoint is
// host:port without scheme; UseSSL controls https vs http. PathStyle must
// be true for MinIO and most on-prem S3 implementations; false for AWS S3
// (virtual-hosted-style is required) and most public clouds.
type S3Config struct {
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	UseSSL          bool
	PathStyle       bool
}

// S3Store is the workspace persistence backend. One instance is held by the
// server and shared across all turns.
type S3Store struct {
	client *minio.Client
	bucket string
}

func NewS3Store(cfg S3Config) (*S3Store, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("s3: endpoint required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("s3: bucket required")
	}
	c, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure:       cfg.UseSSL,
		Region:       cfg.Region,
		BucketLookup: bucketLookup(cfg.PathStyle),
	})
	if err != nil {
		return nil, fmt.Errorf("s3: new client: %w", err)
	}
	return &S3Store{client: c, bucket: cfg.Bucket}, nil
}

func bucketLookup(pathStyle bool) minio.BucketLookupType {
	if pathStyle {
		return minio.BucketLookupPath
	}
	return minio.BucketLookupDNS
}

// DownloadTarGz streams the object at key, gunzip-untars it into destDir.
// Returns nil if the object does not exist (treated as empty workspace).
// Tar entries with paths escaping destDir are skipped.
func (s *S3Store) DownloadTarGz(ctx context.Context, key, destDir string) error {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return fmt.Errorf("s3: get object: %w", err)
	}
	defer obj.Close()

	gr, err := gzip.NewReader(obj)
	if err != nil {
		// minio-go's GetObject is lazy: it doesn't return an error until first
		// Read. A 404 surfaces here as a gzip read failure on an XML error doc.
		// Discriminate on the underlying minio error.
		if errResp := minio.ToErrorResponse(err); errResp.Code == "NoSuchKey" {
			return nil
		}
		// gzip.NewReader returned its own error (e.g. "unexpected EOF" on the
		// XML error body). Re-check by stat-ing the object.
		if _, statErr := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); statErr != nil {
			if minio.ToErrorResponse(statErr).Code == "NoSuchKey" {
				return nil
			}
		}
		return fmt.Errorf("s3: gzip reader: %w", err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("s3: tar next: %w", err)
		}
		dest, ok := safeJoin(destDir, hdr.Name)
		if !ok {
			fmt.Fprintf(os.Stderr, "s3: skipping unsafe tar entry: %q\n", hdr.Name)
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return fmt.Errorf("s3: mkdir %s: %w", dest, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				return fmt.Errorf("s3: mkdir parent %s: %w", dest, err)
			}
			f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return fmt.Errorf("s3: create %s: %w", dest, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return fmt.Errorf("s3: copy %s: %w", dest, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("s3: close %s: %w", dest, err)
			}
		default:
			// Skip symlinks, char/block devices, etc.
		}
	}
}

// UploadTarGz walks srcDir, packages it as a tar.gz, and PUTs it to the given
// key. The tarball is buffered in memory so minio-go can issue a single PUT
// request (avoids multipart for the typical small workspace payload). Symlinks
// are skipped. File modes are normalized to 0644 (regular) / 0755 (dir).
// Failures during walk are logged and the offending file is skipped; the
// upload still completes with whatever was packed.
func (s *S3Store) UploadTarGz(ctx context.Context, srcDir, key string) error {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := writeTarball(srcDir, tw); err != nil {
		return fmt.Errorf("s3: build tarball: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("s3: flush tar: %w", err)
	}
	if err := gw.Close(); err != nil {
		return fmt.Errorf("s3: flush gzip: %w", err)
	}

	data := buf.Bytes()
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
		ContentType:          "application/gzip",
		// DisableContentSha256: true sends x-amz-content-sha256: UNSIGNED-PAYLOAD instead
		// of the AWS V4 chunked-streaming encoding. Chunked encoding is incompatible with
		// the httptest fake used in tests, and also adds overhead with no practical benefit
		// when TLS (UseSSL: true in production) already provides transport integrity.
		// This flag is active in production as well — it is not a test-only workaround.
		DisableContentSha256: true,
	})
	if err != nil {
		return fmt.Errorf("s3: put object: %w", err)
	}
	return nil
}

func writeTarball(srcDir string, tw *tar.Writer) error {
	return filepath.WalkDir(srcDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			fmt.Fprintf(os.Stderr, "s3: walk error at %s: %v (skipping)\n", path, walkErr)
			return nil
		}
		if path == srcDir {
			return nil
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			fmt.Fprintf(os.Stderr, "s3: stat %s: %v (skipping)\n", path, err)
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		hdr := &tar.Header{Name: filepath.ToSlash(rel)}
		switch {
		case d.IsDir():
			hdr.Typeflag = tar.TypeDir
			hdr.Mode = 0o755
			hdr.Name += "/"
			if err := tw.WriteHeader(hdr); err != nil {
				return fmt.Errorf("write header %s: %w", rel, err)
			}
		case info.Mode().IsRegular():
			f, err := os.Open(path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "s3: open %s: %v (skipping)\n", path, err)
				return nil
			}
			hdr.Typeflag = tar.TypeReg
			hdr.Mode = 0o644
			hdr.Size = info.Size()
			if err := tw.WriteHeader(hdr); err != nil {
				_ = f.Close()
				return fmt.Errorf("write header %s: %w", rel, err)
			}
			_, err = io.Copy(tw, f)
			_ = f.Close()
			if err != nil {
				return fmt.Errorf("copy %s: %w", rel, err)
			}
		default:
			return nil
		}
		return nil
	})
}

// safeJoin returns the cleaned absolute join of base and rel, plus a bool that
// is false if rel resolves outside base. Rejects absolute paths and any rel
// containing ".." segments that escape base.
func safeJoin(base, rel string) (string, bool) {
	if filepath.IsAbs(rel) {
		return "", false
	}
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "..\\") {
		return "", false
	}
	full := filepath.Join(base, cleaned)
	rel2, err := filepath.Rel(base, full)
	if err != nil || rel2 == ".." || strings.HasPrefix(rel2, "..") {
		return "", false
	}
	return full, true
}
