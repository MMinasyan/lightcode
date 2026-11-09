package melta

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// mintInstance mints the owner's process-lifetime identity: 16 random bytes
// as 32 lowercase hex.
func mintInstance() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("mint instance identity: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}
