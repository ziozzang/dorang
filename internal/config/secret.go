package config

import (
	"os"
	"strings"
)

// EnvDevelopment is the only server.env under which an inline literal secret is
// accepted (design §4.1).
const EnvDevelopment = "development"

// EnvProduction is the default server.env.
const EnvProduction = "production"

// SecretRef is a reference to a secret. It is embedded inline wherever the
// design writes key_env / key_file / key_ref / key as sibling keys, so it
// decodes and encodes exactly the shape of §4.2.
//
// Exactly one source may be set. The resolved value is held in an unexported
// field and is never exposed by String, YAML marshaling, or any error message
// produced by this package; read it with [SecretRef.Value].
type SecretRef struct {
	// Env names an environment variable holding the secret.
	Env string `yaml:"key_env,omitempty"`
	// File names a file holding the secret; a trailing newline is trimmed.
	File string `yaml:"key_file,omitempty"`
	// Ref is an external reference such as "vault:secret/data/x#key".
	//
	// It is REFUSED at validation. The field is kept so that the refusal can
	// name the key and say what to use instead; deleting it would make a
	// key_ref: line an unknown-field error that reads like a typo, which is the
	// wrong diagnosis for a deployment migrating off a vault-integrated proxy.
	Ref string `yaml:"key_ref,omitempty"`
	// Inline is a literal secret, accepted only when server.env is
	// "development". Resolution moves the literal out of this field so it
	// cannot be re-marshaled back into a file or a log line.
	Inline string `yaml:"key,omitempty"`

	value    string
	resolved bool
	fromFile bool
}

// IsZero reports whether no source is configured.
func (s SecretRef) IsZero() bool {
	return s.Env == "" && s.File == "" && s.Ref == "" && s.Inline == "" && !s.resolved
}

// sources counts the configured sources.
func (s SecretRef) sources() int {
	n := 0
	for _, v := range []string{s.Env, s.File, s.Ref, s.Inline} {
		if v != "" {
			n++
		}
	}
	if n == 0 && s.resolved {
		return 1 // an inline literal that has already been moved out
	}
	return n
}

// Source returns a redacted description of where the secret comes from,
// suitable for logs and error messages.
func (s SecretRef) Source() string {
	switch {
	case s.Env != "":
		return "key_env:" + s.Env
	case s.File != "":
		return "key_file:" + s.File
	case s.Ref != "":
		return "key_ref:" + s.Ref
	case s.Inline != "" || (s.resolved && !s.fromFile):
		return "key:(inline literal)"
	default:
		return "(unset)"
	}
}

// String returns the redacted source. It exists so that a SecretRef formatted
// with %v or %s can never print a secret.
func (s SecretRef) String() string { return s.Source() }

// GoString returns the redacted source, so %#v cannot print a secret either.
func (s SecretRef) GoString() string { return "config.SecretRef{" + s.Source() + "}" }

// MarshalYAML writes back only the reference, never a resolved value.
//
// Note that a SecretRef embedded with `yaml:",inline"` is flattened by the
// encoder rather than passed through this method, so the real guarantee is
// that [SecretRef.resolve] moves an inline literal out of the exported field.
func (s SecretRef) MarshalYAML() (any, error) {
	m := map[string]string{}
	switch {
	case s.Env != "":
		m["key_env"] = s.Env
	case s.File != "":
		m["key_file"] = s.File
	case s.Ref != "":
		m["key_ref"] = s.Ref
	}
	return m, nil
}

// MarshalJSON writes back only the redacted source.
func (s SecretRef) MarshalJSON() ([]byte, error) {
	return []byte(`"` + strings.ReplaceAll(s.Source(), `"`, `\"`) + `"`), nil
}

// Value returns the resolved secret and whether it is available. A key_ref is
// never resolved by this package, so Value reports false for one.
func (s SecretRef) Value() (string, bool) { return s.value, s.resolved }

// Resolved reports whether a concrete value is available.
func (s SecretRef) Resolved() bool { return s.resolved }

// Reference returns the unresolved external reference, if any.
func (s SecretRef) Reference() string { return s.Ref }

