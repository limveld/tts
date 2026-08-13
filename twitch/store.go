package twitch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Store persists a Token as a JSON file. It holds secrets, so it's written 0600.
//
// Safe for concurrent use: saves are serialized and land through a uniquely named
// temp file, so two savers can't rename each other's file out from under one
// another. A shared temp name used to fail the loser with ENOENT — and could
// publish the wrong bytes — whenever the bot's ticker goroutines refreshed at the
// same instant, which is what happens every time the machine wakes from sleep
// holding an expired token.
type Store struct {
	mu   sync.Mutex
	path string
}

// NewStore builds a store backed by the file at path.
func NewStore(path string) *Store { return &Store{path: path} }

// Load reads the token. A missing file returns (nil, nil) so "not yet authorized"
// is a normal, non-error state.
func (s *Store) Load() (*Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t Token
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("parsing token store %s: %w", s.path, err)
	}
	return &t, nil
}

// Save writes the token atomically: a uniquely named temp file in the same
// directory, fsynced, then renamed over the target.
func (s *Store) Save(t *Token) error {
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	dir := filepath.Dir(s.path) // "." for a bare filename like bot.tokens.json
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	// Same directory as the target so the rename stays within one filesystem (and
	// is therefore atomic); a unique name so no second saver can write or rename
	// the same path. CreateTemp's pattern must not contain a separator, hence Base.
	f, err := os.CreateTemp(dir, filepath.Base(s.path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// Every failure path below must clean up: the temp file holds live secrets.
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()

	if err := f.Chmod(0o600); err != nil { // CreateTemp already does this; be explicit about the promise
		f.Close()
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	// fsync before the rename: a crash or power loss must not leave the store
	// pointing at a file whose contents never reached disk. An empty token store
	// costs a manual `mise run bot:auth`.
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	tmp = "" // renamed away; nothing left to clean up
	fsyncDir(dir)
	return nil
}

// fsyncDir makes the rename itself durable. Best-effort and silent: some
// filesystems refuse to sync a directory, and a token we merely can't prove is
// durable is still better than an error the caller would report as a refresh
// failure.
func fsyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}
