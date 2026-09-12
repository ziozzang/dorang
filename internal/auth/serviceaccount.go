package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// FormatGCPServiceAccount is a Google Cloud service-account key file, which is
// not a token store at all: it holds a private key, and an access token is
// MINTED from it (RFC 7523, a signed JWT exchanged for a bearer) rather than
// refreshed from a refresh token. It is fitted into the OAuth credential's
// shape deliberately, so that health, refresh-ahead, the 401 retry and the
// account header all apply to it unchanged:
//
//   - the "refresh token" is `private_key_id`, a non-secret identifier whose
//     only job is to be non-empty, so the credential knows it CAN refresh;
//   - the "account id" is `client_email`;
//   - there is no access-token field, so the store loads an empty token and
//     the first request mints one;
//   - the store is READ-ONLY: a refreshed token is never written back, because
//     the file is the operator's key, not dorang's cache.
const FormatGCPServiceAccount StoreFormat = "gcp-service-account"

// DefaultServiceAccountScope is the scope every Vertex AI call needs.
const DefaultServiceAccountScope = "https://www.googleapis.com/auth/cloud-platform"

// serviceAccountAssertionTTL is the JWT lifetime Google accepts (one hour is
// the maximum it allows).
const serviceAccountAssertionTTL = time.Hour

// ServiceAccountConfig configures [ServiceAccountRefresher].
type ServiceAccountConfig struct {
	// Path is the service-account JSON key file. Read on every refresh, so a
	// rotated key is picked up without a restart.
	Path string
	// Scope defaults to [DefaultServiceAccountScope].
	Scope string
	// TokenURL overrides the file's own token_uri; tests point it at a fake.
	TokenURL string
	Timeout  time.Duration
	Now      func() time.Time
	// Client is the HTTP client; nil takes the default.
	Client *http.Client
}

// ServiceAccountRefresher mints an access token from a service-account key.
type ServiceAccountRefresher struct {
	cfg    ServiceAccountConfig
	client *http.Client
}

// NewServiceAccountRefresher validates the config.
func NewServiceAccountRefresher(cfg ServiceAccountConfig) (*ServiceAccountRefresher, error) {
	if strings.TrimSpace(cfg.Path) == "" {
		return nil, errors.New("auth: a service-account credential needs the key file's path")
	}
	if cfg.Scope == "" {
		cfg.Scope = DefaultServiceAccountScope
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultRefreshTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	c := cfg.Client
	if c == nil {
		c = &http.Client{Timeout: cfg.Timeout}
	}
	return &ServiceAccountRefresher{cfg: cfg, client: c}, nil
}

// serviceAccountKey is the subset of the key file that minting needs. The
// private key never leaves this function's frame as a string: it is parsed
// and the parsed key is what signs.
type serviceAccountKey struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
	KeyID       string `json:"private_key_id"`
}

// Refresh mints a token. prev is read for its Refresh (the key id, kept so the
// credential stays refreshable) and for nothing else.
func (r *ServiceAccountRefresher) Refresh(ctx context.Context, prev Token) (Token, error) {
	raw, err := os.ReadFile(r.cfg.Path)
	if err != nil {
		return Token{}, fmt.Errorf("%w: %s: %v", ErrTokenStoreUnreadable, r.cfg.Path, errNoPath(err))
	}
	var k serviceAccountKey
	if err := json.Unmarshal(raw, &k); err != nil {
		return Token{}, fmt.Errorf("%w: service-account key file is not a JSON object", ErrTokenStoreMalformed)
	}
	if k.ClientEmail == "" || k.PrivateKey == "" {
		return Token{}, fmt.Errorf("%w: service-account key file has no client_email or private_key", ErrTokenStoreMalformed)
	}
	tokenURL := r.cfg.TokenURL
	if tokenURL == "" {
		tokenURL = k.TokenURI
	}
	if tokenURL == "" {
		tokenURL = "https://oauth2.googleapis.com/token"
	}
	key, err := parseRSAPrivateKey(k.PrivateKey)
	if err != nil {
		return Token{}, fmt.Errorf("%w: service-account private_key: %v", ErrTokenStoreMalformed, err)
	}
	now := r.cfg.Now()
	assertion, err := signServiceAccountJWT(key, k.KeyID, k.ClientEmail, r.cfg.Scope, tokenURL, now)
	if err != nil {
		return Token{}, fmt.Errorf("%w: could not sign the assertion", ErrRefreshEndpoint)
	}

	ctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", assertion)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, fmt.Errorf("%w %s: the request could not be built", ErrRefreshEndpoint, tokenURL)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return Token{}, fmt.Errorf("%w %s: %s", ErrRefreshEndpoint, tokenURL, transportClass(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRefreshResponse))
	if err != nil {
		return Token{}, fmt.Errorf("%w %s: the response could not be read", ErrRefreshEndpoint, tokenURL)
	}
	if resp.StatusCode != http.StatusOK {
		return Token{}, fmt.Errorf("%w %s: status %d", ErrRefreshEndpoint, tokenURL, resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return Token{}, fmt.Errorf("%w %s: the response carried no access token", ErrRefreshEndpoint, tokenURL)
	}
	ttl := time.Duration(out.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = serviceAccountAssertionTTL
	}
	return Token{
		Access:    out.AccessToken,
		Refresh:   prev.Refresh,
		ExpiresAt: now.Add(ttl),
		AccountID: k.ClientEmail,
	}, nil
}

func parseRSAPrivateKey(pemText string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return nil, errors.New("not PEM")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("not an RSA key")
		}
		return rk, nil
	}
	rk, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("not a PKCS#8 or PKCS#1 RSA key")
	}
	return rk, nil
}

// signServiceAccountJWT renders the RS256 assertion RFC 7523 §2.1 describes
// and Google's token endpoint accepts.
func signServiceAccountJWT(key *rsa.PrivateKey, kid, iss, scope, aud string, now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	hdr := map[string]string{"alg": "RS256", "typ": "JWT"}
	if kid != "" {
		hdr["kid"] = kid
	}
	h, _ := json.Marshal(hdr)
	claims, _ := json.Marshal(map[string]any{
		"iss":   iss,
		"scope": scope,
		"aud":   aud,
		"iat":   now.Unix(),
		"exp":   now.Add(serviceAccountAssertionTTL).Unix(),
	})
	signing := enc.EncodeToString(h) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}
