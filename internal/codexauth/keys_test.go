package codexauth

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"testing"
)

func TestGenerateRSAKey_ReturnsUsableKeyMaterial(t *testing.T) {
	kid, kp, err := GenerateRSAKey()
	if err != nil {
		t.Fatalf("GenerateRSAKey: %v", err)
	}
	if kid == "" {
		t.Fatal("kid should not be empty")
	}
	if kp.PrivateKey.N.BitLen() < 2048 {
		t.Errorf("key too small: %d bits", kp.PrivateKey.N.BitLen())
	}
	// Public modulus & exponent serialize round-trippably.
	if _, err := base64.RawURLEncoding.DecodeString(kp.PublicN); err != nil {
		t.Errorf("PublicN not base64url: %v", err)
	}
	if _, err := base64.RawURLEncoding.DecodeString(kp.PublicE); err != nil {
		t.Errorf("PublicE not base64url: %v", err)
	}
	// Private key encoded as PKCS#8 DER.
	parsed, err := x509.ParsePKCS8PrivateKey(kp.PrivatePKCS8)
	if err != nil {
		t.Fatalf("ParsePKCS8PrivateKey: %v", err)
	}
	if _, ok := parsed.(*rsa.PrivateKey); !ok {
		t.Errorf("parsed key is not *rsa.PrivateKey: %T", parsed)
	}
}

func TestGenerateEd25519Key_ReturnsKeypair(t *testing.T) {
	priv, pub, err := GenerateEd25519Key()
	if err != nil {
		t.Fatalf("GenerateEd25519Key: %v", err)
	}
	if len(pub) != 32 {
		t.Errorf("pub len = %d, want 32", len(pub))
	}
	// PKCS#8 DER round-trip.
	parsed, err := x509.ParsePKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("ParsePKCS8PrivateKey: %v", err)
	}
	// crypto/x509.ParsePKCS8PrivateKey returns ed25519.PrivateKey whose
	// Public() returns crypto.PublicKey (a defined type alias for any).
	signer, ok := parsed.(crypto.Signer)
	if !ok {
		t.Fatalf("parsed key is not crypto.Signer: %T", parsed)
	}
	if _, ok := signer.Public().(ed25519.PublicKey); !ok {
		t.Errorf("parsed key's Public() is not ed25519.PublicKey: %T", signer.Public())
	}
}
