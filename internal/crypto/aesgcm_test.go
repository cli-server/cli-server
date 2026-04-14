package crypto

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return key
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := testKey(t)
	plaintext := []byte("hello, credential proxy!")

	ct, err := Encrypt(key, plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := Decrypt(key, ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round-trip mismatch: got %q, want %q", got, plaintext)
	}
}

func TestEncryptDecryptEmptyPlaintext(t *testing.T) {
	key := testKey(t)
	ct, err := Encrypt(key, []byte{})
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := Decrypt(key, ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty plaintext, got %d bytes", len(got))
	}
}

func TestDecryptTampered(t *testing.T) {
	key := testKey(t)
	ct, err := Encrypt(key, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte in the middle of the ciphertext.
	ct[len(ct)/2] ^= 0xff
	if _, err := Decrypt(key, ct); err == nil {
		t.Fatal("expected error on tampered ciphertext")
	}
}

func TestDecryptWrongKey(t *testing.T) {
	key1 := testKey(t)
	key2 := testKey(t)
	ct, err := Encrypt(key1, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decrypt(key2, ct); err == nil {
		t.Fatal("expected error with wrong key")
	}
}

func TestDecryptTooShort(t *testing.T) {
	key := testKey(t)
	if _, err := Decrypt(key, []byte("short")); err == nil {
		t.Fatal("expected error on short ciphertext")
	}
}

func TestLoadKeyFromEnv(t *testing.T) {
	key := testKey(t)
	envVar := "TEST_CREDPROXY_KEY"

	tests := []struct {
		name    string
		value   string
		wantKey []byte // nil means expect error
	}{
		{"hex", hex.EncodeToString(key), key},
		{"base64", base64.StdEncoding.EncodeToString(key), key},
		{"url-safe base64", base64.URLEncoding.EncodeToString(key), key},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envVar, tt.value)

			got, err := LoadKeyFromEnv(envVar)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(got, tt.wantKey) {
				t.Fatal("key mismatch")
			}
		})
	}

	// Passphrase fallback: arbitrary string is derived via SHA-256.
	t.Run("passphrase", func(t *testing.T) {
		passphrase := "my-secret-password-from-pulumi"
		t.Setenv(envVar, passphrase)

		got, err := LoadKeyFromEnv(envVar)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got) != 32 {
			t.Fatalf("expected 32-byte key, got %d bytes", len(got))
		}
		// Same passphrase must produce the same key.
		t.Setenv(envVar, passphrase)
		got2, _ := LoadKeyFromEnv(envVar)
		if !bytes.Equal(got, got2) {
			t.Fatal("passphrase derivation not deterministic")
		}
	})
}

func TestLoadKeyFromEnvMissing(t *testing.T) {
	// Use an env var name that is very unlikely to be set.
	_, err := LoadKeyFromEnv("TEST_CREDPROXY_MISSING_KEY_12345")
	if err == nil {
		t.Fatal("expected error for missing env var")
	}
}
