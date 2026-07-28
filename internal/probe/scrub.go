package probe

import (
	"net/url"
	"strings"
	"sync"
)

const (
	// minScrubLen is the shortest string worth replacing. A short secret cannot
	// be distinguished from ordinary text, and replacing it mangles the message
	// without protecting anything.
	minScrubLen = 8
	// maxRememberedSecrets bounds the scrub list. A credential presents one
	// token at a time; an OAuth one presents a new token after each refresh
	// (DESIGN §11.2b), and an error can be recorded about a token that has just
	// been replaced, so a few generations are kept.
	maxRememberedSecrets = 4
)

// scrubber removes credential material from text that is about to be recorded.
//
// It is a backstop, not the mechanism. Nothing in this package interpolates a
// secret into a message: errors carry a sentinel, a provider id and a status,
// and never a response body. The backstop exists because two things outside
// this package's control can carry a secret into text anyway — net/http's
// *url.Error quotes the request URL, and a provider that authenticates in a
// query parameter therefore puts the key in every transport error; and an
// [Auth] is provider code whose errors this package refuses to wrap for exactly
// that reason.
//
// A scrubber is safe for concurrent use.
type scrubber struct {
	mu      sync.Mutex
	secrets []string
}

// remember adds a secret to the scrub list, most recent first.
func (s *scrubber) remember(secret string) {
	if len(secret) < minScrubLen {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, have := range s.secrets {
		if have == secret {
			return
		}
	}
	s.secrets = append([]string{secret}, s.secrets...)
	if len(s.secrets) > maxRememberedSecrets {
		s.secrets = s.secrets[:maxRememberedSecrets]
	}
}

// text removes every remembered secret from in, in both its plain and its
// URL-escaped spelling — a key in a query parameter reaches an error message
// percent-encoded, and a plain-text search would miss it.
func (s *scrubber) text(in string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, secret := range s.secrets {
		in = strings.ReplaceAll(in, secret, redacted)
		if esc := url.QueryEscape(secret); esc != secret {
			in = strings.ReplaceAll(in, esc, redacted)
		}
	}
	return in
}
