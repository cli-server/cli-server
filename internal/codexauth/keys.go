package codexauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"math/big"
)

// RSAKeyPair is an RSA-2048 keypair ready for JWKS exposure (public)
// and JWT signing (private). PublicN/PublicE are base64url-no-pad
// encoded for direct JWKS serialization; PrivatePKCS8 is the DER blob
// stored encrypted at rest.
type RSAKeyPair struct {
	PrivateKey   *rsa.PrivateKey
	PrivatePKCS8 []byte
	PublicN      string
	PublicE      string
}

// GenerateRSAKey mints a fresh RSA-2048 keypair and a deterministic kid
// derived from the public modulus prefix. kid stability matters because
// it's embedded in JWT headers and used to look up the verification key
// in JWKS; a random kid forces full JWKS scans.
func GenerateRSAKey() (kid string, kp *RSAKeyPair, err error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", nil, fmt.Errorf("rsa.GenerateKey: %w", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", nil, fmt.Errorf("MarshalPKCS8PrivateKey: %w", err)
	}
	pubN := base64.RawURLEncoding.EncodeToString(priv.N.Bytes())
	pubE := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(priv.E)).Bytes())
	// kid: first 16 chars of the modulus, deterministic and grep-friendly
	kid = "rsa-" + pubN[:16]
	return kid, &RSAKeyPair{
		PrivateKey:   priv,
		PrivatePKCS8: pkcs8,
		PublicN:      pubN,
		PublicE:      pubE,
	}, nil
}

// GenerateEd25519Key produces an Ed25519 keypair used per-agent for
// AgentAssertion signing. PrivatePKCS8 is stored on the client (inside
// the Agent Identity JWT's agent_private_key claim); the public key
// stays server-side for verification.
func GenerateEd25519Key() (privatePKCS8 []byte, public []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("ed25519.GenerateKey: %w", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, fmt.Errorf("MarshalPKCS8PrivateKey: %w", err)
	}
	return pkcs8, pub, nil
}