// validate checks the shape of a secret reference without touching the
// environment or the filesystem.
func (s SecretRef) validate(env, path string, c *collector) {
	switch n := s.sources(); {
	case n == 0:
		c.add(path, "no secret source: set one of key_env or key_file")
		return
	case n > 1:
		c.add(path, "%d secret sources set: set exactly one of key_env, key_file or key", n)
		return
	}
	// A key_ref is recorded and never fetched: this build ships no resolver, so
	// the secret it names does not exist at request time. Accepting it meant a
	// credential that loaded, validated, and then sent every upstream request
	// with no Authorization header at all — the failure arriving as a 401 from
	// the provider, hours later, with nothing in the configuration to explain it.
	// It is refused here rather than resolved to nothing.
	if s.Ref != "" {
		c.add(path+".key_ref",
			"key_ref is not resolved by this build: no secret resolver ships with it, so a "+
				"credential declared this way would load with no usable secret and send "+
				"unauthenticated requests upstream. Use key_env or key_file — a vault agent "+
				"that writes a file or exports a variable satisfies both")
		return
	}
	if s.Inline != "" && env != EnvDevelopment {
		c.add(path+".key",
			"an inline literal secret is accepted only when server.env is %q (server.env is %q); "+
				"use key_env, key_file or key_ref instead", EnvDevelopment, env)
	}
}

// resolve reads the referenced secret. It appends to c on failure and never
// puts a secret value into an error.
func (s *SecretRef) resolve(env, path string, c *collector) {
	switch {
	case s.Inline != "":
		if env != EnvDevelopment {
			return // already reported by validate; never load the literal
		}
		s.value, s.resolved, s.fromFile = s.Inline, true, false
		s.Inline = "" // the literal does not survive resolution
	case s.Env != "":
		v, ok := os.LookupEnv(s.Env)
		if !ok {
			c.add(path+".key_env", "environment variable %s is not set", s.Env)
			return
		}
		if v == "" {
			c.add(path+".key_env", "environment variable %s is empty", s.Env)
			return
		}
		s.value, s.resolved, s.fromFile = v, true, false
	case s.File != "":
		b, err := os.ReadFile(ExpandPath(s.File))
		if err != nil {
			c.add(path+".key_file", "cannot read secret file: %s", redactPathError(err, s.File))
			return
		}
		v := strings.TrimRight(string(b), "\r\n")
		if v == "" {
			c.add(path+".key_file", "secret file %s is empty", s.File)
			return
		}
		s.value, s.resolved, s.fromFile = v, true, true
	case s.Ref != "":
		// Left for an external resolver; recorded, not fetched.
	}
}

// redactPathError renders a filesystem error without any chance of the file's
// contents appearing in it.
func redactPathError(err error, path string) string {
	msg := err.Error()
	switch {
	case os.IsNotExist(err):
		return "no such file: " + path
	case os.IsPermission(err):
		return "permission denied: " + path
	}
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		return path + ": " + msg[i+2:]
	}
	return path
}

// EnvStateDir relocates dorang's writable state: the embedded database, the
// trace spool, the generated key pepper, batch blobs and the shadow report.
//
// It exists because the container image declares
// `VOLUME ["/var/lib/dorang"]` and `ENV DORANG_STATE_DIR=/var/lib/dorang`, and
// nothing read the variable. The shipped defaults are all under `~`, which for
// the image's `nonroot` user is /home/nonroot — so every container wrote its
// database, its spool and its generated pepper OUTSIDE the declared volume, into
// the container's writable layer, and lost all of it on restart. The pepper is
// the sharp end of that: regenerating it makes every issued api key
// unverifiable.
const EnvStateDir = "DORANG_STATE_DIR"

// ExpandPath resolves a configured path.
//
// A leading "~" becomes the state directory when [EnvStateDir] is set, and the
// current user's home directory otherwise. Nothing else moves: an absolute path
// in the file is an instruction, and silently re-rooting it would make the
// variable a way to redirect a deliberate choice rather than a way to place the
// defaults.
//
// It is applied to paths this package reads; other packages call it on the paths
// they read (storage.sqlite.path, metering.spool.dir, shadow.report.path).
func ExpandPath(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	root := StateDir()
	if root == "" {
		return p
	}
	if p == "~" {
		return root
	}
	return root + string(os.PathSeparator) + p[2:]
}

// StateDir is where "~"-rooted state paths resolve to: DORANG_STATE_DIR when it
// is set, else the user's home directory, else "" when neither is available.
func StateDir() string {
	if v := strings.TrimSpace(os.Getenv(EnvStateDir)); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}
