package shortid

import (
	"crypto/rand"
	"math/big"
)

// charset is lowercase alphanumeric only (base36) because subdomains are
// case-insensitive — browsers and DNS normalise them to lowercase.
const charset = "0123456789abcdefghijklmnopqrstuvwxyz"

// Generate returns a cryptographically random 16-character base36 string.
func Generate() string {
	b := make([]byte, 16)
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
