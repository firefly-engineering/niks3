package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Stats counts what the daemon was given and what reached the server, so a
// supervisor (e.g. the GitHub Action's post step) can tell an idle run from
// one whose uploads were all rejected. Safe for concurrent use.
type Stats struct {
	mu        sync.Mutex
	received  int
	pushed    int
	failed    int
	lastError string
}

type statsFile struct {
	Received  int    `json:"received"`
	Pushed    int    `json:"pushed"`
	Failed    int    `json:"failed"`
	Remaining int    `json:"remaining"`
	LastError string `json:"last_error,omitempty"`
}

// AddReceived records n store paths handed over by the post-build-hook.
func (s *Stats) AddReceived(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.received += n
}

// WrapPush counts the paths of each successful push and each failed one,
// keeping the last error.
func (s *Stats) WrapPush(push PushFunc) PushFunc {
	return func(ctx context.Context, paths []string) ([]string, error) {
		uploaded, err := push(ctx, paths)

		s.mu.Lock()
		defer s.mu.Unlock()

		if err != nil {
			s.failed++
			s.lastError = err.Error()
		} else {
			s.pushed += len(paths)
		}

		return uploaded, err
	}
}

// WriteFile writes the counters as JSON, with remaining paths left in the
// queue, replacing path atomically.
func (s *Stats) WriteFile(path string, remaining int) error {
	s.mu.Lock()
	data, err := json.Marshal(statsFile{
		Received:  s.received,
		Pushed:    s.pushed,
		Failed:    s.failed,
		Remaining: remaining,
		LastError: s.lastError,
	})
	s.mu.Unlock()

	if err != nil {
		return fmt.Errorf("encoding stats: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".stats-*")
	if err != nil {
		return fmt.Errorf("creating stats file: %w", err)
	}

	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()

		return fmt.Errorf("writing stats file: %w", err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing stats file: %w", err)
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("renaming stats file: %w", err)
	}

	return nil
}
