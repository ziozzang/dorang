package auth

import (
	"errors"
	"testing"
)

// The incumbent gates a model by TEAM — `access_via_team_ids` on the model row,
// on 26 of 56 real deployments — and dorang expresses the same relation from the
// other end: the team carries the model allow-list, and [Principal.Authorize]
// consults the key's, the user's and the team's.
//
// The mechanism is not new. What was missing is a test of the property a
// migrating operator is relying on, which is not "a team list refuses a model
// outside it" (auth_test.go has that) but the DIRECTION of the combination: a
// key-level list may narrow its team's and may never widen it. Without that
// half, issuing a key with `models: [everything]` under a restricted team would
// be the migration's hole, and nothing here would have said so.

// TestAKeyInheritsItsTeamsModelRestriction. The key states no list at all, which
// on its own subject means "unrestricted" — and it is still refused, because the
// team is a separate subject with its own veto.
func TestAKeyInheritsItsTeamsModelRestriction(t *testing.T) {
	p := &Principal{
		KeyID: "k", TeamID: "t",
		Key:  Limits{},
		Team: &Limits{Models: []string{"small"}},
	}
	if err := p.Authorize(Access{Now: testNow, Model: "small"}); err != nil {
		t.Fatalf("the team allows small and it was refused: %v", err)
	}
	err := p.Authorize(Access{Now: testNow, Model: "enormous"})
	if got := ReasonOf(err); got != ReasonModelNotAllowed {
		t.Fatalf("a key with no list of its own reached a model its team does not allow: %v", err)
	}
	// The refusal names the TEAM, not the key. An operator told "this key is not
	// allowed" would go and edit the key, which is the one change that cannot
	// fix it.
	if s := subjectOf(err); s != "team" {
		t.Errorf("the refusal blames %q; the restriction is the team's", s)
	}
}

// TestAKeyLevelListNarrowsAndNeverWidens is the property the whole shape rests
// on. Authorize is a serial veto — every subject must allow — so the effective
// set is the INTERSECTION, and no list written on a key can add a model its team
// withheld.
func TestAKeyLevelListNarrowsAndNeverWidens(t *testing.T) {
	narrows := &Principal{
		KeyID: "k", TeamID: "t",
		Key:  Limits{Models: []string{"small"}},
		Team: &Limits{Models: []string{"small", "medium"}},
	}
	if err := narrows.Authorize(Access{Now: testNow, Model: "small"}); err != nil {
		t.Fatalf("both subjects allow small and it was refused: %v", err)
	}
	err := narrows.Authorize(Access{Now: testNow, Model: "medium"})
	if got := ReasonOf(err); got != ReasonModelNotAllowed {
		t.Fatalf("the key's own narrower list did not apply: %v", err)
	}
	if s := subjectOf(err); s != "key" {
		t.Errorf("the refusal blames %q; the narrowing was the key's", s)
	}

	widens := &Principal{
		KeyID: "k", TeamID: "t",
		Key:  Limits{Models: []string{"small", "enormous"}},
		Team: &Limits{Models: []string{"small"}},
	}
	err = widens.Authorize(Access{Now: testNow, Model: "enormous"})
	if got := ReasonOf(err); got != ReasonModelNotAllowed {
		t.Fatalf("a key-level list WIDENED its team's restriction: %v", err)
	}

	// The strongest form of the same statement: `*` on the key is the widest
	// value the vocabulary has, and it still cannot reach past the team.
	star := &Principal{
		KeyID: "k", TeamID: "t",
		Key:  Limits{Models: []string{"*"}},
		Team: &Limits{Models: []string{"small"}},
	}
	if err := star.Authorize(Access{Now: testNow, Model: "small"}); err != nil {
		t.Fatalf("* on the key refused a model the team allows: %v", err)
	}
	if got := ReasonOf(star.Authorize(Access{Now: testNow, Model: "enormous"})); got != ReasonModelNotAllowed {
		t.Fatal("`*` on a key reached past its team's allow-list")
	}
}

// TestAUserRestrictionIsItsOwnSubject. A team gate and a user gate are separate
// vetoes, so a model has to clear both. It is the same shape as the budget and
// rate envelopes, which is the point: a migrating operator learns one rule.
func TestAUserRestrictionIsItsOwnSubject(t *testing.T) {
	p := &Principal{
		KeyID: "k", UserID: "u", TeamID: "t",
		Key:  Limits{},
		User: &Limits{Models: []string{"small", "medium"}},
		Team: &Limits{Models: []string{"medium", "enormous"}},
	}
	if err := p.Authorize(Access{Now: testNow, Model: "medium"}); err != nil {
		t.Fatalf("the model is in both lists and was refused: %v", err)
	}
	for _, m := range []string{"small", "enormous"} {
		if got := ReasonOf(p.Authorize(Access{Now: testNow, Model: m})); got != ReasonModelNotAllowed {
			t.Errorf("%q cleared only one of the two subjects and was allowed", m)
		}
	}
}

// TestAnEmptyTeamListRestrictsNothing is the caveat the importer has to state to
// an operator inverting `access_via_team_ids`, asserted rather than asserted-in-
// prose. The source says "only these teams may reach this model"; dorang says
// "this team may reach only these models" — so a team nobody gives a list to
// still reaches everything, and the migration is not complete until every OTHER
// team has one.
func TestAnEmptyTeamListRestrictsNothing(t *testing.T) {
	p := &Principal{KeyID: "k", TeamID: "t", Key: Limits{}, Team: &Limits{}}
	if err := p.Authorize(Access{Now: testNow, Model: "a-model-nobody-granted"}); err != nil {
		t.Fatalf("an empty allow-list restricted something: %v", err)
	}
}

// subjectOf names which of key/user/team refused, or "" for anything else.
func subjectOf(err error) string {
	var e *Error
	if !errors.As(err, &e) {
		return ""
	}
	return e.Subject
}
