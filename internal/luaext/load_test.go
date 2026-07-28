package luaext

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const allHooks = uint8(1<<numHooks) - 1

// TestLuaSourceIsALoadError is the promise that narrowing the scope did not
// leave a configuration surface that accepts Lua and quietly never runs it.
//
// This build has no Lua interpreter. A `.lua` file under extensions.lua.dir is
// therefore a startup failure with a message that says so, not a file the
// loader steps over while the operator believes their policy is enforced.
func TestLuaSourceIsALoadError(t *testing.T) {
	dir := t.TempDir()
	writePolicy(t, dir, "on_request.lua", "function on_request(r) return false end\n")

	_, err := LoadDir(dir, allHooks)
	if err == nil {
		t.Fatal("a .lua file must not load silently")
	}
	if !errors.Is(err, ErrLuaSource) {
		t.Fatalf("err = %v, want ErrLuaSource", err)
	}
	for _, want := range []string{"on_request.lua", PolicyExt} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name %q: %v", want, err)
		}
	}

	// And the same through New, which is the path the gateway actually takes.
	if _, err := New(Options{Enabled: true, Dir: dir}); !errors.Is(err, ErrLuaSource) {
		t.Fatalf("New must refuse to start: %v", err)
	}
}

func TestLoadDirNamesFilesAfterTheirHook(t *testing.T) {
	dir := t.TempDir()
	writePolicy(t, dir, "on_request.policy", "allow\n")
	writePolicy(t, dir, "on_request.10-team-acl.policy", `deny "no" if team_id == "x"`+"\n")
	writePolicy(t, dir, "on_route.policy", `deny "d" if provider == "p"`+"\n")
	writePolicy(t, dir, "README.md", "not a program\n")
	writePolicy(t, dir, ".hidden", "not a program\n")
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}

	progs, err := LoadDir(dir, allHooks)
	if err != nil {
		t.Fatal(err)
	}
	if len(progs) != 3 {
		t.Fatalf("loaded %d programs, want 3", len(progs))
	}
	// Lexical order, so an operator can number files to order them.
	if progs[0].Name != "on_request.10-team-acl.policy" || progs[0].Hook != HookRequest {
		t.Errorf("progs[0] = %s/%s", progs[0].Name, progs[0].Hook)
	}
	if progs[1].Name != "on_request.policy" {
		t.Errorf("progs[1] = %s", progs[1].Name)
	}
	if progs[2].Hook != HookRoute {
		t.Errorf("progs[2] hook = %s", progs[2].Hook)
	}
}

func TestLoadDirRejections(t *testing.T) {
	for _, tc := range []struct {
		name    string
		file    string
		src     string
		allowed uint8
		want    string
	}{
		{"unknown hook name", "on_teatime.policy", "allow\n", allHooks, "named after its hook point"},
		{"unrecognised extension", "on_request.txtx", "allow\n", allHooks, "unrecognised extension"},
		{"hook not listed", "on_request.policy", "allow\n", 1 << HookResponse, "does not list"},
		{"syntax error", "on_request.policy", "deny if\n", allHooks, "expected a value"},
	} {
		dir := t.TempDir()
		writePolicy(t, dir, tc.file, tc.src)
		_, err := LoadDir(dir, tc.allowed)
		if err == nil {
			t.Errorf("%s: expected an error", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v does not mention %q", tc.name, err, tc.want)
		}
	}
}

func TestLoadDirMissingDirectory(t *testing.T) {
	if _, err := LoadDir(filepath.Join(t.TempDir(), "nope"), allHooks); err == nil {
		t.Fatal("a missing extension directory must be a startup failure, not an empty engine")
	}
}

func TestLoadDirEmptyIsNotAnError(t *testing.T) {
	progs, err := LoadDir(t.TempDir(), allHooks)
	if err != nil || len(progs) != 0 {
		t.Fatalf("an empty directory loads nothing: %v %v", progs, err)
	}
	// An engine built from it is on but has no units, so every hook is free.
	e, err := New(Options{Enabled: true, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for h := Hook(0); h < numHooks; h++ {
		if e.Enabled(h) {
			t.Errorf("%s reported enabled with no units", h)
		}
	}
}

func TestHookSetFromConfiguredNames(t *testing.T) {
	m, err := hookSet(nil)
	if err != nil || m != allHooks {
		t.Fatalf("an empty list means all hooks: %v %v", m, err)
	}
	m, err = hookSet([]string{"on_email"})
	if err != nil || m != 1<<HookEmail {
		t.Fatalf("hookSet(on_email) = %v %v", m, err)
	}
	if _, err := hookSet([]string{"nope"}); err == nil {
		t.Fatal("an unknown hook name must be refused")
	}
}
