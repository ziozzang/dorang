package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/parseguard"
)

func TestConfigEditorsRejectDeepOrCyclicYAML(t *testing.T) {
	for _, b := range []string{strings.Repeat("[", 1000) + "0" + strings.Repeat("]", 1000), "a: &a [*a]"} {
		if _, err := LoadBytes([]byte(b)); err == nil {
			t.Fatal("hostile YAML loaded")
		}
		if _, err := SetDeploymentEnabled([]byte(b), "g", "p", "m", 0, true); err == nil {
			t.Fatal("hostile YAML reached editor")
		}
	}
}

func TestLockedFilesAreBoundedAndOversizedEditsLeaveFileIntact(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte("server: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := EditLocked(p, func([]byte) ([]byte, error) { return []byte(strings.Repeat("x", int(parseguard.MaxFileBytes)+1)), nil }); !errors.Is(err, parseguard.ErrSize) {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "server: {}\n" {
		t.Fatal("rejected edit changed file")
	}
	f, err := os.OpenFile(p, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := readLocked(p); !errors.Is(err, parseguard.ErrSize) {
		t.Fatal(err)
	}
	called := false
	if err := EditLocked(p, func(b []byte) ([]byte, error) { called = true; return b, nil }); !errors.Is(err, parseguard.ErrSize) || called {
		t.Fatalf("%v callback=%v", err, called)
	}
}
