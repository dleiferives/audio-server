// Package store provides disk-backed audio file storage with optional TTL-based cleanup.
package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Store struct {
	Dir string
	TTL time.Duration
	mu  sync.RWMutex
}

func New(dir string, ttl time.Duration) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := os.MkdirAll(abs, 0755); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	s := &Store{Dir: abs, TTL: ttl}
	if ttl > 0 {
		go s.sweepLoop()
	}
	return s, nil
}

func (s *Store) Write(jobID string, audio []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.path(jobID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, audio, 0644); err != nil {
		return fmt.Errorf("store write: %w", err)
	}
	return os.Rename(tmp, path)
}

func (s *Store) Read(jobID string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, err := os.ReadFile(s.path(jobID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("store: job %s audio not found", jobID)
		}
		return nil, fmt.Errorf("store read: %w", err)
	}
	return data, nil
}

func (s *Store) Delete(jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path(jobID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("store delete: %w", err)
	}
	return nil
}

func (s *Store) path(jobID string) string {
	return filepath.Join(s.Dir, jobID+".pcm")
}

func (s *Store) sweepLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.sweep()
	}
}

func (s *Store) sweep() {
	if s.TTL <= 0 {
		return
	}
	cutoff := time.Now().Add(-s.TTL)
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pcm") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(s.Dir, e.Name()))
		}
	}
}
