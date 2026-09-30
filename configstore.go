package main

import "sync"

// ConfigStore keeps the live configuration and rewrites printmon.json whenever
// the dashboard changes something (new printer, cabinet, subnet).
type ConfigStore struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

// NewConfigStore wraps an already loaded config.
func NewConfigStore(path string, cfg Config) *ConfigStore {
	return &ConfigStore{path: path, cfg: cfg}
}

// Get returns a snapshot safe to use from any goroutine.
func (s *ConfigStore) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Path returns the config file location.
func (s *ConfigStore) Path() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.path
}

// Update mutates the config and persists it. When saving fails the in-memory
// change is kept, so the user is told about the write error instead of losing
// the edit silently.
func (s *ConfigStore) Update(fn func(*Config)) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.cfg)
	err := SaveConfig(s.path, s.cfg)
	return s.cfg, err
}
