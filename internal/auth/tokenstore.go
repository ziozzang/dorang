package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Where an OAuth token is read from (DESIGN §11.2b).
type TokenSource uint8

const (
	// SourceFile reads the vendor CLI's own JSON store. It is the only source
	// dorang writes back to, and the reason writes must be atomic.
	SourceFile TokenSource = iota
	// SourceExec runs a command that prints the token. The command owns the
	// token; dorang does not write it back.
	SourceExec
	// SourceEnv reads the token from an environment variable.
	SourceEnv
)

// String returns the configuration spelling.
func (s TokenSource) String() string {
	switch s {
	case SourceFile:
		return "file"
	case SourceExec:
		return "exec"
	case SourceEnv:
		return "env"
	}
	return "unknown"
}

// ParseTokenSource decodes a configured oauth.source value.
func ParseTokenSource(s string) (TokenSource, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "", "file":
		return SourceFile, nil
	case "exec":
		return SourceExec, nil
	case "env":
		return SourceEnv, nil
	}
	return 0, fmt.Errorf("auth: unknown oauth source %q (want file, exec or env)", s)
}

// Token store errors. None of them ever carries token material: a malformed
// store is reported by field name, never by field value, and a failing command
// is reported by exit status, never by its output.
var (
	// ErrTokenStoreMalformed reports a store dorang could read but not parse.
	ErrTokenStoreMalformed = errors.New("auth: oauth token store is malformed")
	// ErrTokenStoreUnreadable reports a store that could not be read at all.
	ErrTokenStoreUnreadable = errors.New("auth: oauth token store is unreadable")
	// ErrTokenStoreReadOnly reports a write to a source that has no write side.
	// It is informational: exec and env sources are read-only by construction,
	// because the command or the environment owns the token.
	ErrTokenStoreReadOnly = errors.New("auth: oauth token source is read-only")
)

// TokenFields names the keys a vendor's store uses, because there is no
// standard for them (DESIGN §11.2b's example configures account_id_field for
// exactly this reason).
//
// A name may be a dotted path into a nested object — "tokens.access_token" —
// because two of the three stores a real machine carries nest their tokens one
// level down. A path names a leaf; every key it does not name survives a write
// untouched, at whatever depth it sits.
type TokenFields struct {
	AccessToken  string // default "access_token"
	RefreshToken string // default "refresh_token"
	ExpiresAt    string // default "expires_at"
	AccountID    string // default "account_id"

	// ExpiresAtEncoding says how ExpiresAt is spelled where it is not a
	// timestamp. A store that carries no expiry at all can still name the JWT
	// it does carry, which is what makes refresh-ahead-of-expiry possible for
	// one (see [ExpiryJWTClaim]).
	ExpiresAtEncoding ExpiryEncoding
}

// ExpiryEncoding selects how the expiry field is read.
type ExpiryEncoding uint8

const (
	// ExpiryTimestamp is the default: RFC 3339, unix seconds or unix
	// milliseconds, told apart by shape.
	ExpiryTimestamp ExpiryEncoding = iota
	// ExpiryJWTClaim reads the `exp` claim of a JWT the store already carries.
	//
	// It exists because a store can hold a token and no expiry — and a token
	// with no expiry is never refreshed ahead of time (see [Token.NeedsRefresh]),
	// which leaves the whole of §11.2b resting on the 401 fallback. The claim is
	// read, never verified: dorang is not the audience and holds no key. It is
	// metadata about a token dorang already has.
	ExpiryJWTClaim
)

func (f TokenFields) withDefaults() TokenFields {
	if f.AccessToken == "" {
		f.AccessToken = "access_token"
	}
	if f.RefreshToken == "" {
		f.RefreshToken = "refresh_token"
	}
	if f.ExpiresAt == "" {
		f.ExpiresAt = "expires_at"
	}
	if f.AccountID == "" {
		f.AccountID = "account_id"
	}
	return f
}

