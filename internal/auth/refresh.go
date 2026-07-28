package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HTTPRefresher is the RFC 6749 §6 refresh_token grant.
//
// [Refresher]'s documentation says implementations live with their providers,
// for a reason that holds: this package cannot scrub a token it has never seen,
// so a provider's own refresher is outside the guarantee. This one is not a
// provider's — it is the standard exchange, written here, and therefore held to
// this package's rule directly: no response body, no response header and no
// decoded field ever reaches an error it returns. What an error carries is the
// endpoint, the HTTP status, and nothing else.
//
// It is configuration-driven rather than a table of vendors. The endpoint and
// the client id of a public CLI client are published values, not secrets, and
// baking a third party's client registration into this repository would make
// dorang's build the thing that has to change when the third party rotates it.

// RefreshEncoding is how the exchange's parameters are sent.
type RefreshEncoding uint8

const (
	// EncodingForm is application/x-www-form-urlencoded — RFC 6749's own
	// spelling, and what the Google and OpenAI token endpoints take.
	EncodingForm RefreshEncoding = iota
	// EncodingJSON is a JSON body, which some vendors require instead.
	EncodingJSON
)

// String returns the configuration spelling.
func (e RefreshEncoding) String() string {
	if e == EncodingJSON {
		return "json"
	}
	return "form"
}

// ParseRefreshEncoding decodes a configured oauth.refresh.encoding value.
func ParseRefreshEncoding(s string) (RefreshEncoding, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "", "form":
		return EncodingForm, nil
	case "json":
		return EncodingJSON, nil
	}
	return 0, fmt.Errorf("auth: unknown oauth refresh encoding %q (want form or json)", s)
}

// DefaultRefreshTimeout bounds one exchange. It is generous: the exchange runs
// off the request path, and a token endpoint that is slow is still better than a
// credential that steps aside.
const DefaultRefreshTimeout = 30 * time.Second

// maxRefreshResponse bounds what is read from a token endpoint. A response
// larger than this is not a token set, and reading it into memory is how a
// misconfigured endpoint — an HTML login page, a proxy error — becomes a memory
// problem.
const maxRefreshResponse = 1 << 20

// ErrRefreshEndpoint reports a token endpoint that answered something other than
// a token set.
var ErrRefreshEndpoint = errors.New("auth: oauth token endpoint")

// RefreshConfig configures an [HTTPRefresher].
type RefreshConfig struct {
	// TokenURL is the endpoint. Required, and it must be https: a refresh token
	// posted over http is a credential handed to the network.
	TokenURL string
	// ClientID identifies the OAuth client. Required by every endpoint that
	// serves a public client.
	ClientID string
	// ClientSecret is sent where the client is confidential. Empty is the
	// public-client (PKCE) case, which is what a CLI usually registers.
	ClientSecret string
	// Scope is sent when non-empty. RFC 6749 makes it optional on a refresh and
	// most endpoints ignore it.
	Scope string
	// Encoding selects the request body's spelling.
	Encoding RefreshEncoding
	// Timeout bounds one exchange. Zero uses [DefaultRefreshTimeout].
	Timeout time.Duration
	// Client issues the request. Nil uses a client built here with Timeout.
	Client *http.Client
	// Now supplies the clock, so that expires_in becomes a deterministic
	// expiry in a test.
	Now func() time.Time
}

// HTTPRefresher exchanges a refresh token for its successor.
type HTTPRefresher struct {
	cfg    RefreshConfig
	client *http.Client
}

