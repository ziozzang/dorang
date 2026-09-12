package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// Load reads, defaults, resolves and validates a configuration file.
//
// Every problem is reported at once: the returned error is a
// *[ValidationError] listing each problem with the YAML path it was found at.
// A secret value never appears in it.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return load(data, path)
}

// LoadBytes is [Load] for configuration already in memory.
func LoadBytes(data []byte) (*Config, error) { return load(data, "") }

func load(data []byte, source string) (*Config, error) {
	c, err := decodeConfig(data, source)
	if err != nil {
		return nil, err
	}
	c.ApplyDefaults()

	col := &collector{}
	c.validate(col)       // structure first — no I/O, no environment
	c.resolveSecrets(col) // then read what the structure points at
	if err := col.err(source); err != nil {
		return nil, err
	}
	c.buildIndex()
	return c, nil
}

// decodeConfig decodes YAML strictly: an unknown key is a problem to report,
// not a line to ignore. A typo in a configuration file is otherwise invisible
// until the behavior it was meant to change fails to happen.
func decodeConfig(data []byte, source string) (*Config, error) {
	c := &Config{source: source}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		if errors.Is(err, io.EOF) {
			return &Config{source: source}, nil // an empty file is all defaults
		}
		return nil, parseError(err, source)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, &ValidationError{Source: source, Errors: []FieldError{{
			Message: "the file holds more than one YAML document; dorang reads exactly one",
		}}}
	}
	return c, nil
}

// parseError turns a YAML decode failure into the same aggregate shape as
// validation, so a caller has one error type to render.
func parseError(err error, source string) error {
	var te *yaml.TypeError
	if errors.As(err, &te) {
		fe := make([]FieldError, 0, len(te.Errors))
		for _, m := range te.Errors {
			fe = append(fe, FieldError{Message: m})
		}
		return &ValidationError{Source: source, Errors: fe}
	}
	return &ValidationError{Source: source, Errors: []FieldError{{Message: err.Error()}}}
}

// resolveSecrets reads every referenced secret. A key_ref is recorded, not
// fetched: an external resolver owns it.
func (c *Config) resolveSecrets(col *collector) {
	env := c.Server.Env
	for i := range c.Credentials {
		if c.Credentials[i].Key.sources() == 1 {
			c.Credentials[i].Key.resolve(env, fmt.Sprintf("credentials[%d]", i), col)
		}
		// An OAuth client secret is a secret like any other (§4.1): it is a
		// reference in the file and a value only in memory, resolved by the same
		// code as every other one so that there is no second way for one to be
		// read.
		if o := c.Credentials[i].OAuth; o != nil && o.Refresh.ClientSecret.sources() == 1 {
			o.Refresh.ClientSecret.resolve(env,
				fmt.Sprintf("credentials[%d].oauth.refresh", i), col)
		}
	}
	// A Bedrock session token is a short-lived AWS credential, so it is a
	// reference like every other secret rather than a literal in the file.
	for i := range c.Providers {
		if c.Providers[i].Params.SessionToken.sources() == 1 {
			c.Providers[i].Params.SessionToken.resolve(env,
				fmt.Sprintf("providers[%d].params.session_token", i), col)
		}
	}
	for _, name := range sortedKeys(c.KeyRotation.Providers) {
		kp := c.KeyRotation.Providers[name]
		for i := range kp.Keys {
			if kp.Keys[i].Key.sources() == 1 {
				kp.Keys[i].Key.resolve(env,
					fmt.Sprintf("key_rotation.providers[%q].keys[%d]", name, i), col)
			}
		}
		c.KeyRotation.Providers[name] = kp // the map holds a copy of the struct
	}
	// The SMTP password is a secret like any other: it lives in the
	// environment, a file or a vault reference, never in the file (§4.1).
	if c.Notifications.Email.SMTP.Password.sources() == 1 {
		c.Notifications.Email.SMTP.Password.resolve(env, "notifications.email.smtp", col)
	}
	if c.Notifications.Email.HTTP.Secret.sources() == 1 {
		c.Notifications.Email.HTTP.Secret.resolve(env, "notifications.email.http", col)
	}
	// §10.5b: the placeholder seed. It is cluster-wide rather than per-process,
	// which is exactly why it is a reference and not a value generated at start.
	if c.Filters.Secret.sources() == 1 {
		c.Filters.Secret.resolve(env, "filters.secret", col)
	}
}
