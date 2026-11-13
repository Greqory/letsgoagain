// Package opencode talks to the shared OpenCode v2 service: where it runs (service.json), its HTTP API
// (sessions, messages, prompts), its event stream, and recognising meridian's limit error in a failure.
package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Service is the shared OpenCode service as described by its service.json.
type Service struct {
	URL      string `json:"url"`
	PID      int    `json:"pid"`
	Version  string `json:"version"`
	Password string `json:"password"`
}

// DefaultServicePath is $XDG_STATE_HOME/opencode/service.json, falling back to ~/.local/state.
func DefaultServicePath() (string, error) {
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "opencode", "service.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("service path: %w", err)
	}
	return filepath.Join(home, ".local", "state", "opencode", "service.json"), nil
}

// ReadService reads service.json. The password is never part of an error message.
func ReadService(path string) (Service, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Service{}, fmt.Errorf("read service file: %w", err)
	}
	var s Service
	if err := json.Unmarshal(raw, &s); err != nil {
		return Service{}, fmt.Errorf("parse service file %s: %w", path, err)
	}
	if s.URL == "" || s.Password == "" {
		return Service{}, errors.New("service file has no url or password")
	}
	return s, nil
}
