package discovery

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

func newPeerHint() (string, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate discovery peer hint: %w", err)
	}
	return hex.EncodeToString(random[:]), nil
}
