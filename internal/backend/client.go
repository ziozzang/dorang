package backend

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// NewClient is the HTTP client the backend uses when the caller supplies none.
//
// Redirects are not followed, and that is a security property rather than a
// preference (DESIGN §10.6). A 30x from a provider endpoint makes an ordinary
// HTTP client re-issue the request — with the provider credential attached — to
// a host the UPSTREAM chose. That turns any compromised or misconfigured
// backend into a credential-exfiltration primitive, and it needs no attacker
// access to dorang at all. Returning the last response instead lets
// [Backend.Do] refuse it by status.
func NewClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          256,
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			// A streaming relay must see bytes as they arrive, and dorang has to
			// read the terminal usage frame, so the response is never compressed
			// on the wire between here and the provider.
			DisableCompression: true,
		},
	}
}

// Backoff kinds, matching the configuration vocabulary.
const (
	BackoffExponential = "exponential"
	BackoffLinear      = "linear"
	BackoffConstant    = "constant"
)

// Policy is the in-provider retry policy — `providers[].retry` — which is a
// different thing from the fallback chain of §7.6.
//
// # What is retried, and why it is so little
//
// A chat completion is not idempotent and carries no idempotency key. Retrying
// one that MAY have been executed bills the caller twice for an answer they
// receive once, and no response says which happened. So an attempt is retried
// here only when it provably never reached the model: the connection was never
// established. Everything after that — a 5xx, a 429, a timeout while
// generating — is the fallback chain's decision, and the chain is the better
// instrument anyway because it can move to a deployment that is not broken.
//
// This is narrower than a reader of `max_attempts: 2` might assume, so it is
// stated here rather than implied by the code.
type Policy struct {
	// MaxAttempts counts the first attempt. Zero and one both mean no retry.
	MaxAttempts int
	// Backoff is one of the Backoff* constants. Empty means exponential.
	Backoff string
	// Base is the first wait. Zero means no wait between attempts.
	Base time.Duration
}

func (p Policy) withDefaults() Policy {
	if p.MaxAttempts < 1 {
		p.MaxAttempts = 1
	}
	if p.Backoff == "" {
		p.Backoff = BackoffExponential
	}
	return p
}

// attempts is the number of attempts this policy permits, at least one.
func (p Policy) attempts() int {
	if p.MaxAttempts < 1 {
		return 1
	}
	return p.MaxAttempts
}

// wait is the pause before attempt n, where n counts from one and n == 1 is the
// first retry.
func (p Policy) wait(n int) time.Duration {
	if p.Base <= 0 || n < 1 {
		return 0
	}
	switch p.Backoff {
	case BackoffConstant:
		return p.Base
	case BackoffLinear:
		return p.Base * time.Duration(n)
	default:
		// Exponential, doubling from Base. Shifting rather than multiplying
		// keeps a long chain from overflowing into a negative duration.
		if n > 20 {
			n = 20
		}
		return p.Base << (n - 1)
	}
}
