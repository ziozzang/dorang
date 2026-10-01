package systemone

import (
	"errors"
	"strings"
	"testing"
)

// TestTheDocumentedExamplesAreAccepted: every request example in the two
// contracts' own documentation must pass the gateway unchanged —
// https://docs.typesafe.ai/api.md and https://docs.ollama.com/api/systemone.md,
// as fetched 2026-10-01.
func TestTheDocumentedExamplesAreAccepted(t *testing.T) {
	for name, body := range map[string]string{
		"typesafe noul": `{"state":"Help! My payouts have been failing for 3 days.","model":"jev-latest",
			"questions":{"is_urgent":{"type":"noul","instructions":"Does this convey urgency?"}}}`,
		"typesafe noul with criteria": `{"state":"Help!","model":"jev-latest","questions":{"is_urgent":{
			"type":"noul","instructions":"Does this convey urgency?",
			"criteria":{"true":"Explicitly time-sensitive","false":"No urgency expressed"}}}}`,
		"typesafe choice": `{"state":"Help!","model":"jev-latest","questions":{"department":{
			"type":"choice","instructions":"Which team should handle this?",
			"criteria":{"billing":"Payments, invoicing, refunds","technical":"Bugs, outages, integrations",
			"sales":"Pricing, upgrades, new accounts"}}}}`,
		"typesafe score": `{"state":"Help!","model":"jev-latest","questions":{"frustration":{
			"type":"score","instructions":"How frustrated is the customer?",
			"criteria":["Calm","Frustrated","Very angry"]}}}`,
		"typesafe structured instructions": `{"state":{"resume":"x"},"model":"jev-latest","questions":{"dup":{
			"type":"noul","instructions":{"potential_duplicate":{"name":"John Smith"},
			"question":"Is the resume for the same person as ` + "`potential_duplicate`" + `?"}}}}`,
		"ollama choice": `{"model":"nimble","state":"Our checkout has returned 500 errors since 9am.",
			"questions":{"label":{"type":"choice","instructions":"Which label fits this ticket?",
			"criteria":{"billing":"Payments and refunds","bug":"Software errors","account":"Login and account access"}}}}`,
		"ollama keep_alive and a null option": `{"model":"nimble","state":"s","keep_alive":"5m",
			"questions":{"q":{"type":"choice","instructions":"i","criteria":{"a":null,"b":"B"}}}}`,
	} {
		if _, err := DecodeRequest([]byte(body)); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

// TestMalformedRequestsAreRefusedNamingTheField: the gateway's refusal is a
// validation error the client can act on — which field, and what it must be.
func TestMalformedRequestsAreRefusedNamingTheField(t *testing.T) {
	choice := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`"o` + strings.Repeat("x", i) + `":"d"`)
		}
		return `{"model":"m","state":"s","questions":{"q":{"type":"choice","instructions":"i","criteria":{` + b.String() + `}}}}`
	}
	score := func(n int) string {
		levels := strings.TrimSuffix(strings.Repeat(`"l",`, n), ",")
		return `{"model":"m","state":"s","questions":{"q":{"type":"score","instructions":"i","criteria":[` + levels + `]}}}`
	}
	for _, c := range []struct {
		name, body, param string
	}{
		{"not an object", `[]`, "body"},
		{"no model", `{"state":"s","questions":{"q":{"type":"noul","instructions":"i"}}}`, "model"},
		{"blank model", `{"model":"  ","state":"s","questions":{"q":{"type":"noul","instructions":"i"}}}`, "model"},
		{"no state", `{"model":"m","questions":{"q":{"type":"noul","instructions":"i"}}}`, "state"},
		{"numeric state", `{"model":"m","state":42,"questions":{"q":{"type":"noul","instructions":"i"}}}`, "state"},
		{"no questions", `{"model":"m","state":"s"}`, "questions"},
		{"empty questions", `{"model":"m","state":"s","questions":{}}`, "questions"},
		{"unknown type", `{"model":"m","state":"s","questions":{"q":{"type":"rank","instructions":"i"}}}`, "questions.q.type"},
		{"no instructions", `{"model":"m","state":"s","questions":{"q":{"type":"noul"}}}`, "questions.q.instructions"},
		{"noul extra key", `{"model":"m","state":"s","questions":{"q":{"type":"noul","instructions":"i","criteria":{"maybe":"x"}}}}`, "questions.q.criteria"},
		{"choice without criteria", `{"model":"m","state":"s","questions":{"q":{"type":"choice","instructions":"i"}}}`, "questions.q.criteria"},
		{"choice of one", choice(1), "questions.q.criteria"},
		{"choice of 256", choice(256), "questions.q.criteria"},
		{"score as object", `{"model":"m","state":"s","questions":{"q":{"type":"score","instructions":"i","criteria":{"a":"b"}}}}`, "questions.q.criteria"},
		{"score of one", score(1), "questions.q.criteria"},
		{"score of 27", score(27), "questions.q.criteria"},
	} {
		_, err := DecodeRequest([]byte(c.body))
		var e *Error
		if !errors.As(err, &e) {
			t.Errorf("%s: want a validation error, got %v", c.name, err)
			continue
		}
		if e.Param != c.param {
			t.Errorf("%s: param %q, want %q (%s)", c.name, e.Param, c.param, e.Message)
		}
	}
	// The limits at the edges are admitted: the gateway takes what either
	// contract takes.
	for name, body := range map[string]string{"choice of 255": choice(255), "score of 26": score(26), "choice of 2": choice(2)} {
		if _, err := DecodeRequest([]byte(body)); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

func TestDecodeRecordsWhatBackendsCheck(t *testing.T) {
	req, err := DecodeRequest([]byte(`{"model":"m","state":"s","questions":{
		"b":{"type":"score","instructions":"i","criteria":["x","y","z"]},
		"a":{"type":"choice","instructions":"i","criteria":{"p":null,"q":"Q"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Questions) != 2 || req.Questions[0].ID != "a" || req.Questions[0].Options != 2 ||
		req.Questions[1].ID != "b" || req.Questions[1].Options != 3 {
		t.Errorf("questions = %+v, want a(2 options), b(3 levels) in id order", req.Questions)
	}
	if !req.StringCriteriaOnly {
		t.Error("string and null criteria are string-only")
	}
	req, err = DecodeRequest([]byte(`{"model":"m","state":"s","questions":{
		"a":{"type":"choice","instructions":"i","criteria":{"p":{"detail":"x"},"q":"Q"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.StringCriteriaOnly {
		t.Error("an object description must clear StringCriteriaOnly: Ollama refuses it")
	}
}

func TestCheckResponse(t *testing.T) {
	u, ok, err := CheckResponse([]byte(`{"model":"jev-1.13.0","answers":{"q":{"type":"noul","noul":0.95}},
		"usage":{"input_tokens":296,"output_tokens":20}}`))
	if err != nil || !ok || u.InputTokens != 296 || u.OutputTokens != 20 {
		t.Errorf("usage %+v ok=%v err=%v", u, ok, err)
	}
	if _, _, err := CheckResponse([]byte(`{"error":"model not found"}`)); !errors.Is(err, ErrNotAResponse) {
		t.Errorf("an error envelope on a 200 must not pass as an answer: %v", err)
	}
}
