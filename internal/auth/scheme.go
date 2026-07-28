package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// KeyPrefix is the mandatory prefix of every credential dorang accepts.
//
// The gate exists so that a stored digest cannot be replayed as a credential:
// rows hold hex, hex does not start with "sk-", and the prefix is checked
// before any lookup (R1-A).
const KeyPrefix = "sk-"

const (
	// LookupBytes is how many bytes of sha256(token) form the index key.
	LookupBytes = 16
	// LookupHexLen is the length of the hex-encoded index key, which is what
	// the api_keys.lookup column stores (DESIGN §2.4, §9.2).
	LookupHexLen = 2 * LookupBytes
	// MaxTokenLen is the longest credential handled on the allocation-free
	// path. Longer credentials are still accepted; they just allocate.
	MaxTokenLen = 512

	blockSize = 64 // sha256 block size, the HMAC padding width
)

// Scheme names how a stored digest was derived from its token.
type Scheme uint8

const (
	// SchemeUnknown is the zero value and never verifies.
	SchemeUnknown Scheme = iota
	// SchemeDorangV1 is HMAC-SHA256(pepper, token): the only scheme dorang
	// issues (DESIGN §2.4).
	SchemeDorangV1
	// SchemeLegacySHA256 is a plain unsalted sha256(token), accepted only
	// during a configured import window.
	SchemeLegacySHA256
)

// String returns the name used in configuration and in the api_keys.hash_scheme
// column.
func (s Scheme) String() string {
	switch s {
	case SchemeDorangV1:
		return "dorang_v1"
	case SchemeLegacySHA256:
		return "legacy_sha256"
	}
	return "unknown"
}

// ParseScheme decodes a stored hash_scheme value.
func ParseScheme(s string) (Scheme, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "dorang_v1":
		return SchemeDorangV1, nil
	case "legacy_sha256":
		return SchemeLegacySHA256, nil
	}
	return SchemeUnknown, fmt.Errorf("auth: unknown hash scheme %q", s)
}

// Lookup is the scheme-independent index key: the first LookupBytes bytes of
// sha256(token). It is an array rather than a string so that the hot path can
// use it as a map key without allocating.
type Lookup [LookupBytes]byte

// Hex renders the index key the way the store holds it.
func (l Lookup) Hex() string { return hex.EncodeToString(l[:]) }

// String implements fmt.Stringer. The index key is a truncated digest, not a
// credential, and is safe to log.
func (l Lookup) String() string { return l.Hex() }

