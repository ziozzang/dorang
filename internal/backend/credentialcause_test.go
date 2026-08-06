package backend

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/auth"
)

// The three categories dorang determines itself are named; nothing else is.
//
// "not usable" alone cost an afternoon on a real deployment: the operator's
// 0600 credential file was mounted into a distroless image running as another
// user, and unreadable, expired and malformed all produced the same sentence.
// They are three different actions.
//
// The line this test draws is DESIGN §11.2b's: a refresher's message is never
// wrapped, because it is provider code and its error can carry the token it
// failed to exchange. These three sentinels are raised by internal/auth BEFORE
// any provider is contacted, so naming which one fired leaks nothing.
func TestACredentialFailureNamesTheCategoryDorangDetermined(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		want  string
	}{
		{"unreadable", fmt.Errorf("%w: /auth/auth.json: permission denied", auth.ErrTokenStoreUnreadable), "cannot be read"},
		{"malformed", fmt.Errorf("%w: not a JSON object", auth.ErrTokenStoreMalformed), "shape `format:` declares"},
		{"read-only", fmt.Errorf("%w: env source", auth.ErrTokenStoreReadOnly), "cannot be written"},
	} {
		got := credentialError("cred-1", tc.cause).Error()
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: message does not say which category fired.\n  got:  %s\n  want to contain: %s",
				tc.name, got, tc.want)
		}
		if !strings.Contains(got, "cred-1") {
			t.Errorf("%s: the credential id is missing, so the operator cannot tell which one", tc.name)
		}
	}
}

// A provider's own error is still never quoted.
//
// This is the property the categories must not cost. An exchange failure comes
// back from provider code and can carry the token it failed to exchange, so its
// text stays out of a client-visible message however useful it looks.
func TestAProviderErrorIsStillNotQuoted(t *testing.T) {
	leaked := "refresh failed: invalid_grant for token sk-ABCDEF0123456789"
	got := credentialError("cred-1", errors.New(leaked)).Error()

	if strings.Contains(got, "sk-ABCDEF") || strings.Contains(got, "invalid_grant") {
		t.Fatalf("a provider error reached the client:\n  %s", got)
	}
	// And an unrecognised cause adds nothing at all rather than guessing.
	if strings.Contains(got, "cannot be read") || strings.Contains(got, "malformed") {
		t.Errorf("an unrecognised cause was reported as one of the known categories:\n  %s", got)
	}
	if !strings.Contains(got, "cred-1") {
		t.Errorf("the id went missing: %s", got)
	}
}

// A nil cause is the old message, unchanged.
func TestNoCauseIsTheBareMessage(t *testing.T) {
	got := credentialError("cred-1", nil).Error()
	if strings.Contains(got, "—") {
		t.Errorf("a category was appended for a cause nobody supplied: %s", got)
	}
}
