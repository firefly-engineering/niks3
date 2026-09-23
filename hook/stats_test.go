package hook_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Mic92/niks3/hook"
)

func TestStatsCountsPushesAndKeepsLastError(t *testing.T) {
	t.Parallel()

	var stats hook.Stats

	fail := true
	push := stats.WrapPush(func(_ context.Context, paths []string) ([]string, error) {
		if fail {
			return nil, errors.New("server returned 401: Unauthorized")
		}

		return paths, nil
	})

	stats.AddReceived(3)

	if _, err := push(context.Background(), []string{"/nix/store/a", "/nix/store/b"}); err == nil {
		t.Fatal("expected the wrapped error")
	}

	fail = false

	if _, err := push(context.Background(), []string{"/nix/store/c"}); err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(t.TempDir(), "stats.json")
	if err := stats.WriteFile(file, 2); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}

	want := map[string]any{
		"received":   3.0,
		"pushed":     1.0,
		"failed":     1.0,
		"remaining":  2.0,
		"last_error": "server returned 401: Unauthorized",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}