// NewHTTPRefresher builds a refresher.
func NewHTTPRefresher(cfg RefreshConfig) (*HTTPRefresher, error) {
	if strings.TrimSpace(cfg.TokenURL) == "" {
		return nil, errors.New("auth: an oauth refresher needs a token_url")
	}
	u, err := url.Parse(cfg.TokenURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("auth: oauth token_url %q is not a URL", cfg.TokenURL)
	}
	// http is refused rather than warned about. The body of this request is a
	// refresh token, and a warning that scrolls past is not a control. Loopback
	// is allowed because that is where a test's fake endpoint lives, and a
	// listener on the loopback interface is not on a network.
	if u.Scheme != "https" && !isLoopbackURL(u) {
		return nil, fmt.Errorf(
			"auth: oauth token_url %q is not https: the body of a refresh is a refresh token, "+
				"and posting one in clear hands the account to anything on the path", cfg.TokenURL)
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, errors.New("auth: an oauth refresher needs a client_id")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultRefreshTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	r := &HTTPRefresher{cfg: cfg, client: cfg.Client}
	if r.client == nil {
		r.client = &http.Client{Timeout: cfg.Timeout}
	}
	return r, nil
}

// isLoopbackURL reports whether the host is the loopback interface.
func isLoopbackURL(u *url.URL) bool {
	h := u.Hostname()
	return h == "127.0.0.1" || h == "::1" || h == "localhost"
}

// String redacts: this type holds a client secret.
func (r *HTTPRefresher) String() string {
	return fmt.Sprintf("auth.HTTPRefresher{token_url:%s client_id:%s secret:(redacted)}",
		r.cfg.TokenURL, r.cfg.ClientID)
}

// GoString redacts %#v.
func (r *HTTPRefresher) GoString() string { return r.String() }

// Format redacts every other verb.
func (r *HTTPRefresher) Format(f fmt.State, verb rune) { writeRedacted(f, verb, r.String()) }

// Refresh implements [Refresher].
func (r *HTTPRefresher) Refresh(ctx context.Context, prev Token) (Token, error) {
	if prev.Refresh == "" {
		return Token{}, ErrNoRefreshToken
	}
	ctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	body, contentType := r.body(prev.Refresh)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.TokenURL, strings.NewReader(body))
	if err != nil {
		// Not wrapped: a request-construction error quotes the URL, and while
		// the URL is not a secret, nothing here needs the decorated text.
		return Token{}, fmt.Errorf("%w %s: the request could not be built",
			ErrRefreshEndpoint, r.cfg.TokenURL)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		// The transport error is reduced to a class. *url.Error quotes the
		// request URL, which is safe, but the chain below it is not this
		// package's to vouch for and the refresh token is in the body.
		return Token{}, fmt.Errorf("%w %s: %s", ErrRefreshEndpoint, r.cfg.TokenURL, transportClass(err))
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRefreshResponse))
	if err != nil {
		return Token{}, fmt.Errorf("%w %s: the response could not be read",
			ErrRefreshEndpoint, r.cfg.TokenURL)
	}
	if resp.StatusCode != http.StatusOK {
		// The status and nothing else. A token endpoint answering 400 puts
		// {"error":"invalid_grant"} in the body, and several put the token it
		// refused in there with it.
		return Token{}, fmt.Errorf("%w %s: status %d", ErrRefreshEndpoint, r.cfg.TokenURL, resp.StatusCode)
	}

	tok, err := r.decode(raw)
	if err != nil {
		return Token{}, err
	}
	if tok.Empty() {
		return Token{}, fmt.Errorf("%w %s: the response carried no access token",
			ErrRefreshEndpoint, r.cfg.TokenURL)
	}
	return tok, nil
}

// body renders the exchange's parameters.
func (r *HTTPRefresher) body(refresh string) (body, contentType string) {
	if r.cfg.Encoding == EncodingJSON {
		m := map[string]string{
			"grant_type":    "refresh_token",
			"refresh_token": refresh,
			"client_id":     r.cfg.ClientID,
		}
		if r.cfg.ClientSecret != "" {
			m["client_secret"] = r.cfg.ClientSecret
		}
		if r.cfg.Scope != "" {
			m["scope"] = r.cfg.Scope
		}
		b, err := json.Marshal(m)
		if err != nil {
			// Unreachable for a map of strings; a body of "{}" fails the
			// exchange rather than panicking with a token in the stack.
			return "{}", "application/json"
		}
		return string(b), "application/json"
	}
	v := url.Values{}
	v.Set("grant_type", "refresh_token")
	v.Set("refresh_token", refresh)
	v.Set("client_id", r.cfg.ClientID)
	if r.cfg.ClientSecret != "" {
		v.Set("client_secret", r.cfg.ClientSecret)
	}
	if r.cfg.Scope != "" {
		v.Set("scope", r.cfg.Scope)
	}
	return v.Encode(), "application/x-www-form-urlencoded"
}

// tokenResponse is RFC 6749 §5.1, plus the two expiry spellings endpoints
// actually use.
type tokenResponse struct {
	AccessToken  string      `json:"access_token"`
	RefreshToken string      `json:"refresh_token"`
	ExpiresIn    json.Number `json:"expires_in"`
	ExpiresAt    json.Number `json:"expires_at"`
	AccountID    string      `json:"account_id"`
}

// decode reads the token set. Every failure names the endpoint and the shape,
// never the content: this is the one function in the exchange that has the token
// in a variable, so it is the one that must not put a variable into a message.
func (r *HTTPRefresher) decode(raw []byte) (Token, error) {
	var tr tokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return Token{}, fmt.Errorf("%w %s: the response is not a JSON object",
			ErrRefreshEndpoint, r.cfg.TokenURL)
	}
	t := Token{
		Access:    tr.AccessToken,
		Refresh:   tr.RefreshToken,
		AccountID: tr.AccountID,
	}
	switch {
	case tr.ExpiresIn != "":
		if n, err := strconv.ParseInt(string(tr.ExpiresIn), 10, 64); err == nil && n > 0 {
			t.ExpiresAt = r.cfg.Now().Add(time.Duration(n) * time.Second).UTC()
		}
	case tr.ExpiresAt != "":
		if n, err := strconv.ParseInt(string(tr.ExpiresAt), 10, 64); err == nil && n > 0 {
			if n >= 1e11 {
				t.ExpiresAt = time.UnixMilli(n).UTC()
			} else {
				t.ExpiresAt = time.Unix(n, 0).UTC()
			}
		}
	}
	return t, nil
}

// transportClass reduces a transport failure to a class, so that nothing the
// chain below net/http decided to include travels with it.
func transportClass(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	return "not reached"
}
