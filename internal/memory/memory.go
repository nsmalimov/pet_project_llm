// Package memory is persistent knowledge that outlives a single task:
// user preferences, project rules and learned corrections. The current
// implementation is a flat JSONL file; the interface is what matters —
// pattern detection ("user said 'remove obvious comments' three times →
// propose a rule") plugs in behind Propose/Confirm later without touching
// callers.
package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Kind string

const (
	KindPreference  Kind = "preference"   // how the user likes things done
	KindProjectRule Kind = "project_rule" // constraints of a specific project
	KindCorrection  Kind = "correction"   // observed correction, candidate for a rule
)

type Item struct {
	ID        string    `json:"id"`
	Kind      Kind      `json:"kind"`
	Scope     string    `json:"scope,omitempty"` // "" = global, otherwise repo path/name
	Text      string    `json:"text"`
	Status    string    `json:"status"` // proposed | confirmed
	CreatedAt time.Time `json:"created_at"`
}

// Store is the persistence interface for memory items.
type Store interface {
	Add(item Item) error
	List(kind Kind) ([]Item, error) // kind "" = all
	// Relevant returns confirmed items that should be injected into prompts
	// for the given repo scopes.
	Relevant(scopes []string) ([]string, error)
}

// FileStore is a JSONL-backed memory store.
type FileStore struct {
	path string
	mu   sync.Mutex
}

func NewFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &FileStore{path: filepath.Join(dir, "memory.jsonl")}, nil
}

func (s *FileStore) Add(item Item) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if item.Status == "" {
		item.Status = "confirmed"
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	}
	b, err := json.Marshal(item)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func (s *FileStore) List(kind Kind) ([]Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Item
	dec := json.NewDecoder(f)
	for {
		var it Item
		if err := dec.Decode(&it); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return out, fmt.Errorf("decode %s: %w", s.path, err)
		}
		if kind == "" || it.Kind == kind {
			out = append(out, it)
		}
	}
	return out, nil
}

func (s *FileStore) Relevant(scopes []string) ([]string, error) {
	items, err := s.List("")
	if err != nil {
		return nil, err
	}
	inScope := func(scope string) bool {
		if scope == "" {
			return true
		}
		for _, s := range scopes {
			if s == scope {
				return true
			}
		}
		return false
	}
	var out []string
	for _, it := range items {
		if it.Status == "confirmed" && it.Kind != KindCorrection && inScope(it.Scope) {
			out = append(out, it.Text)
		}
	}
	return out, nil
}
