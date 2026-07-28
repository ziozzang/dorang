package auth

import (
	"net/http"
	"net/textproto"
	"strings"
)

// The six accepted authentication header names (COMPATIBILITY §7.3). Any one
// of them authenticates a request, and every one of them is stripped before
// the request is forwarded upstream.
const (
	// HeaderAuthorization carries "Bearer <token>".
	HeaderAuthorization = "Authorization"
	// HeaderAPIKey is the plain proxy-style header.
	HeaderAPIKey = "API-Key"
	// HeaderXAPIKey is the Anthropic-style header.
	HeaderXAPIKey = "x-api-key"
	// HeaderXGoogAPIKey is the Google-style header.
	HeaderXGoogAPIKey = "x-goog-api-key"
	// HeaderAzureAPIKey is the Azure API Management style header.
	HeaderAzureAPIKey = "Ocp-Apim-Subscription-Key" // pragma: allowlist secret — header name, not a credential
	// HeaderDorangAPIKey is dorang's own proxy-specific header.
	HeaderDorangAPIKey = "x-dorang-api-key" // pragma: allowlist secret — header name, not a credential
)

// bearerPrefix is matched case-insensitively, as RFC 7235 requires.
const bearerPrefix = "bearer "

// accepted lists the six header names in the order they are consulted. The
// order only decides which header wins when a client sends several; any one of
// them authenticates.
var accepted = [6]string{
	HeaderAuthorization,
	HeaderAPIKey,
	HeaderXAPIKey,
	HeaderXGoogAPIKey,
	HeaderAzureAPIKey,
	HeaderDorangAPIKey,
}

// acceptedCanonical is the same list in net/textproto canonical form, which is
// how a server-parsed http.Header keys them.
var acceptedCanonical = func() [6]string {
	var out [6]string
	for i, h := range accepted {
		out[i] = textproto.CanonicalMIMEHeaderKey(h)
	}
	return out
}()

// Headers returns the accepted header names, in consultation order.
func Headers() []string {
	out := make([]string, len(accepted))
	copy(out, accepted[:])
	return out
}

// Extract pulls a credential out of a request's headers and reports which
// header supplied it.
//
// Authorization is read as "Bearer <token>", matched case-insensitively. A
// bare Authorization value with no scheme and no space is also accepted,
// because clients configured with a proxy base URL commonly send one; any
// other authentication scheme (Basic, Negotiate, …) is ignored rather than
// misread as a token.
func Extract(h http.Header) (token, header string, ok bool) {
	if h == nil {
		return "", "", false
	}
	for i, name := range acceptedCanonical {
		v := strings.TrimSpace(h.Get(name))
		if v == "" {
			continue
		}
		if accepted[i] == HeaderAuthorization {
			if t, good := parseAuthorization(v); good {
				return t, accepted[i], true
			}
			continue
		}
		return v, accepted[i], true
	}
	// A header map built by hand rather than parsed off the wire may hold
	// non-canonical keys, which Get would miss. Authentication is worth the
	// second, slower pass.
	for k, vs := range h {
		idx := acceptedIndex(k)
		if idx < 0 || len(vs) == 0 {
			continue
		}
		v := strings.TrimSpace(vs[0])
		if v == "" {
			continue
		}
		if accepted[idx] == HeaderAuthorization {
			if t, good := parseAuthorization(v); good {
				return t, accepted[idx], true
			}
			continue
		}
		return v, accepted[idx], true
	}
	return "", "", false
}

// parseAuthorization reads a credential out of an Authorization value.
func parseAuthorization(v string) (string, bool) {
	if len(v) >= len(bearerPrefix) && strings.EqualFold(v[:len(bearerPrefix)], bearerPrefix) {
		t := strings.TrimSpace(v[len(bearerPrefix):])
		return t, t != ""
	}
	if strings.ContainsAny(v, " \t") {
		return "", false // some other authentication scheme; not ours to read
	}
	return v, true
}

// Strip removes every accepted authentication header from h.
//
// A client credential must never reach a provider (COMPATIBILITY §7.3): the
// upstream credential is set by the transport after this call, so stripping
// first is what guarantees the two cannot both be present.
func Strip(h http.Header) {
	if len(h) == 0 {
		return
	}
	for _, name := range acceptedCanonical {
		delete(h, name)
	}
	// Delete non-canonical spellings too; a header map assembled in code can
	// hold them, and a forwarded credential is not a mistake worth making
	// cheaply recoverable.
	for k := range h {
		if acceptedIndex(k) >= 0 {
			delete(h, k)
		}
	}
}

// acceptedIndex reports the position of a header name in the accepted list,
// case-insensitively, or -1.
func acceptedIndex(name string) int {
	for i, a := range accepted {
		if strings.EqualFold(name, a) {
			return i
		}
	}
	return -1
}
