// Package config loads, validates, and hot-reloads dorang's configuration file.
//
// The file is the baseline: a single file must be enough to run the gateway
// (design §4.1). Loading has four phases, in this order:
//
//  1. decode  — strict YAML decoding; an unknown key is an error, not a shrug
//  2. default — every unset field takes the documented default
//  3. resolve — secret references are read from the environment or from disk
//  4. validate — every problem is collected, then reported at once
//
// Phases 3 and 4 never stop at the first problem. [Load] returns a
// *[ValidationError] carrying one entry per problem, each with the YAML path it
// was found at, so one run of the gateway reports one full list of what is
// wrong with the file.
//
// # Secrets
//
// A secret never appears in the configuration file. Every key-bearing element
// carries a [SecretRef] with exactly one of:
//
//	key_env:  NAME          read from the process environment
//	key_file: /path/to/key  read from a file, trailing newline trimmed
//	key_ref:  vault:…       recorded verbatim for an external resolver
//	key:      literal       accepted only when server.env is "development"
//
// A resolved secret is held in an unexported field. It is never rendered by
// String, %v, %#v, YAML marshaling, or any error message this package
// produces.
//
// # Model names are opaque
//
// Design §2.1: a model name is an opaque string. Nothing in this package splits
// a model name on ':' or '/' — not validation, not lookup, not import.
// Provider identity comes only from explicit configuration fields. [ImportProxyConfig]
// resolves a "provider/model" form by matching against the declared provider
// list and reports what it cannot resolve rather than guessing.
package config
