package redact

import "testing"

// A secret is removed in both the plain and the percent-encoded spelling.
//
// The escaped case is not hypothetical: a provider that authenticates in a
// query parameter puts the key in every transport error, because net/http's
// *url.Error quotes the request URL — and it arrives escaped, so a plain-text
// search misses it entirely.
func TestTextRemovesBothSpellings(t *testing.T) {
	const secret = "sk-FAKE-0000000000000000/with+specials" // pragma: allowlist secret — fabricated

	cases := []struct {
		name string
		in   string
	}{
		{"plain", "Invalid API key: " + secret},
		{"escaped", `Get "https://api.example.com/v1?key=sk-FAKE-0000000000000000%2Fwith%2Bspecials": EOF`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := Text(c.in, secret)
			if Contains(out, secret) {
				t.Errorf("the secret survives scrubbing: %q", out)
			}
			if out == c.in {
				t.Errorf("nothing was replaced: %q", out)
			}
		})
	}
}

// A short "secret" is not matched, or every message becomes placeholders.
func TestTextIgnoresShortSecrets(t *testing.T) {
	in := "the model is a good one"
	if got := Text(in, "a", "good", ""); got != in {
		t.Errorf("a short secret mangled the message: %q", got)
	}
}

// Several secrets are removed at once — an OAuth credential presents a new
// token after each refresh and an error can be recorded about one that was
// just replaced.
func TestTextRemovesEverySecretGiven(t *testing.T) {
	a, b := "token-aaaaaaaaaaaaaaaa", "token-bbbbbbbbbbbbbbbb"
	out := Text("first "+a+" then "+b, a, b)
	if Contains(out, a, b) {
		t.Errorf("a secret survived: %q", out)
	}
}

// An empty input and no secrets are both no-ops.
func TestTextDegenerateCases(t *testing.T) {
	if got := Text("", "aaaaaaaaaaaa"); got != "" {
		t.Errorf("empty input became %q", got)
	}
	if got := Text("unchanged"); got != "unchanged" {
		t.Errorf("no secrets changed the text: %q", got)
	}
}
