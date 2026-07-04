package twitch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Store persists a Token as a JSON file. It holds secrets, so it's written 0600.
type Store struct {
	path string
}

// NewStore builds a store backed by the file at path.
func NewStore(path string) *Store { return &Store{path: path} }

// Load reads the token. A missing file returns (nil, nil) so "not yet authorized"
// is a normal, non-error state.
func (s *Store) Load() (*Token, error) {
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

// Save writes the token atomically (temp file + rename) with 0600 perms.
func (s *Store) Save(t *Token) error {
	raw, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
