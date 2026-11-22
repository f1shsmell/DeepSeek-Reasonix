package serve

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"reasonix/internal/base/fileutil"
	"reasonix/internal/contract/config"
)

// persistedShareState is what a share remembers across a restart: whether the
// person had it open, and the addresses it was answering on. The addresses are
// kept because a paired phone's credential rides a cookie scoped to the origin
// it paired with, so reopening somewhere else makes every paired phone scan
// again — the one thing this file exists to prevent.
type persistedShareState struct {
	Open      bool     `json:"open"`
	Addresses []string `json:"addresses,omitempty"`
	// Address is the one-door spelling a state file carried before doors; a
	// file that still holds one reads as a one-door share.
	Address string `json:"address,omitempty"`
}

// shareStatePath rides the Reasonix state directory beside the device trust
// file, so REASONIX_HOME isolation holds and a test instance never reopens the
// real one's share.
func shareStatePath() string {
	home := strings.TrimSpace(config.ReasonixHomeDir())
	if home == "" {
		return ""
	}
	return filepath.Join(home, "state", "share-state.json")
}

// loadShareState reads what the previous process had open. A missing, empty or
// unreadable file reads as "closed", which is the shipping default.
func loadShareState(path string) persistedShareState {
	if path == "" {
		return persistedShareState{}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return persistedShareState{}
	}
	var saved persistedShareState
	if err := json.Unmarshal(raw, &saved); err != nil {
		return persistedShareState{}
	}
	if len(saved.Addresses) == 0 && saved.Address != "" {
		saved.Addresses = []string{saved.Address}
	}
	return saved
}

// saveShareState writes the state atomically. A failure is the caller's to log:
// the share already did what the person asked, and losing the note is better
// than refusing them over a disk.
func saveShareState(path string, st persistedShareState) error {
	if path == "" {
		return nil
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fileutil.AtomicWriteFile(path, raw, 0o600)
}