// expiryStyle records how a store spelled its expiry, so that writing the file
// back does not change the encoding under the vendor CLI that shares it.
type expiryStyle uint8

const (
	expiryRFC3339 expiryStyle = iota // the default for a store dorang creates
	expiryUnixSeconds
	expiryUnixMillis
	// expiryFromJWT records that the expiry was READ OUT OF the access token
	// rather than out of a field of its own. Writing it back would add a key the
	// vendor's CLI never wrote, to a file the vendor's CLI reads.
	expiryFromJWT
)

// Token is one OAuth token set.
//
// It is secret material and redacts itself under every fmt verb (DESIGN
// §11.2b, §4.1): no token is ever logged, formatted into an error, or placed in
// a snapshot.
type Token struct {
	// Access is the bearer token sent upstream.
	Access string
	// Refresh is exchanged for the next Access. Some providers invalidate it
	// on use, which is why refresh is single-flight.
	Refresh string
	// ExpiresAt is when Access stops working. Zero means the store did not say,
	// which disables scheduled refresh for this credential — see
	// [Token.NeedsRefresh].
	ExpiresAt time.Time
	// AccountID is the vendor's account identifier, sent as a separate header
	// where the provider requires one. It is an identifier, not a secret.
	AccountID string

	// extra holds every other key the store carried, so that writing the file
	// back preserves fields dorang knows nothing about. The vendor's CLI reads
	// this file too.
	extra map[string]json.RawMessage
	// style is how the store spelled ExpiresAt.
	style expiryStyle
}

// String redacts.
func (t Token) String() string {
	exp := "none"
	if !t.ExpiresAt.IsZero() {
		exp = t.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("auth.Token{access:(redacted) refresh:(redacted) expires:%s account:%s}",
		exp, t.AccountID)
}

// GoString redacts %#v.
func (t Token) GoString() string { return t.String() }

// Format redacts every other verb.
func (t Token) Format(f fmt.State, verb rune) { writeRedacted(f, verb, t.String()) }

// Empty reports whether there is no usable access token.
func (t Token) Empty() bool { return t.Access == "" }

// Expired reports whether the access token has passed its expiry. A token whose
// store carried no expiry is never reported expired here — only a 401 can say.
func (t Token) Expired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && !now.Before(t.ExpiresAt)
}

// NeedsRefresh reports whether the token is inside its refresh margin.
//
// A token with no expiry returns false: refreshing ahead of an expiry that was
// never reported is not possible, and treating "unknown" as "now" would refresh
// in a loop against the auth server, which is the failure §11.2b names. Such a
// credential is renewed only by the 401 fallback.
func (t Token) NeedsRefresh(now time.Time, margin time.Duration) bool {
	if t.Empty() {
		return true
	}
	if t.ExpiresAt.IsZero() {
		return false
	}
	return !now.Before(t.ExpiresAt.Add(-margin))
}

// newerThan reports whether t supersedes o. It is how a token another process
// wrote to the shared store is recognised.
func (t Token) newerThan(o Token) bool {
	if t.Empty() {
		return false
	}
	if o.Empty() {
		return true
	}
	return t.Access != o.Access && !t.ExpiresAt.Before(o.ExpiresAt)
}

// merge fills in what a provider's refresh response did not return. Providers
// commonly omit the refresh token when it is unchanged, and omit the account
// id always; dropping either would break the next refresh or the account
// header.
func (t Token) merge(prev Token) Token {
	if t.Refresh == "" {
		t.Refresh = prev.Refresh
	}
	if t.AccountID == "" {
		t.AccountID = prev.AccountID
	}
	if t.extra == nil {
		t.extra = prev.extra
	}
	t.style = prev.style
	return t
}

