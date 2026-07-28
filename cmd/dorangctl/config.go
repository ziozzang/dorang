package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/ziozzang/dorang/internal/app"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/pkg/catalog"
	"gopkg.in/yaml.v3"
)

// runConfig implements `config lint` and `config check`.
func (e env) runConfig(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.stderr, "usage: dorangctl config lint|check [file...]")
		return 2
	}
	switch args[0] {
	case "lint", "check":
		return e.configLint(args[1:])
	}
	return e.fail("unknown config subcommand %q", args[0])
}

// configLint validates configuration files and the model catalogs they name.
//
// Two different checkers run, because two different files are wrong in two
// different ways: internal/config validates the gateway configuration and
// pkg/catalog lints the model catalog overlays. The catalog layer that
// DORANG_CATALOG_PATH names is included, so this lints what a server would
// actually load rather than only what was typed on the command line.
func (e env) configLint(args []string) int {
	fs := newFlagSet("config lint", e)
	catalogPaths := fs.String("catalog", "", "extra model catalog files or directories, comma separated")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	paths := fs.Args()
	if len(paths) == 0 {
		def := os.Getenv(EnvConfigPath)
		if def == "" {
			def = defaultConfigPath
		}
		paths = []string{def}
	}

	failed := false
	for _, path := range paths {
		// The same policy the server applies to --check: a secret that is not on
		// this machine is a warning, a wrong reference is not.
		cfg, warnings, err := app.CheckConfig(path)
		for _, w := range warnings {
			fmt.Fprintf(e.stderr, "%s: warning: %s\n", path, w)
		}
		if err != nil {
			e.reportProblems(path, err)
			failed = true
			continue
		}
		if cfg == nil {
			fmt.Fprintf(e.stdout, "%s: ok (secrets not readable here, see warnings)\n", path)
			continue
		}
		fmt.Fprintf(e.stdout, "%s: ok — %d provider(s), %d credential(s), %d model group(s)\n",
			path, len(cfg.Providers), len(cfg.Credentials), len(cfg.Models))
	}

	catalogs := splitComma(*catalogPaths)
	catalogs = append(catalogs, catalog.EnvPaths()...)
	if len(catalogs) > 0 {
		problems := catalog.LintFiles(catalogs...)
		for _, p := range problems {
			fmt.Fprintf(e.stderr, "  %s\n", p.String())
		}
		if catalog.HasErrors(problems) {
			failed = true
		} else {
			fmt.Fprintf(e.stdout, "model catalog: ok — %d file layer(s), %d note(s)\n",
				len(catalogs), len(problems))
		}
	}
	if failed {
		return 1
	}
	return 0
}

// runImport implements `import config`.
func (e env) runImport(args []string) int {
	if len(args) < 2 || args[0] != "config" {
		fmt.Fprintln(e.stderr, "usage: dorangctl import config <file>")
		return 2
	}
	path := args[1]
	data, err := os.ReadFile(path)
	if err != nil {
		if isNotExist(err) {
			return e.fail("no such file: %s", path)
		}
		return e.fail("%v", err)
	}
	cfg, warnings, err := config.ImportProxyConfig(data)
	if err != nil {
		return e.fail("%s: %v", path, err)
	}
	// DESIGN §4.1: a secret is never logged, never in an error message, never in
	// a snapshot; only the credential's opaque id and its health leave the
	// subsystem. Every other tool in this repository obeys that by not holding a
	// secret in the first place. This one holds several — it is the one command
	// whose job is to READ a foreign configuration and WRITE dorang's — so it is
	// the one place the rule has to be enforced rather than inherited.
	//
	// What went out before was the literal: `key: sk-…` on stdout, into the new
	// configuration file, into terminal scrollback, and into the CI log of any
	// scripted migration. What goes out now is the credential's id, its provider,
	// the environment variable to put the value in, and a fingerprint — which is
	// everything an operator needs to finish the migration and none of the secret.
	//
	// The operator has not lost anything: the value is still in the file they are
	// migrating FROM. The generated configuration will not start a server until
	// they set the named variables, and that is intended.
	creds, secrets := redactImportedCredentials(cfg)

	// Warnings go to stderr so the converted configuration on stdout can be
	// redirected into a file without them landing in it. Every one of them is
	// something the import could not resolve rather than something it guessed:
	// DESIGN §2.4 records that guessing is what makes an import untrustworthy.
	//
	// They are scrubbed on the way out. A warning about a bad value is the usual
	// way a redaction is undone — the message helpfully quotes what it could not
	// accept — and this loop is the only place every one of them passes through.
	for _, w := range warnings {
		fmt.Fprintf(e.stderr, "warning: %s\n", scrubSecrets(w.String(), secrets))
	}
	for _, c := range creds {
		fmt.Fprintf(e.stderr,
			"credential %s (provider %s): the source file held a literal secret (%s). "+
				"It was NOT copied into the output; set %s to that value before starting dorang\n",
			c.id, c.provider, c.fingerprint, c.env)
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return e.fail("%v", err)
	}
	// The last thing between a secret and stdout. Redaction above is the fix;
	// this is the assertion that the fix held, made where the bytes are rather
	// than only in a test. It fires when a literal reaches the encoder by some
	// route this function did not walk — a second SecretRef field, an embedding
	// whose MarshalYAML is bypassed by `yaml:",inline"`, a future import that
	// copies the value somewhere else as well. Two of those have happened here
	// already.
	clean, leaked := scrubBytes(out, secrets)
	if leaked {
		fmt.Fprintln(e.stderr, "dorangctl: a literal secret survived redaction and was "+
			"replaced with <redacted> in the output. The configuration below is incomplete "+
			"and this is a bug in dorangctl; please report it")
	}
	fmt.Fprintf(e.stderr, "# imported %s: %d provider(s), %d credential(s), %d model group(s), %d warning(s)\n",
		path, len(cfg.Providers), len(cfg.Credentials), len(cfg.Models), len(warnings))
	_, _ = e.stdout.Write(clean)
	if leaked {
		return 1
	}
	return 0
}