// ParseLookup decodes a stored index key.
func ParseLookup(s string) (Lookup, error) {
	var l Lookup
	if len(s) != LookupHexLen {
		return l, fmt.Errorf("auth: lookup key must be %d hex characters, got %d", LookupHexLen, len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return l, fmt.Errorf("auth: lookup key is not hex: %w", err)
	}
	copy(l[:], b)
	return l, nil
}

// LookupKey returns the hex index key for a token, which is what a store query
// is keyed by. The hot path uses [Hasher.Lookup] instead, which does not
// allocate.
func LookupKey(token string) string {
	sum := sum256(token)
	return hex.EncodeToString(sum[:LookupBytes])
}

// Digest is a 32-byte hash. It is a digest, never a credential.
type Digest [sha256.Size]byte

// Hex renders the digest the way the store holds it.
func (d Digest) Hex() string { return hex.EncodeToString(d[:]) }

// String implements fmt.Stringer.
func (d Digest) String() string { return d.Hex() }

// ParseDigest decodes a stored token_hash value.
func ParseDigest(s string) (Digest, error) {
	var d Digest
	if len(s) != 2*sha256.Size {
		return d, fmt.Errorf("auth: digest must be %d hex characters, got %d", 2*sha256.Size, len(s))
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return d, fmt.Errorf("auth: digest is not hex: %w", err)
	}
	copy(d[:], b)
	return d, nil
}

// LegacyPolicy governs the legacy_sha256 import window (DESIGN §2.4).
//
// Enabling it without an Until date is a configuration error: an unsalted
// single-round digest is a permanent cryptographic constraint if it is allowed
// to become permanent, so the window must be bounded by construction.
type LegacyPolicy struct {
	// Enabled admits legacy_sha256 rows at all.
	Enabled bool
	// Until is the instant after which legacy verification is refused.
	// It is mandatory whenever Enabled is set.
	Until time.Time
}

// Validate reports whether the policy is a usable configuration at now. A
// legacy window that is enabled must carry a date, and that date must be in
// the future for the window to be worth configuring. Configuration loading
// should call this; [New] only requires that the date is present, so that a
// gateway whose window has simply lapsed keeps starting (with legacy inert)
// rather than refusing to boot.
func (p LegacyPolicy) Validate(now time.Time) error {
	if !p.Enabled {
		return nil
	}
	if p.Until.IsZero() {
		return ErrLegacyNoUntil
	}
	if !p.Until.After(now) {
		return fmt.Errorf("auth: auth.legacy.until (%s) is not in the future: %w",
			p.Until.UTC().Format(time.RFC3339), ErrLegacyExpiredWindow)
	}
	return nil
}

// Active reports whether legacy verification is admissible at now.
func (p LegacyPolicy) Active(now time.Time) bool {
	return p.Enabled && !p.Until.IsZero() && now.Before(p.Until)
}

// Configuration errors for the legacy window.
var (
	// ErrLegacyNoUntil is returned when auth.legacy.enabled is set without
	// auth.legacy.until. The date is mandatory (DESIGN §2.4).
	ErrLegacyNoUntil = errors.New("auth: auth.legacy.enabled requires auth.legacy.until")
	// ErrLegacyExpiredWindow reports a legacy window whose date has passed.
	ErrLegacyExpiredWindow = errors.New("auth: legacy window has expired")
	// ErrNoPepper is returned when no pepper is configured. dorang_v1 cannot
	// exist without one.
	ErrNoPepper = errors.New("auth: no key pepper configured (server.key_pepper_env)")
)

// Hasher derives and verifies credential digests.
//
// It holds the HMAC key schedule derived from the pepper, which is equivalent
// to holding the pepper, so it redacts itself under every fmt verb. A Hasher
// is immutable and safe for concurrent use.
type Hasher struct {
	ipad   [blockSize]byte
	opad   [blockSize]byte
	legacy LegacyPolicy
}

// NewHasher precomputes the HMAC key schedule for a pepper. The pepper is not
// retained in its original form and is never exposed again.
func NewHasher(pepper string, legacy LegacyPolicy) (*Hasher, error) {
	if pepper == "" {
		return nil, ErrNoPepper
	}
	if legacy.Enabled && legacy.Until.IsZero() {
		return nil, ErrLegacyNoUntil
	}
	h := &Hasher{legacy: legacy}
	key := []byte(pepper)
	if len(key) > blockSize {
		sum := sha256.Sum256(key)
		key = sum[:]
	}
	copy(h.ipad[:], key)
	copy(h.opad[:], key)
	for i := range h.ipad {
		h.ipad[i] ^= 0x36
		h.opad[i] ^= 0x5c
	}
	return h, nil
}

// Legacy returns the configured legacy window.
func (h *Hasher) Legacy() LegacyPolicy { return h.legacy }

// String redacts. It exists so a Hasher formatted with %v or %s can never
// print key material.
func (h Hasher) String() string { return "auth.Hasher{pepper:(redacted)}" }

// GoString redacts %#v.
func (h Hasher) GoString() string { return h.String() }

// Format redacts every other verb, including %x and %d over the raw struct.
func (h Hasher) Format(f fmt.State, verb rune) { writeRedacted(f, verb, h.String()) }

// digests computes, from one stack buffer and without allocating,
// sum = sha256(token) — which is both the index key source and the
// legacy_sha256 digest — and mac = HMAC-SHA256(pepper, token).
func (h *Hasher) digests(token string) (sum, mac Digest) {
	if len(token) > MaxTokenLen {
		return h.digestsSlow(token)
	}
	var buf [blockSize + MaxTokenLen]byte
	copy(buf[:blockSize], h.ipad[:])
	n := copy(buf[blockSize:], token)

	sum = sha256.Sum256(buf[blockSize : blockSize+n])
	inner := sha256.Sum256(buf[:blockSize+n])

	var obuf [blockSize + sha256.Size]byte
	copy(obuf[:blockSize], h.opad[:])
	copy(obuf[blockSize:], inner[:])
	mac = sha256.Sum256(obuf[:])
	return sum, mac
}

// digestsSlow handles credentials longer than MaxTokenLen. It allocates; no
// realistic credential reaches it.
func (h *Hasher) digestsSlow(token string) (sum, mac Digest) {
	buf := make([]byte, blockSize+len(token))
	copy(buf[:blockSize], h.ipad[:])
	copy(buf[blockSize:], token)
	sum = sha256.Sum256(buf[blockSize:])
	inner := sha256.Sum256(buf)
	var obuf [blockSize + sha256.Size]byte
	copy(obuf[:blockSize], h.opad[:])
	copy(obuf[blockSize:], inner[:])
	mac = sha256.Sum256(obuf[:])
	return sum, mac
}

// sum256 is sha256 over a token without allocating for reasonable lengths.
func sum256(token string) Digest {
	if len(token) > MaxTokenLen {
		return sha256.Sum256([]byte(token))
	}
	var buf [MaxTokenLen]byte
	n := copy(buf[:], token)
	return sha256.Sum256(buf[:n])
}

// Lookup returns the scheme-independent index key for a token.
func (h *Hasher) Lookup(token string) Lookup {
	sum := sum256(token)
	var l Lookup
	copy(l[:], sum[:LookupBytes])
	return l
}

// Hash returns the digest a token would be stored under in the given scheme.
// It is what an importer and the rehash worker write.
func (h *Hasher) Hash(scheme Scheme, token string) (Digest, error) {
	sum, mac := h.digests(token)
	switch scheme {
	case SchemeDorangV1:
		return mac, nil
	case SchemeLegacySHA256:
		return sum, nil
	}
	return Digest{}, fmt.Errorf("auth: cannot hash under scheme %q", scheme)
}

// VerifyToken checks a token against a stored digest under a stored scheme.
//
// The scheme selects which digest is compared, but the selection is
// branchless: both digests are computed and the comparison target is chosen
// with crypto/subtle, so the work done does not depend on the credential.
// A legacy row is refused outright when the import window is closed.
func (h *Hasher) VerifyToken(scheme Scheme, want Digest, token string, now time.Time) error {
	sum, mac := h.digests(token)
	return h.verify(scheme, want, sum, mac, now)
}

// verify is the hot-path half of VerifyToken, taking digests the caller has
// already computed.
func (h *Hasher) verify(scheme Scheme, want, sum, mac Digest, now time.Time) error {
	switch scheme {
	case SchemeDorangV1:
		// admitted unconditionally
	case SchemeLegacySHA256:
		if !h.legacy.Enabled {
			return ErrLegacyDisabled
		}
		if !h.legacy.Active(now) {
			return ErrLegacyWindowClosed
		}
	default:
		return ErrSchemeUnsupported
	}

	// Branchless selection between the two candidate digests. The scheme is a
	// property of the stored row, not of the credential, but selecting with
	// subtle keeps the comparison itself free of any data-dependent branch.
	var got Digest
	isV1 := subtle.ConstantTimeEq(int32(scheme), int32(SchemeDorangV1))
	subtle.ConstantTimeCopy(isV1, got[:], mac[:])
	subtle.ConstantTimeCopy(1-isV1, got[:], sum[:])

	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		return ErrDigestMismatch
	}
	return nil
}

// writeRedacted implements a redacting fmt.Formatter for any verb.
func writeRedacted(f fmt.State, verb rune, s string) {
	switch verb {
	case 'q':
		fmt.Fprintf(f, "%q", s)
	default:
		_, _ = f.Write([]byte(s))
	}
}
