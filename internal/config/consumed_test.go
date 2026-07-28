package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestEveryConfiguredFieldIsReadSomewhere is the guard against this repository's
// dominant defect class: a setting that loads, validates, and is read by nothing
// (DESIGN §17.1).
//
// It walks the whole Config type, collects every field that carries a `yaml`
// tag, and requires each one's Go name to appear as an identifier in
// non-test Go source OUTSIDE internal/config. A field that only this package
// mentions is a field the gateway does not act on.
//
// WHAT IT CANNOT CATCH, stated plainly because a guard whose limits are unknown
// is a guard nobody can rely on:
//
//   - A field that is read and then dropped. `Deployment.Timeout` was copied
//     into a struct nobody forwarded for most of this project's life, and every
//     name in that chain was "referenced". Reference is necessary, not
//     sufficient.
//   - A field whose Go name collides with an unrelated identifier. `Enabled`,
//     `Path`, `Drop`, `Interval` and `Timeout` are used everywhere; for those the
//     check is vacuous, and four settings this sweep confirmed to be unwired —
//     `providers[].metrics.interval`, `providers[].params.drop`,
//     `providers[].usage_probe.interval` and
//     `models[].deployments[].stream_timeout` — are invisible to it for exactly
//     that reason. They are tracked in docs/CONFIG.md §23.1 instead. The check
//     does hold for the distinctive names, which is where new settings land:
//     `MaxQueueWait`, `ClientPriority`, `PrefixTTL`, `AffinityGroup`.
//   - Semantics. Reading `MaxQueue` and comparing it against the wrong thing
//     passes here.
//
// So it is a floor, not a proof. The per-setting tests beside it are what assert
// the behaviour; this one only makes "nothing anywhere mentions it" impossible
// to ship, which is the state all twelve of the swept settings were in.
func TestEveryConfiguredFieldIsReadSomewhere(t *testing.T) {
	idents := identifiersOutsideConfig(t)

	var walk func(rt reflect.Type, path string, seen map[reflect.Type]bool)
	var missing, fixed []string
	walk = func(rt reflect.Type, path string, seen map[reflect.Type]bool) {
		for rt.Kind() == reflect.Ptr || rt.Kind() == reflect.Slice || rt.Kind() == reflect.Map {
			rt = rt.Elem()
		}
		if rt.Kind() != reflect.Struct || seen[rt] {
			return
		}
		seen[rt] = true
		defer delete(seen, rt)

		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if !f.IsExported() {
				continue
			}
			tag, ok := f.Tag.Lookup("yaml")
			if !ok {
				continue
			}
			name := path + "." + f.Name
			read := idents.reads(f.Name)
			switch {
			case readExempt[name]:
			case knownUnwired[name]:
				if read {
					// The ledger shrinks and never silently: a setting that has
					// been wired must leave the list, or the list stops meaning
					// anything.
					fixed = append(fixed, name)
				}
			case !read:
				missing = append(missing, name+"  (yaml:"+tag+")")
			}
			walk(f.Type, name, seen)
		}
	}
	walk(reflect.TypeOf(Config{}), "Config", map[reflect.Type]bool{})

	if len(missing) > 0 {
		t.Errorf("%d configured field(s) are read by nothing outside internal/config.\n"+
			"Each one loads, validates, and does nothing — the defect class DESIGN §17.1 "+
			"names as this codebase's dominant one.\nWire it, refuse it in Validate, or add "+
			"it to knownUnwired with the reason and the section it comes from:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
	if len(fixed) > 0 {
		t.Errorf("%d field(s) listed as unwired are now read somewhere. Remove them from "+
			"knownUnwired — a backlog that does not shrink is a backlog nobody reads:\n  %s",
			len(fixed), strings.Join(fixed, "\n  "))
	}
}

// readExempt lists configured fields deliberately not read outside this package,
// each with the reason. An entry here is a claim that has to be defensible; it is
// not a way to silence the check.
var readExempt = map[string]bool{
	// Refused at validation rather than acted on. The field exists so the
	// refusal can name the key and say what to use instead — deleting it would
	// turn `rpm:` into an unknown-field error that reads like a typo, which is
	// the wrong diagnosis for someone migrating a working configuration.
	"Config.Capacity.ProviderGroups.RPM":   true,
	"Config.Capacity.ProviderGroups.TPM":   true,
	"Config.Capacity.CredentialGroups.RPM": true,
	"Config.Capacity.CredentialGroups.TPM": true,
	"Config.Capacity.Models.Limits.RPM":    true,
	"Config.Capacity.Models.Limits.TPM":    true,
	"Config.Capacity.Global.RPM":           true,
	"Config.Capacity.Global.TPM":           true,
	"Config.Capacity.Principals.RPM":       true,
	"Config.Capacity.Principals.TPM":       true,
	// §13. The one mode that would have read it — `capacity_mode:
	// shared-redis` — is refused at load, because this build ships the
	// protocol and no client that speaks it. The key stays loadable so that a
	// configuration imported from LiteLLM, which carries redis settings,
	// parses rather than failing on an unknown field.
	"Config.Cluster.RedisURLEnv": true,
	// Same, for the vault reference this build ships no resolver for.
	"Config.Credentials.Key.Ref":                   true,
	"Config.KeyRotation.Providers.Keys.Key.Ref":    true,
	"Config.Notifications.Email.SMTP.Password.Ref": true,
	"Config.Notifications.Email.HTTP.Secret.Ref":   true,
	"Config.Filters.Secret.Ref":                    true,

	// Consumed inside this package, which is the whole of their effect. A
	// secret source is resolved here and leaves as a value; server.env gates
	// whether an inline literal is accepted at all, and nothing downstream has
	// any business knowing which of the four spellings a secret came from.
	"Config.Server.Env":                               true,
	"Config.Credentials.Key.Env":                      true,
	"Config.Credentials.Key.Inline":                   true,
	"Config.KeyRotation.Providers.Keys.Key.Env":       true,
	"Config.KeyRotation.Providers.Keys.Key.Inline":    true,
	"Config.Notifications.Email.SMTP.Password.Env":    true,
	"Config.Notifications.Email.SMTP.Password.Inline": true,
	"Config.Notifications.Email.HTTP.Secret.Env":      true,
	"Config.Notifications.Email.HTTP.Secret.Inline":   true,
	"Config.Filters.Secret.Env":                       true,
	"Config.Filters.Secret.Inline":                    true,
	// A version this build does not understand is refused here; a version it
	// does understand has nothing left to say.
	"Config.Version": true,
	// Refused here as well: numeric metering cannot be turned off, so the flag
	// exists to carry the refusal rather than to be consulted downstream.
	"Config.Metering.Numeric": true,
}

// knownUnwired is the ledger of settings that still load and do nothing. It is
// the list docs/CONFIG.md §23.1 keeps in prose, kept here as well so that it
// cannot drift from the code: adding a setting without a consumer fails this
// test, and wiring one without removing its entry fails it too.
//
// Every entry names the design section it comes from, so that "what is left" is
// answerable without re-deriving it. None of these was in the scope of the sweep
// that added this guard; they are what the guard found on its first run, which is
// the strongest argument for having it.
var knownUnwired = map[string]bool{
	// §6.2 provider usage probes. internal/probe implements the fetchers and
	// nothing constructs one from configuration.
	"Config.Providers.UsageProbe":         true,
	"Config.Providers.UsageProbe.Fetcher": true,
	// §10.3 parameter conversion. The knob that says whether unsupported
	// parameters are dropped never reaches the conversion path; only the kind's
	// own capability set decides.
	"Config.Providers.Params.DropUnsupported": true,
	// §7.4b. The prefix chain is cut logarithmically; `checkpoints: fixed`
	// validates and selects nothing.
	"Config.Routing.Prefix.Checkpoints": true,
	// §5.3 key affinity. Not validated either — the only occurrence in the tree
	// is the struct tag.
	"Config.KeyRotation.Providers.AffinityGroup": true,
	// §12. No OTLP exporter is wired, and no logger reads the level or the
	// format — diagnostics go through the Logf hook the caller supplies.
	"Config.Observability.OTLPEndpoint": true,
	"Config.Observability.LogLevel":     true,
	"Config.Observability.LogFormat":    true,
}

// identifiersOutsideConfig collects every identifier in non-test Go source
// outside internal/config.
// identSet answers whether a field name is mentioned outside internal/config.
type identSet map[string]bool

// reads reports whether name is used, directly or through an accessor.
//
// The accessor case is not a loophole, it is the normal shape here: several
// settings are read through a method that applies the default —
// PrometheusEnabled, GrantsClientPriority, IsEnabled — and a caller of one of
// those is reading the field as surely as a caller that touches it directly. The
// test is containment rather than equality, which is why the check is a floor:
// a short field name is easy to contain by accident.
func (s identSet) reads(name string) bool {
	if s[name] {
		return true
	}
	for id := range s {
		if len(id) > len(name) && (strings.HasPrefix(id, name) || strings.HasSuffix(id, name)) {
			return true
		}
	}
	return false
}

func identifiersOutsideConfig(t *testing.T) identSet {
	t.Helper()
	root := moduleRoot(t)
	out := identSet{}
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata", "node_modules":
				return fs.SkipDir
			}
			// This package is the one place a configured field is guaranteed to
			// be mentioned, so it cannot count as a consumer of itself.
			if path == filepath.Join(root, "internal", "config") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			// A package mid-edit by another change must not fail this guard: it
			// would report every field as unread and bury the real signal.
			t.Logf("skipping unparseable %s: %v", path, perr)
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				out[id.Name] = true
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if len(out) < 100 {
		t.Fatalf("only %d identifiers found under %s: the walk is not reaching the source, "+
			"so this guard would pass vacuously", len(out), root)
	}
	return out
}

// moduleRoot finds the directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("no go.mod above the working directory")
	return ""
}
