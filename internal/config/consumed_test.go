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
//
//   - A field whose Go name collides with an unrelated identifier. `Enabled`,
//     `Endpoint`, `Path`, `Drop`, `Scope`, `Interval` and `Timeout` are used
//     everywhere; for those the check is vacuous, and six settings confirmed to
//     be unwired are invisible to it for exactly that reason:
//     `providers[].metrics.enabled`, `.endpoint` and `.interval` (nothing
//     scrapes a backend at all — the whole block is inert, not just the poll
//     interval), `providers[].params.drop`, `providers[].usage_probe.interval`
//     and `models[].deployments[].stream_timeout`. They are tracked in
//     docs/CONFIG.md §23.1 instead. The check does hold for the distinctive
//     names, which is where new settings land: `MaxQueueWait`,
//     `ClientPriority`, `PrefixTTL`, `AffinityGroup`.
//
//     The metrics block is the worked example of why this limit costs
//     something. Because the guard could not contradict it, docs/CONFIG.md
//     §23.1 said "the endpoint and the enabled flag are read; the poll interval
//     is not" for as long as it took someone to grep by hand — a claim wrong in
//     both halves, understating an entirely unwired feature as a one-field gap.
//     A prose ledger the guard cannot check has to be re-derived, not trusted,
//     and the entries here name which ones those are so that the re-derivation
//     has a work list.
//
//     `Config.Server.ReadTimeout`, `.IdleTimeout`, `.ReadHeaderTimeout`,
//     `Config.Auth.MissBudget.Rate` and `.Burst` are the second worked example,
//     and they are worse than the metrics block because the collision is with
//     the CONSUMER's own field name: `ReadTimeout` and `IdleTimeout` occur in
//     internal/server and `MissRate`/`MissBurst` in internal/auth, so the guard
//     reported all five as consumed on the day they were added and would have
//     gone on doing so for as long as they were unwired. A field is most likely
//     to collide with the identifier of the thing it is supposed to be feeding,
//     which makes this the failure mode of the check rather than an edge of it.
//     What holds those five is TestReadHeaderTimeoutIsReachableFromConfiguration
//     and its four siblings in internal/app, which drive [LoadBytes] through an
//     assembled gateway and assert a socket the running server does or does not
//     close and a status the running authenticator does or does not return.
//
//   - The OPPOSITE DIRECTION, entirely. A knob that exists on server.Options or
//     auth.Config and has no Config field at all is not walked here, because
//     this walk starts from Config. That direction is where most of this
//     repository's unreachable settings have come from, and four of them are
//     still open; docs/CONFIG.md §23.1b is its ledger, and it is prose for the
//     same reason §23.1 is — there is nothing here to hang it on.
//
//   - Semantics. Reading `MaxQueue` and comparing it against the wrong thing
//     passes here.
//
// So it is a floor, not a proof. The per-setting tests beside it are what assert
// the behaviour; this one only makes "nothing anywhere mentions it" impossible
// to ship, which is the state all twelve of the swept settings were in.
func TestEveryConfiguredFieldIsReadSomewhere(t *testing.T) {
	idents := identifiersOutsideConfig(t)

	// Canaries: unexported identifiers that exist nowhere but internal/config.
	// If the scan sees one, it is reading a copy of this package — a nested
	// checkout, a vendored tree, a stale build directory — and every answer it
	// gives is worthless in the direction that matters, because every field
	// looks consumed. Without this the guard fails open and still passes.
	for _, canary := range []string{"defaultLogLevel", "defaultCheckpoints", "defaultRedisURLEnv"} {
		if idents[canary] {
			t.Fatalf("the scan found %q, which exists only inside internal/config: "+
				"it is walking a copy of this package, so every field would look "+
				"consumed and this guard would pass while detecting nothing", canary)
		}
	}

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
	// Config.Compat.UsageChunkChoices and Config.Compat.AnthropicTotalTokens
	// were here, refused at their non-default value because the value had no
	// path from configuration into internal/wire. Both are wired now — they
	// travel on backend.Call, filled from dispatchState in internal/app — so the
	// entries are gone with the refusals they documented.
	//
	// The removal is deliberately not evidence of anything. This check is
	// SILENT for a readExempt entry that is read: the case body is empty, so
	// unlike knownUnwired there is no back-pressure telling you to delete a
	// stale one. UsageChunkChoices makes it worse — the scan matches
	// internal/wire/openai's own UsageChunkChoices TYPE by containment, so it
	// would report "read" whether or not this field were, and it did so for the
	// whole time it was refused. What proves the wiring is
	// TestUsageChunkChoicesIsSelectableFromConfiguration and
	// TestAnthropicTotalTokensIsSelectableFromConfiguration in internal/app,
	// which drive a loaded configuration to an emitted frame and assert the
	// shape changes with it.
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
			case ".git", "testdata", "node_modules", ".claude":
				return fs.SkipDir
			}
			// A nested checkout is a second copy of this repository, and every
			// field is "read" inside it — by internal/config itself. Walking one
			// does not weaken this guard, it disables it: the ledger direction
			// reports every entry as wired, and the direction that matters more
			// reports every new setting as consumed, silently. Found the hard
			// way, from an agent's worktree under .claude/, which turned the
			// whole check into a no-op that still passed.
			//
			// Keyed on go.mod rather than on the directory name, so any nested
			// module is excluded however it got there.
			if path != root {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return fs.SkipDir
				}
			}
			// This package is the one place a configured field is guaranteed to
			// be mentioned, so it cannot count as a consumer of itself. Matched
			// at any depth for the same reason as above.
			if filepath.Base(filepath.Dir(path)) == "internal" && d.Name() == "config" {
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
