package luaext

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// PolicyExt is the extension of a policy file.
const PolicyExt = ".policy"

// docExts are files the loader is willing to find in the extension directory
// and ignore. Everything else is either a program or a mistake.
var docExts = map[string]bool{".md": true, ".txt": true, ".yaml": true, ".yml": true}

// LoadDir compiles every policy file in dir.
//
// # On loading from a directory
//
// DESIGN §11.5 says loading should be explicit configuration rather than a scan
// of a writable directory, because a plugin mechanism that picks up whatever
// appears in a path is a code-execution primitive. This loader scans, because
// extensions.lua.dir is the only surface the configuration schema offers — and
// the objection is answered a different way: a `.policy` file is not code. The
// language has no I/O, no function calls, no loops and no way to build a
// string, so the worst an attacker who can write to this directory achieves is
// to refuse traffic, which they could also achieve by writing to the
// configuration file itself. If the schema grows an explicit file list, this
// should follow it.
//
// The hook a file belongs to comes from its name: the part before the first dot
// must be a hook point, so `on_request.policy` and `on_request.team-acl.policy`
// both register at on_request and load in lexical order.
//
// Three things are load errors rather than skips, because each of them is a
// configuration that looks like it works and does not:
//
//   - A `.lua` file. Lua *is* executable in this build — see [Plugin] — but only
//     because an operator named the file in the configuration. A plugin that ran
//     because it was found in a writable directory is precisely the
//     code-execution primitive §11.5 refuses, so a `.lua` here is an error that
//     tells the operator where to declare it, not a file that is silently
//     ignored and not one that is silently run.
//   - A file whose name does not start with a hook point, or names a hook that
//     extensions.lua.hooks excludes.
//   - Any other unrecognised extension.
func LoadDir(dir string, allowed uint8) ([]*Program, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("luaext: extensions.lua.dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, ent := range entries {
		if ent.IsDir() || strings.HasPrefix(ent.Name(), ".") {
			continue
		}
		names = append(names, ent.Name())
	}
	sort.Strings(names)

	var out []*Program
	for _, name := range names {
		ext := strings.ToLower(filepath.Ext(name))
		switch {
		case ext == ".lua":
			return nil, fmt.Errorf("%w: %s: declare it under filters.plugins with a name and a "+
				"path, which is how a plugin is loaded (§11.5); a directory that runs whatever "+
				"appears in it is a code-execution primitive",
				ErrLuaSource, filepath.Join(dir, name))
		case docExts[ext]:
			continue
		case ext != PolicyExt:
			return nil, fmt.Errorf("luaext: %s: unrecognised extension %q in the extension directory; "+
				"policy files end in %s", filepath.Join(dir, name), ext, PolicyExt)
		}

		hookName := name
		if i := strings.IndexByte(hookName, '.'); i >= 0 {
			hookName = hookName[:i]
		}
		h, ok := ParseHook(hookName)
		if !ok {
			return nil, fmt.Errorf("luaext: %s: a policy file is named after its hook point; "+
				"%q is not one of %v", filepath.Join(dir, name), hookName, hookNames)
		}
		if allowed&(1<<uint(h)) == 0 {
			return nil, fmt.Errorf("luaext: %s: registers at %s, which extensions.lua.hooks does not list",
				filepath.Join(dir, name), h)
		}

		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("luaext: %s: %w", filepath.Join(dir, name), err)
		}
		p, err := Compile(name, h, src)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}
