package workspace

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3Config carries the S3-compatible endpoint configuration.
//
// Endpoint is a full URL with scheme — http://... or https://... — so operators
// can use the same form they paste into rclone / aws cli. Trailing path
// segments are not supported. PathStyle must be true for MinIO and most on-prem
// S3 implementations; false for AWS S3 (virtual-hosted-style is required) and
// most public clouds.
type S3Config struct {
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	PathStyle       bool
}

// S3Store is the workspace persistence backend. One instance is held by the
// server and shared across all turns.
type S3Store struct {
	client *s3.Client
	bucket string
}

func NewS3Store(cfg S3Config) (*S3Store, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("s3: endpoint required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("s3: bucket required")
	}
	if _, err := url.Parse(cfg.Endpoint); err != nil {
		return nil, fmt.Errorf("s3: parse endpoint %q: %w", cfg.Endpoint, err)
	}

	client := s3.New(s3.Options{
		Region:       cfg.Region,
		BaseEndpoint: aws.String(cfg.Endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		UsePathStyle: cfg.PathStyle,
	})
	return &S3Store{client: client, bucket: cfg.Bucket}, nil
}

// DownloadTarGz streams the object at key, gunzip-untars it into destDir.
// Returns nil if the object does not exist (treated as empty workspace).
// Tar entries with paths escaping destDir are skipped.
func (s *S3Store) DownloadTarGz(ctx context.Context, key, destDir string) error {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	})
	if err != nil {
		var nsk *s3types.NoSuchKey
		if errors.As(err, &nsk) {
			return nil
		}
		return fmt.Errorf("s3: get object %s: %w", key, err)
	}
	defer out.Body.Close()

	gr, err := gzip.NewReader(out.Body)
	if err != nil {
		return fmt.Errorf("s3: corrupt tar.gz at %s: %w", key, err)
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
// key. The tarball is buffered in memory so the SDK can issue a single PUT
// request (avoids multipart for the typical small workspace payload). Symlinks
// are skipped. File modes are normalized to 0644 (regular) / 0755 (dir).
// Failures during walk are logged and the offending file is skipped; the
// upload still completes with whatever was packed.
//
// excludeRel, if non-nil, returns true for any rel-path under srcDir that
// should be omitted from the tarball. Used by callers that store some
// subdirectories as separate S3 objects (per-session jsonl).
func (s *S3Store) UploadTarGz(ctx context.Context, srcDir, key string, excludeRel func(rel string) bool) error {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := writeTarball(srcDir, tw, excludeRel); err != nil {
		return fmt.Errorf("s3: build tarball: %w", err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("s3: flush tar: %w", err)
	}
	if err := gw.Close(); err != nil {
		return fmt.Errorf("s3: flush gzip: %w", err)
	}

	body := bytes.NewReader(buf.Bytes())
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        &s.bucket,
		Key:           &key,
		Body:          body,
		ContentLength: aws.Int64(int64(body.Len())),
		ContentType:   aws.String("application/gzip"),
	})
	if err != nil {
		return fmt.Errorf("s3: put object %s: %w", key, err)
	}
	return nil
}

func writeTarball(srcDir string, tw *tar.Writer, excludeRel func(rel string) bool) error {
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
		relSlash := filepath.ToSlash(rel)
		if excludeRel != nil && excludeRel(relSlash) {
			if d.IsDir() {
				return filepath.SkipDir
			}
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