// tokenStore is where one credential's token lives.
type tokenStore interface {
	// Load reads the current token.
	Load() (Token, error)
	// Save writes a refreshed token back. Read-only sources return
	// ErrTokenStoreReadOnly.
	Save(Token) error
	// Writable reports whether Save does anything.
	Writable() bool
	// Describe names the store for an error message. It never includes a token.
	Describe() string
}

// decodeToken parses a vendor store's JSON.
//
// Every parse failure is reported as ErrTokenStoreMalformed naming the field,
// never wrapping the decoder's own error: encoding/json and time.Parse both
// quote the offending input, and the offending input here can be a token.
func decodeToken(b []byte, f TokenFields) (Token, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return Token{}, fmt.Errorf("%w: not a JSON object", ErrTokenStoreMalformed)
	}
	t := Token{extra: map[string]json.RawMessage{}}
	// Only a field named at the TOP level is consumed out of extra. A dotted
	// path leaves its whole container behind, because the container holds keys
	// dorang does not own and a write has to put them back.
	consumed := map[string]bool{}
	for _, name := range []string{f.AccessToken, f.RefreshToken, f.ExpiresAt, f.AccountID} {
		if !strings.Contains(name, ".") {
			consumed[name] = true
		}
	}
	for k, v := range raw {
		if !consumed[k] {
			t.extra[k] = v
		}
	}
	var err error
	if t.Access, err = decodeString(raw, f.AccessToken); err != nil {
		return Token{}, err
	}
	if t.Refresh, err = decodeString(raw, f.RefreshToken); err != nil {
		return Token{}, err
	}
	if t.AccountID, err = decodeString(raw, f.AccountID); err != nil {
		return Token{}, err
	}
	if t.ExpiresAt, t.style, err = decodeExpiry(raw, f.ExpiresAt, f.ExpiresAtEncoding); err != nil {
		return Token{}, err
	}
	return t, nil
}

// lookupPath resolves a dotted field path to its leaf. A path that runs through
// something that is not an object is a miss rather than an error: the store is
// the vendor's, and a shape dorang did not expect is a field it does not have.
func lookupPath(raw map[string]json.RawMessage, path string) (json.RawMessage, bool) {
	segs := strings.Split(path, ".")
	cur := raw
	for i, s := range segs {
		v, ok := cur[s]
		if !ok {
			return nil, false
		}
		if i == len(segs)-1 {
			return v, true
		}
		var next map[string]json.RawMessage
		if err := json.Unmarshal(v, &next); err != nil {
			return nil, false
		}
		cur = next
	}
	return nil, false
}

func decodeString(raw map[string]json.RawMessage, key string) (string, error) {
	v, ok := lookupPath(raw, key)
	if !ok {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", fmt.Errorf("%w: field %q is not a string", ErrTokenStoreMalformed, key)
	}
	return s, nil
}

