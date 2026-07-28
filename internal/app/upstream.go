package app

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// upstream is one configured provider, resolved: everything the backend half of
// a request needs, in one struct that never changes after a reload builds it.
type upstream struct {
	name    string
	kind    string
	api     catalog.API
	baseURL string
	timeout time.Duration
}

// upstreamTable is the provider and credential lookup the dispatcher uses. It
// is immutable; a hot reload builds a new one and swaps the pointer.
type upstreamTable struct {
	providers map[string]*upstream
	creds     map[string]*credential
}

// newUpstreamTable resolves providers[] and the credential set.
//
// A provider with no base URL falls back to the base URL its kind declares in
// the catalog (DESIGN §4.3): the kind's URL is a DEFAULT for configuration, and
// a deployment that does not override it is asking for exactly that default.
// A provider that ends up with no URL at all is a start-up failure, because the
// alternative is a 502 on the first request that names it.
func newUpstreamTable(cfg *config.Config, cat *catalog.Catalog) (*upstreamTable, error) {
	t := &upstreamTable{
		providers: make(map[string]*upstream, len(cfg.Providers)),
		creds:     collectCredentials(cfg),
	}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		base := trimBase(p.BaseURL)
		if base == "" {
			if kd, ok := cat.Kind(p.Kind); ok {
				base = trimBase(kd.BaseURL)
			}
		}
		if base == "" {
			return nil, fmt.Errorf(
				"app: provider %q has no base_url and its kind %q declares none", p.Name, p.Kind)
		}
		if _, err := url.Parse(base); err != nil {
			return nil, fmt.Errorf("app: provider %q base_url: %w", p.Name, err)
		}
		t.providers[p.Name] = &upstream{
			name:    p.Name,
			kind:    p.Kind,
			api:     apiFor(cat, p.Kind),
			baseURL: base,
			timeout: p.Timeout.Duration(),
		}
	}
	return t, nil
}

func (t *upstreamTable) provider(name string) (*upstream, bool) {
	u, ok := t.providers[name]
	return u, ok
}

func (t *upstreamTable) secret(credentialID string) string {
	if c, ok := t.creds[credentialID]; ok {
		return c.secret
	}
	return ""
}

// Endpoint suffixes, per wire adapter.
const (
	pathChatCompletions = "/chat/completions"
	pathCompletions     = "/completions"
	pathEmbeddings      = "/embeddings"
	pathMessages        = "/messages"
	pathCountTokens     = "/messages/count_tokens"
	pathModerations     = "/moderations"
	pathRerank          = "/rerank"
	pathSpeech          = "/audio/speech"
	pathTranscriptions  = "/audio/transcriptions"
	pathTranslations    = "/audio/translations"
	pathImageGenerate   = "/images/generations"
	pathImageEdit       = "/images/edits"
	pathImageVariation  = "/images/variations"
)

// cohereRerankPath is the vendor's own rerank endpoint.
//
// It is joined RAW rather than through [upstream.endpoint], because that helper
// supplies "/v1" for a bare host and this route lives under /v2. Running it
// through the helper produces /v1/v2/rerank, which 404s.
const cohereRerankPath = "/v2/rerank"

// endpoint joins a provider's base URL with the path for one operation.
//
// Configured base URLs come in two shapes in the wild and both are correct: one
// already carries the version segment ("https://api.example.com/v1",
// "https://api.z.ai/api/coding/paas/v4") and one is a bare host
// ("https://api.anthropic.com"). A bare host gets "/v1" and a versioned one does
// not, which is the only rule that leaves every catalogued kind pointing at a
// real endpoint.
func (u *upstream) endpoint(suffix string) string {
	base := trimBase(u.baseURL)
	if strings.HasSuffix(base, suffix) {
		return base
	}
	if p, err := url.Parse(base); err == nil && (p.Path == "" || p.Path == "/") {
		base += "/v1"
	}
	return base + suffix
}

// endpointRaw joins a suffix that carries its own version segment, so a bare
// host gets nothing inserted.
func (u *upstream) endpointRaw(suffix string) string {
	base := trimBase(u.baseURL)
	if strings.HasSuffix(base, suffix) {
		return base
	}
	return base + suffix
}

// defaultUpstreamClient is the client used when the caller supplies none.
//
// Redirects are not followed: a redirect from a provider endpoint is a
// misconfiguration or a captive portal, and following one would resend the
// provider credential to whatever answered.
func defaultUpstreamClient() *http.Client {
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
			// A streaming relay must see bytes as they arrive, and dorang has
			// to read the terminal usage frame, so the response is never
			// compressed on the wire between here and the provider.
			DisableCompression: true,
		},
	}
}