// importedCredential is what an operator is told about one imported secret:
// which credential, which provider, where to put the value, and enough of a
// fingerprint to tell two of them apart. Not the value.
type importedCredential struct {
	id          string
	provider    string
	env         string
	fingerprint string
}

// redactImportedCredentials moves every literal secret in an imported
// configuration to an environment reference, before anything can write it.
//
// The generated variable name follows the same shape internal/config uses,
// DORANG_<PROVIDER>_API_KEY, and gains a numeric suffix when one provider
// carries several credentials — two providers' keys landing in one variable
// would be a silent misconfiguration rather than a redaction.
//
// It returns the report lines and the literals it removed. The literals are kept
// only so the output can be checked against them a few lines later; nothing else
// reads them.
func redactImportedCredentials(cfg *config.Config) ([]importedCredential, []string) {
	if cfg == nil {
		return nil, nil
	}
	used := map[string]bool{}
	for i := range cfg.Credentials {
		if v := cfg.Credentials[i].Key.Env; v != "" {
			used[v] = true
		}
	}
	var (
		found   []importedCredential
		secrets []string
	)
	for i := range cfg.Credentials {
		c := &cfg.Credentials[i]
		literal := c.Key.Inline
		if literal == "" {
			continue
		}
		name := importEnvName(c.Provider, used)
		// The whole reference is replaced rather than the field cleared: a
		// SecretRef carries four alternative sources and an unresolved value,
		// and leaving any of them behind next to a fresh key_env would be a
		// configuration with two secret sources, which validation refuses.
		c.Key = config.SecretRef{Env: name}
		found = append(found, importedCredential{
			id: c.ID, provider: c.Provider, env: name, fingerprint: fingerprint(literal),
		})
		secrets = append(secrets, literal)
	}
	return found, secrets
}

// importEnvName picks an unused environment variable name for a provider.
func importEnvName(provider string, used map[string]bool) string {
	base := "DORANG_" + envIdent(provider) + "_API_KEY"
	name := base
	for n := 2; used[name]; n++ {
		name = base + "_" + strconv.Itoa(n)
	}
	used[name] = true
	return name
}

// envIdent turns a provider name into the shape an environment variable can
// have. A provider name is free-form — "azure-eu/west" is legal — and a variable
// name is not.
func envIdent(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
			b = append(b, c-('a'-'A'))
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	if len(b) == 0 {
		return "IMPORTED"
	}
	return string(b)
}

// fingerprint identifies a secret without disclosing it: a prefix of its
// SHA-256, which is stable across runs and not reversible.
//
// Four bytes is deliberate. It is enough for an operator to tell two credentials
// apart and to confirm they exported the right value, and short enough that it
// is not a useful oracle for confirming a guess at a low-entropy one.
func fingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return "sha256:" + hex.EncodeToString(sum[:4])
}

// minScrubbable is the shortest string scrubSecrets will act on. Something
// shorter than this is not a credential, and treating it as one would replace
// every incidental occurrence of it in the output.
const minScrubbable = 6

// scrubSecrets removes any of secrets from a string bound for a stream.
func scrubSecrets(s string, secrets []string) string {
	for _, sec := range secrets {
		if len(sec) < minScrubbable {
			continue
		}
		s = strings.ReplaceAll(s, sec, "<redacted>")
	}
	return s
}

// scrubBytes is scrubSecrets over bytes, and reports whether it had to act.
func scrubBytes(b []byte, secrets []string) ([]byte, bool) {
	acted := false
	for _, sec := range secrets {
		if len(sec) < minScrubbable || !bytes.Contains(b, []byte(sec)) {
			continue
		}
		acted = true
		b = bytes.ReplaceAll(b, []byte(sec), []byte("<redacted>"))
	}
	return b, acted
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if p := s[start:i]; p != "" {
				out = append(out, p)
			}
			start = i + 1
		}
	}
	return out
}
