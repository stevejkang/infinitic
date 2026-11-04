// Package credstore reads and writes credential blobs in the places Claude Code looks for them:
// the macOS keychain, or 0600 files on Linux (the same model Claude Code itself uses there).
package credstore

import (
	"crypto/sha256"
	"encoding/hex"
)

// Slot names one place a credential blob can live.
type Slot struct {
	key   string // keychain service name, or file path
	Label string // human-readable, for messages
}

// Store is a credential backend.
type Store interface {
	// Read returns the stored blob and whether one exists.
	Read(Slot) (string, bool, error)
	// Write stores the blob atomically and verifies it reads back identically.
	Write(Slot, string) error
	// Delete removes the blob. Deleting a missing slot is not an error.
	Delete(Slot) error

	// Primary is the login claude and tools that share its login use by default.
	Primary() Slot
	// Pool is where infinitic parks an inactive account.
	Pool(accountID string) Slot
	// Isolated is the slot claude uses when started with CLAUDE_CONFIG_DIR=configDir.
	Isolated(configDir string) Slot
}

// claudeServiceFor mirrors Claude Code's keychain naming for a non-default config dir.
func claudeServiceFor(primaryService, configDir string) string {
	sum := sha256.Sum256([]byte(configDir))
	return primaryService + "-" + hex.EncodeToString(sum[:])[:8]
}
