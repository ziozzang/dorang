package metrics

import (
	"time"

	"github.com/ziozzang/dorang/internal/auth"
)

// AuthSource is the part of [auth.Authenticator] this package needs.
type AuthSource interface {
	Stats() auth.Stats
}

// AuthCollector renders the O(1) authentication cache of DESIGN §15.2.6 and the
// credential-rehash migration of §2.4.
//
// The rehash counters are a migration progress bar and are treated as one. They
// are emitted only when rehash-on-use is actually configured, because with the
// feature off `dorang_auth_rehash_done_total` sits at zero forever — which
// reads as "the migration is not progressing" when the truth is "the migration
// is not running". Those need different actions from an operator, so they must
// not look the same.
type AuthCollector struct {
	src AuthSource
	// RehashOnUse mirrors auth.rehash_on_use.
	rehash bool
	// LegacyUntil is auth.legacy.until, the date after which legacy
	// verification is refused. Zero omits the countdown.
	legacyUntil time.Time
	now         func() time.Time
}

// NewAuthCollector builds the collector.
func NewAuthCollector(src AuthSource, rehashOnUse bool, legacyUntil time.Time, now func() time.Time) *AuthCollector {
	if now == nil {
		now = time.Now
	}
	return &AuthCollector{src: src, rehash: rehashOnUse, legacyUntil: legacyUntil, now: now}
}

// CollectorName implements [Collector].
func (c *AuthCollector) CollectorName() string { return "auth" }

// Collect implements [Collector].
func (c *AuthCollector) Collect(w *Writer) {
	st := c.src.Stats()

	w.Metric("dorang_auth_cache_hits_total", Counter,
		"Credential lookups answered from the lock-free snapshot.")
	w.Uint(st.Hits)

	w.Metric("dorang_auth_cache_misses_total", Counter,
		"Credential lookups that fell through to the overlay or the store. This is the "+
			"`cold-auth` profile of DESIGN §15.1, whose budget is 15 ms against a "+
			"warm-local 200 µs.")
	w.Uint(st.Misses)

	// Absent until something has been looked up. A hit ratio of 0.0 on an idle
	// process would read as a cache that never works.
	if total := st.Hits + st.Misses; total > 0 {
		w.Metric("dorang_auth_cache_hit_ratio", Gauge,
			"Fraction in [0,1] of credential lookups served from the snapshot. Absent "+
				"until a lookup has happened.")
		w.Float(float64(st.Hits) / float64(total))
	}

	w.Metric("dorang_auth_store_calls_total", Counter,
		"Store reads issued for a credential miss.")
	w.Uint(st.StoreCalls)

	w.Metric("dorang_auth_lookup_throttled_total", Counter,
		"Credential lookups refused WITHOUT consulting the store because the "+
			"unknown-key budget was empty. Every distinct unknown key used to be one "+
			"database round trip an unauthenticated caller could buy for nothing; this "+
			"is what that amplifier costs now. Read against "+
			"dorang_auth_store_calls_total: this one rising while that one flattens is "+
			"the bound holding. A sustained non-zero value with no attack means the "+
			"authenticator's miss budget is below the deployment's real rate of lookups "+
			"that find nothing — a client retrying a key that was revoked, most likely.")
	w.Uint(st.LookupsThrottled)

	w.Metric("dorang_auth_coalesced_total", Counter,
		"Misses that joined an in-flight store read instead of issuing their own. A "+
			"cold start on a hot key should produce one store call, not a thundering herd.")
	w.Uint(st.Coalesced)

	w.Metric("dorang_auth_rejected_total", Counter,
		"Requests refused for a missing or unusable credential before any lookup.")
	w.Uint(st.Rejected)

	w.Metric("dorang_auth_master_key_uses_total", Counter,
		"Requests authenticated with the master key.")
	w.Uint(st.MasterHits)

	w.Metric("dorang_auth_snapshot_keys", Gauge,
		"Credentials in the immutable snapshot the request path reads.")
	w.Int(int64(st.SnapshotSize))

	w.Metric("dorang_auth_overlay_keys", Gauge,
		"Credentials learned at runtime and not yet folded into the snapshot.")
	w.Int(int64(st.OverlaySize))

	if !c.rehash {
		return
	}

	w.Metric("dorang_auth_rehash_queued_total", Counter,
		"Credentials seen on the legacy_sha256 scheme and queued for upgrade to "+
			"dorang_v1 (DESIGN §2.4). Counted once per cached entry, so it is the number "+
			"of distinct legacy keys that have actually been used — which is the "+
			"denominator of the migration, not an estimate of it.")
	w.Uint(st.RehashQueued)

	w.Metric("dorang_auth_rehash_done_total", Counter,
		"Credentials upgraded off legacy_sha256. Queued minus done minus dropped is how "+
			"many used keys are still on the legacy scheme: the migration's progress bar, "+
			"and it must reach zero before auth.legacy.until.")
	w.Uint(st.RehashDone)

	w.Metric("dorang_auth_rehash_dropped_total", Counter,
		"Upgrades dropped because the rehash queue was full. These keys stay on the "+
			"legacy scheme and will be re-queued the next time they are used, so a "+
			"persistent non-zero value means the migration is not converging.")
	w.Uint(st.RehashDropped)

	w.Metric("dorang_auth_rehash_pending", Gauge,
		"Used credentials still on legacy_sha256: queued minus completed minus dropped.")
	w.Int(int64(st.RehashQueued) - int64(st.RehashDone) - int64(st.RehashDropped))

	if !c.legacyUntil.IsZero() {
		w.Metric("dorang_auth_legacy_sunset_seconds", Gauge,
			"Seconds until auth.legacy.until, after which legacy_sha256 verification is "+
				"refused and any key still on it stops working. Absent when no legacy "+
				"window is configured — an open-ended legacy window is a permanent one, "+
				"which DESIGN §2.4 refuses to allow.")
		w.Float(c.legacyUntil.Sub(c.now()).Seconds())
	}
}

