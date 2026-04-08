package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveAndLoadCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".credentials.json")

	creds := &Credentials{
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		ExpiresAt:    time.Date(2026, 4, 7, 12, 0, 0, 0, time.UTC),
		Scopes:       []string{"openid", "profile", "agent:register"},
	}

	if err := SaveCredentials(path, creds); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}

	loaded, err := LoadCredentials(path)
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}

	if loaded.AccessToken != creds.AccessToken {
		t.Errorf("AccessToken = %q, want %q", loaded.AccessToken, creds.AccessToken)
	}
	if loaded.RefreshToken != creds.RefreshToken {
		t.Errorf("RefreshToken = %q, want %q", loaded.RefreshToken, creds.RefreshToken)
	}
	if !loaded.ExpiresAt.Equal(creds.ExpiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", loaded.ExpiresAt, creds.ExpiresAt)
	}
}

func TestLoadCredentials_NotExist(t *testing.T) {
	creds, err := LoadCredentials("/nonexistent/.credentials.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if creds != nil {
		t.Errorf("expected nil credentials for missing file, got %+v", creds)
	}
}

func TestDefaultCredentialsPath(t *testing.T) {
	path := DefaultCredentialsPath()
	if filepath.Base(path) != ".credentials.json" {
		t.Errorf("expected .credentials.json, got %s", filepath.Base(path))
	}
}

func TestSaveCredentials_CreatesDir(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "sub", "deep", ".credentials.json")

	creds := &Credentials{
		AccessToken:  "tok",
		RefreshToken: "ref",
		ExpiresAt:    time.Now(),
	}

	if err := SaveCredentials(nested, creds); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}

	info, err := os.Stat(nested)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("permissions = %o, want 0600", perm)
	}
}
