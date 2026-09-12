package backend

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

// AWS Signature Version 4, the request signer Amazon Bedrock requires.
//
// It is here rather than pulled from the AWS SDK for the reason DESIGN §1
// gives for every dependency dorang declines: the SDK is a large surface with
// its own configuration, credential-chain and endpoint-resolution machinery,
// and all of it to produce one Authorization header from a key, a region and
// a request. The algorithm is small and stable (it has not changed since
// 2012), so it is implemented against its specification and pinned to AWS's
// own published test vector rather than trusted to a transitive tree.
//
// What it does NOT do: the credential chain (only a static access key and
// secret, with an optional session token, which is what a gateway is
// configured with), and query-string signing (Bedrock signs headers). Both
// absences are deliberate and neither is silent — the caller passes exactly
// the three inputs, and there is nowhere for a fourth to be read from.

const sigV4Algorithm = "AWS4-HMAC-SHA256"

// signV4 signs req in place, adding X-Amz-Date, X-Amz-Content-Sha256, the
// Authorization header, and X-Amz-Security-Token when a session token is
// given. payload is the exact body bytes req will send.
//
// The signed header set is host, x-amz-date, x-amz-content-sha256 and, when
// present, x-amz-security-token and content-type — the headers whose value
// the server checks. Everything else is left unsigned so a proxy adding a
// header does not break the signature.
func signV4(req *http.Request, payload []byte, accessKeyID, secret, sessionToken, region, service string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")

	payloadHash := hex.EncodeToString(sha256sum(payload))
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if sessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", sessionToken)
	}

	// The canonical request. Host comes from the URL, not a header, because
	// net/http sends it from req.Host and a Host HEADER is ignored.
	host := req.URL.Host
	signed := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	values := map[string]string{
		"host":                 host,
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           amzDate,
	}
	if sessionToken != "" {
		signed = append(signed, "x-amz-security-token")
		values["x-amz-security-token"] = sessionToken
	}
	if ct := req.Header.Get("Content-Type"); ct != "" {
		signed = append(signed, "content-type")
		values["content-type"] = ct
	}
	sort.Strings(signed)

	var canonHeaders strings.Builder
	for _, h := range signed {
		canonHeaders.WriteString(h)
		canonHeaders.WriteByte(':')
		canonHeaders.WriteString(strings.TrimSpace(values[h]))
		canonHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(signed, ";")

	// The path is canonicalised from the DECODED path and written back to
	// RawPath, so the bytes SENT on the wire are exactly the bytes SIGNED:
	// a model id like "us.anthropic.claude-3:0" or an ARN carries characters
	// (`:`, `/` inside the id) that AWS's canonical form percent-encodes, and
	// a signature over one spelling with the other on the wire is a 403.
	canonPath := canonicalURI(req.URL.Path)
	req.URL.RawPath = canonPath

	canonicalReq := strings.Join([]string{
		req.Method,
		canonPath,
		canonicalQuery(req.URL.RawQuery),
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		sigV4Algorithm,
		amzDate,
		scope,
		hex.EncodeToString(sha256sum([]byte(canonicalReq))),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	req.Header.Set("Authorization", sigV4Algorithm+
		" Credential="+accessKeyID+"/"+scope+
		", SignedHeaders="+signedHeaders+
		", Signature="+signature)
}

// canonicalURI percent-encodes each path segment the way AWS's own signer
// does for every service but S3: the unreserved set (A-Za-z0-9-._~) is left
// alone and everything else — `:` and, inside a segment, `/` — is encoded,
// with the separators between segments preserved. It takes the DECODED path so
// the result does not depend on how the URL string was built.
func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = awsURIEncode(s, false)
	}
	return strings.Join(segs, "/")
}

// awsURIEncode percent-encodes s per RFC 3986's unreserved set, which is the
// set AWS SigV4 leaves unescaped. encodeSlash says whether `/` is data (a
// slash inside one path segment, as in a model ARN) or a separator to keep.
func awsURIEncode(s string, encodeSlash bool) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&0x0f])
		}
	}
	return b.String()
}

// canonicalQuery sorts the query by key and re-joins it. Bedrock's Converse
// routes carry no query, so this is usually empty; it is correct for the
// general case rather than special-cased to nothing.
func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

func sha256sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
