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
// internal/probe states this rule and implements it correctly for its own
// error sites. The rule belongs in one place both it and the dispatch path can
// reach, because the shipped dispatch path had no scrubber at all — the same
// "the correct code exists and the path that runs is not the path that has it"
// shape as the controls it was written to protect.
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