// decodeExpiry accepts the three spellings vendor stores actually use: RFC 3339,
// unix seconds and unix milliseconds. The magnitude tells seconds from
// milliseconds; the boundary is far outside any plausible token lifetime.
func decodeExpiry(raw map[string]json.RawMessage, key string, enc ExpiryEncoding) (time.Time, expiryStyle, error) {
	v, ok := lookupPath(raw, key)
	if !ok {
		return time.Time{}, expiryRFC3339, nil
	}
	if enc == ExpiryJWTClaim {
		var jwt string
		if err := json.Unmarshal(v, &jwt); err != nil {
			return time.Time{}, 0, fmt.Errorf(
				"%w: field %q is not a string", ErrTokenStoreMalformed, key)
		}
		// A token that is not a JWT, or one whose payload has no usable `exp`,
		// is not a malformed store: it is a store with no expiry, which
		// [Token.NeedsRefresh] already has an answer for. Reporting it as an
		// error would be reporting on the shape of a token, and the report
		// would be the one place a token could end up.
		ts, okJWT := jwtExpiry(jwt)
		if !okJWT {
			return time.Time{}, expiryFromJWT, nil
		}
		return ts, expiryFromJWT, nil
	}
	var s string
	if err := json.Unmarshal(v, &s); err == nil {
		if s == "" {
			return time.Time{}, expiryRFC3339, nil
		}
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}, 0, fmt.Errorf(
				"%w: field %q is not an RFC 3339 timestamp", ErrTokenStoreMalformed, key)
		}
		return ts.UTC(), expiryRFC3339, nil
	}
	var n json.Number
	if err := json.Unmarshal(v, &n); err != nil {
		return time.Time{}, 0, fmt.Errorf(
			"%w: field %q is neither a timestamp nor a number", ErrTokenStoreMalformed, key)
	}
	i, err := strconv.ParseInt(string(n), 10, 64)
	if err != nil {
		f, ferr := strconv.ParseFloat(string(n), 64)
		if ferr != nil {
			return time.Time{}, 0, fmt.Errorf(
				"%w: field %q is not a whole number of seconds", ErrTokenStoreMalformed, key)
		}
		i = int64(f)
	}
	if i == 0 {
		return time.Time{}, expiryUnixSeconds, nil
	}
	// 1e11 seconds is the year 5138; 1e11 milliseconds is 1973. Anything at or
	// above the boundary is milliseconds.
	if i >= 1e11 {
		return time.UnixMilli(i).UTC(), expiryUnixMillis, nil
	}
	return time.Unix(i, 0).UTC(), expiryUnixSeconds, nil
}

// encodeToken renders a token back into the store's own shape, preserving every
// key dorang does not own and the spelling of the expiry.
func encodeToken(t Token, f TokenFields) ([]byte, error) {
	out := make(map[string]any, len(t.extra)+4)
	for k, v := range t.extra {
		out[k] = v
	}
	setPath(out, f.AccessToken, t.Access)
	if t.Refresh != "" {
		setPath(out, f.RefreshToken, t.Refresh)
	} else {
		deletePath(out, f.RefreshToken)
	}
	if t.AccountID != "" {
		setPath(out, f.AccountID, t.AccountID)
	} else {
		deletePath(out, f.AccountID)
	}
	switch {
	case t.style == expiryFromJWT:
		// The expiry came out of the access token, which has just been written.
		// Adding a field of its own would add a key to a file the vendor's CLI
		// also reads, and the next reader would have two answers.
	case t.ExpiresAt.IsZero():
		deletePath(out, f.ExpiresAt)
	default:
		switch t.style {
		case expiryUnixSeconds:
			setPath(out, f.ExpiresAt, t.ExpiresAt.Unix())
		case expiryUnixMillis:
			setPath(out, f.ExpiresAt, t.ExpiresAt.UnixMilli())
		default:
			setPath(out, f.ExpiresAt, t.ExpiresAt.UTC().Format(time.RFC3339))
		}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		// Unreachable with the value shapes above, and deliberately not
		// wrapped: a marshal error quotes the value it choked on.
		return nil, fmt.Errorf("%w: cannot be re-encoded", ErrTokenStoreMalformed)
	}
	return append(b, '\n'), nil
}

// setPath writes a leaf at a dotted path, creating objects along the way and
// converting a container that arrived as raw JSON into something writable.
//
// Every sibling of the leaf survives, at every level. That is the requirement:
// the file belongs to the vendor's CLI, and a write that dropped a key it wrote
// would break it in a way the operator would not attribute to a gateway
// (DESIGN §11.2b).
func setPath(out map[string]any, path string, v any) {
	segs := strings.Split(path, ".")
	cur := out
	for _, s := range segs[:len(segs)-1] {
		cur = childMap(cur, s)
	}
	cur[segs[len(segs)-1]] = v
}

// deletePath removes a leaf. It creates nothing: a path that is not there is
// already in the state the caller wants.
func deletePath(out map[string]any, path string) {
	segs := strings.Split(path, ".")
	cur := out
	for _, s := range segs[:len(segs)-1] {
		next, ok := existingMap(cur, s)
		if !ok {
			return
		}
		cur = next
	}
	delete(cur, segs[len(segs)-1])
}

