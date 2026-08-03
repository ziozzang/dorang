// Package redact removes credential material from text that is about to be
// recorded or relayed.
//
// It is a backstop, not the mechanism. The mechanism is that dorang's
// client-facing error envelope carries dorang's own words and never the
// upstream's (COMPATIBILITY §11.3), so a provider that echoes the key it was
// given has nothing to echo it into. This package covers what is left: the
// native message IS kept, out of band, in the ledger and the operator's log,
// and that copy can carry a secret two ways nobody chose —
//
//   - a provider that authenticates in a query parameter puts the key in every
//     transport error, because net/http's *url.Error quotes the request URL,
//     and it arrives percent-encoded rather than plain;
//   - a provider that answers 401 with {"error":{"message":"Invalid API key:
//     sk-…"}} puts it in the body, which several OpenAI-compatible servers do.
//
// # This is the authoritative implementation
//
// There are three scrubbers in the tree and they were not all the same rule.
// internal/probe's `scrubber` agrees with this package exactly — same minimum
// length, same two spellings, same placeholder — and differs only in being a
// stateful type that remembers a few token generations across an OAuth refresh.
// internal/backend/errors.go carries an inline `scrub` that agrees with
// NEITHER, and it is the copy on the path that matters: it runs over upstream
// error bodies, over the ledger's native message, and over the SSE error frame
// that is rewritten before it reaches the client.
//
// The inline copy differs in both directions at once, which is why the
// difference outranks the duplication:
//
//   - it searches the PLAIN spelling only, so the percent-encoded case above
//     survives it. A credential echoed back inside a quoted URL is relayed;
//     this package removes it.
//   - it has no [MinLen], so a short credential — `vllm serve --api-key test`
//     is the ordinary way a self-hosted deployment is brought up — matches
//     inside ordinary words. The message `the latest test run … contest the
//     quota` comes out as `the la[redacted] [redacted] run … con[redacted] the
//     quota`, and on the streaming path that mangling is not cosmetic: a
//     rewritten frame is how the relay signals "the upstream echoed the
//     credential", so the client is handed a corrupted error frame and the
//     relay believes a leak occurred.
//
// This package is the one both the other two are measured against, and
// internal/backend is to import it in place of the inline copy. Until that
// lands the inline copy is still what runs on the dispatch path — the comments
// in internal/server say who scrubs rather than naming a package that is not on
// the path, which is the state this note exists to keep honest.
package redact

import (
	"net/url"
	"strings"
)

// Placeholder replaces a secret wherever one is found.
const Placeholder = "[redacted]"

// MinLen is the shortest string worth replacing.
//
// A short secret cannot be told apart from ordinary text, and replacing it
// mangles the message without protecting anything: a four-character key would
// match inside half the words in an error string.
const MinLen = 8

// Text removes every given secret from in, in both its plain and its
// URL-escaped spelling.
//
// Empty and short secrets are ignored rather than matched, so passing an
// unresolved credential does not turn the message into placeholders.
func Text(in string, secrets ...string) string {
	if in == "" {
		return in
	}
	for _, s := range secrets {
		if len(s) < MinLen {
			continue
		}
		in = strings.ReplaceAll(in, s, Placeholder)
		if esc := url.QueryEscape(s); esc != s {
			in = strings.ReplaceAll(in, esc, Placeholder)
		}
	}
	return in
}

// Contains reports whether in holds any of the given secrets, in either
// spelling. It is for assertions and for deciding whether to record at all.
func Contains(in string, secrets ...string) bool {
	for _, s := range secrets {
		if len(s) < MinLen {
			continue
		}
		if strings.Contains(in, s) {
			return true
		}
		if esc := url.QueryEscape(s); esc != s && strings.Contains(in, esc) {
			return true
		}
	}
	return false
}