// OAuthSource is the part of [auth.OAuthManager] this package needs.
type OAuthSource interface {
	Snapshot() []auth.CredentialHealth
}

// OAuthCollector renders the self-refreshing credentials of DESIGN §11.2b.
//
// A refresh loop that has quietly been failing for three days is invisible until
// the token it failed to replace finally expires, at which point every request
// on that credential fails at once. The three numbers that predict it —
// consecutive failures, time until the next backoff attempt, and time until the
// current token expires — are all here, and the last one is the one that says
// how long there is to fix it.
//
// This collector is registered only when OAuth credentials exist. With none
// configured there are no series at all, rather than a healthy-looking zero.
type OAuthCollector struct {
	src OAuthSource
	now func() time.Time
	max int
	f   folder
}

// NewOAuthCollector builds the collector.
func NewOAuthCollector(src OAuthSource, now func() time.Time, maxCredentials int) *OAuthCollector {
	if now == nil {
		now = time.Now
	}
	if maxCredentials <= 0 {
		maxCredentials = DefaultMaxCredentialSeries
	}
	return &OAuthCollector{src: src, now: now, max: maxCredentials,
		f: folder{family: "dorang_credential_health"}}
}

// CollectorName implements [Collector].
func (c *OAuthCollector) CollectorName() string { return "oauth" }

func (c *OAuthCollector) folders() []*folder { return []*folder{&c.f} }

// credentialStates is the state set of `dorang_credential_health`.
var credentialStates = [...]string{"healthy", "unhealthy"}

// Collect implements [Collector].
func (c *OAuthCollector) Collect(w *Writer) {
	creds := c.src.Snapshot()
	if len(creds) == 0 {
		return
	}
	now := c.now()

	// A credential's health does not aggregate, so the tail past the cap is
	// dropped and counted rather than folded into a single misleading series.
	if len(creds) > c.max {
		for range creds[c.max:] {
			c.f.fold()
		}
		creds = creds[:c.max]
	}

	w.Metric("dorang_credential_health", Gauge,
		"1 against a credential's health state, 0 against the other (DESIGN §12.3). An "+
			"unhealthy credential may still hold a working token — a failed refresh does "+
			"not invalidate the token it failed to replace — so this steps the credential "+
			"aside exactly as an exhausted quota does, rather than making it unusable.")
	for _, cr := range creds {
		for _, s := range credentialStates {
			w.Label("credential", cr.ID)
			w.Label("provider", cr.Provider)
			w.Label("state", s)
			w.Bool((s == "healthy") == cr.Healthy)
		}
	}

	w.Metric("dorang_oauth_refreshes_total", Counter,
		"Successful token renewals per credential (DESIGN §11.2b).")
	for _, cr := range creds {
		w.Label("credential", cr.ID)
		w.Label("provider", cr.Provider)
		w.Uint(cr.Refreshes)
	}

	w.Metric("dorang_oauth_store_loads_total", Counter,
		"Tokens adopted from the credential's store rather than exchanged for (DESIGN "+
			"§11.2b). A deployment with no refresh endpoint configured — the safe default, "+
			"where the vendor's own CLI keeps the token current and dorang only reads it — "+
			"shows loads and no refreshes, and this is the ONLY number that moves for it. "+
			"A store that silently stopped being updated otherwise looks exactly like one "+
			"that is fine.")
	for _, cr := range creds {
		w.Label("credential", cr.ID)
		w.Label("provider", cr.Provider)
		w.Uint(cr.StoreLoads)
	}

	w.Metric("dorang_oauth_consecutive_failures", Gauge,
		"Consecutive failed refreshes per credential. This is the number that rises "+
			"silently while the current token still works.")
	for _, cr := range creds {
		w.Label("credential", cr.ID)
		w.Label("provider", cr.Provider)
		w.Int(int64(cr.Failures))
	}

	w.Metric("dorang_oauth_backoff_seconds", Gauge,
		"Seconds until the backoff allows another refresh attempt. Absent when no "+
			"backoff is in effect: zero would mean 'retrying now', which is the opposite "+
			"of 'not retrying at all'.")
	for _, cr := range creds {
		if cr.NextAttempt.IsZero() || !cr.NextAttempt.After(now) {
			continue
		}
		w.Label("credential", cr.ID)
		w.Label("provider", cr.Provider)
		w.Float(cr.NextAttempt.Sub(now).Seconds())
	}

	w.Metric("dorang_oauth_token_expires_seconds", Gauge,
		"Seconds until the current access token stops working. Absent when the store "+
			"did not record an expiry — this is metadata about a token, never the token. "+
			"Read together with the failure count it is how long there is to fix a "+
			"refresh loop that has already broken.")
	for _, cr := range creds {
		if cr.ExpiresAt.IsZero() {
			continue
		}
		w.Label("credential", cr.ID)
		w.Label("provider", cr.Provider)
		w.Float(cr.ExpiresAt.Sub(now).Seconds())
	}

	w.Metric("dorang_oauth_last_refresh_age_seconds", Gauge,
		"Seconds since the last successful renewal. Absent for a credential that has "+
			"never renewed.")
	for _, cr := range creds {
		if cr.LastRefresh.IsZero() {
			continue
		}
		w.Label("credential", cr.ID)
		w.Label("provider", cr.Provider)
		w.Float(now.Sub(cr.LastRefresh).Seconds())
	}
}

var (
	_ AuthSource  = (*auth.Authenticator)(nil)
	_ OAuthSource = (*auth.OAuthManager)(nil)
)