// childMap returns key's object, materialising it if it is raw or absent.
func childMap(m map[string]any, key string) map[string]any {
	if c, ok := existingMap(m, key); ok {
		return c
	}
	n := map[string]any{}
	m[key] = n
	return n
}

// existingMap returns key's object when there is one, converting a raw JSON
// object in place so that later writes to it are seen.
func existingMap(m map[string]any, key string) (map[string]any, bool) {
	switch c := m[key].(type) {
	case map[string]any:
		return c, true
	case json.RawMessage:
		var next map[string]json.RawMessage
		if err := json.Unmarshal(c, &next); err != nil {
			return nil, false
		}
		conv := make(map[string]any, len(next))
		for k, v := range next {
			conv[k] = v
		}
		m[key] = conv
		return conv, true
	}
	return nil, false
}

// jwtExpiry reads the `exp` claim of a JWT.
//
// The signature is NOT verified, and the function says so where it is used:
// dorang is not the audience, holds no key, and is reading metadata about a
// token it already has rather than deciding whether to trust one. Everything
// that can go wrong returns "no expiry" — nothing about the token, well-formed
// or not, becomes an error message, because an error message is exactly where a
// token must never appear (§4.1).
func jwtExpiry(s string) (time.Time, bool) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(string(claims.Exp), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	return time.Unix(n, 0).UTC(), true
}

// DefaultTokenFileMode is the mode a token store dorang creates is given. A
// credential file must not be group- or world-readable.
const DefaultTokenFileMode os.FileMode = 0o600

// fileStore is the vendor CLI's own JSON store.
type fileStore struct {
	path   string
	fields TokenFields
	// readOnly marks a file that is the operator's own key material rather
	// than a token cache — a service-account key — which a refresh must
	// never rewrite.
	readOnly bool
}

func (s *fileStore) Describe() string { return s.path }
func (s *fileStore) Writable() bool   { return !s.readOnly }

func (s *fileStore) Load() (Token, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		// os errors carry the path and the syscall, never file content.
		return Token{}, fmt.Errorf("%w: %s: %v", ErrTokenStoreUnreadable, s.path, errNoPath(err))
	}
	return decodeToken(b, s.fields)
}

// Save rewrites the store atomically.
//
// It re-reads first rather than writing the copy it loaded at startup: the
// vendor's CLI shares this file and may have added keys since. Losing one of
// them would break the CLI, and the operator would not suspect the gateway
// (DESIGN §11.2b).
func (s *fileStore) Save(t Token) error {
	mode := DefaultTokenFileMode
	if fi, err := os.Stat(s.path); err == nil {
		mode = fi.Mode().Perm()
		if cur, err := s.Load(); err == nil {
			t.extra = cur.extra
			if t.style == expiryRFC3339 && cur.style != expiryRFC3339 {
				t.style = cur.style
			}
		}
	}
	b, err := encodeToken(t, s.fields)
	if err != nil {
		return err
	}
	return atomicWrite(s.path, mode, func(w io.Writer) error {
		_, err := w.Write(b)
		return err
	})
}

