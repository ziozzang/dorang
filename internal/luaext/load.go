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
//   - A `.lua` file. This build cannot run Lua (see the package documentation),
//     and silently ignoring the file would leave an operator with a policy they
//     believe is enforced. This is the promise that the configuration surface
//     does not accept Lua that never runs.
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
			return nil, fmt.Errorf("%w: %s: rewrite it as a %s policy or register a Go hook; "+
				"see internal/luaext's package documentation for why this build has no Lua VM",
				ErrLuaSource, filepath.Join(dir, name), PolicyExt)
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
