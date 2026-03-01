package shortid

import (
	"crypto/rand"
	"math/big"
)

const charset = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// Generate returns a cryptographically random 8-character base62 string.
func Generate() string {
	b := make([]byte, 8)
	max := big.NewInt(int64(len(charset)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic("shortid: crypto/rand failed: " + err.Error())
		}
		b[i] = charset[n.Int64()]
	}
	return string(b)
}