// atomicWrite writes through a temporary file in the same directory, fsyncs it,
// and renames it over the target (DESIGN §11.2b).
//
// The rename is the only operation that touches the target, so a reader — the
// vendor's CLI, or dorang after a crash — sees either the whole old file or the
// whole new one and never a truncated one. Anything that fails before the
// rename leaves the original untouched and removes the temporary file.
//
// The original file's mode is preserved. A store dorang creates gets
// DefaultTokenFileMode rather than whatever the umask allows.
func atomicWrite(path string, mode os.FileMode, write func(io.Writer) error) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("auth: oauth token store %s: cannot create a temporary file: %v",
			path, errNoPath(err))
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	// Set the mode before the content exists, so the file is never readable by
	// anyone the original was not.
	if err = tmp.Chmod(mode); err != nil {
		return fmt.Errorf("auth: oauth token store %s: cannot set the file mode: %v",
			path, errNoPath(err))
	}
	if err = write(tmp); err != nil {
		return fmt.Errorf("auth: oauth token store %s: write failed", path)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("auth: oauth token store %s: fsync failed: %v", path, errNoPath(err))
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("auth: oauth token store %s: close failed: %v", path, errNoPath(err))
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("auth: oauth token store %s: rename failed: %v", path, errNoPath(err))
	}
	// Fsync the directory so the rename itself survives a power loss. A failure
	// here is not fatal: the rename has already happened and the file is
	// complete either way.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// errNoPath reduces a filesystem error to its underlying cause. The path is
// already in the message; what is dropped is any chance of the wrapper carrying
// something else.
func errNoPath(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// DefaultExecTimeout bounds a token command.
const DefaultExecTimeout = 10 * time.Second

// execStore runs a command that prints the token.
//
// The command's output is parsed as the store's JSON when it looks like an
// object, and otherwise taken as a bare access token — which is what a `print
// the token` helper usually emits. A bare token has no expiry, so such a
// credential is never refreshed ahead of time; only the 401 fallback renews it.
type execStore struct {
	command []string
	fields  TokenFields
	timeout time.Duration
}

func (s *execStore) Describe() string { return s.command[0] }
func (s *execStore) Writable() bool   { return false }

// Save is a no-op: the command owns the token.
func (s *execStore) Save(Token) error { return ErrTokenStoreReadOnly }

func (s *execStore) Load() (Token, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.command[0], s.command[1:]...)
	out, err := cmd.Output()
	if err != nil {
		// Neither stdout nor stderr appears here. A helper that prints the
		// token to stdout and then fails would otherwise put it in an error,
		// and a badly behaved one prints it to stderr as well.
		return Token{}, fmt.Errorf("%w: command %s failed (%s)",
			ErrTokenStoreUnreadable, s.command[0], exitStatus(err))
	}
	return parseTokenOutput(out, s.fields)
}

// exitStatus renders why a command failed without quoting anything it produced.
func exitStatus(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return "exit status " + strconv.Itoa(ee.ExitCode())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	return "not executed"
}

// parseTokenOutput accepts either the store's JSON object or a bare token.
func parseTokenOutput(b []byte, f TokenFields) (Token, error) {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return Token{}, fmt.Errorf("%w: produced no token", ErrTokenStoreMalformed)
	}
	if s[0] == '{' {
		return decodeToken([]byte(s), f)
	}
	if strings.ContainsAny(s, "\n\r") {
		return Token{}, fmt.Errorf("%w: produced more than one line", ErrTokenStoreMalformed)
	}
	return Token{Access: s, extra: map[string]json.RawMessage{}}, nil
}

// envStore reads the token from the environment.
type envStore struct {
	name   string
	fields TokenFields
}

func (s *envStore) Describe() string { return "$" + s.name }
func (s *envStore) Writable() bool   { return false }

// Save is a no-op: the environment owns the token.
func (s *envStore) Save(Token) error { return ErrTokenStoreReadOnly }

func (s *envStore) Load() (Token, error) {
	v, ok := os.LookupEnv(s.name)
	if !ok || strings.TrimSpace(v) == "" {
		return Token{}, fmt.Errorf("%w: $%s is not set", ErrTokenStoreUnreadable, s.name)
	}
	return parseTokenOutput([]byte(v), s.fields)
}

// expandHome resolves a leading ~/ the way configuration writes it
// (DESIGN §11.2b's example is `~/.some-vendor/auth.json`). A path that cannot
// be expanded is returned unchanged, so the failure is a missing file with a
// legible name rather than a startup error about the home directory.
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}
