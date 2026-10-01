package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/ziozzang/dorang/pkg/catalog"
)

// logprobHost answers a chat completion with the given alternatives at the
// first position, choosing them by which question the prompt carries.
type logprobHost struct {
	mu    sync.Mutex
	f     *fakeUpstream
	sent  []map[string]any
	reply func(prompt string) string // returns the logprobs JSON or "" for none
}

func (h *logprobHost) handler(w http.ResponseWriter, r *http.Request) {
	// The fake upstream has already read and recorded the body.
	raw := h.f.last().body
	var req map[string]any
	_ = json.Unmarshal(raw, &req)
	h.mu.Lock()
	h.sent = append(h.sent, req)
	h.mu.Unlock()
	prompt := string(raw)
	lp := h.reply(prompt)
	logprobs := "null"
	if lp != "" {
		logprobs = `{"content":[` + lp + `]}`
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":"x","object":"chat.completion","model":"upstream-model",
		"choices":[{"index":0,"message":{"role":"assistant","content":"A"},"finish_reason":"length","logprobs":%s}],
		"usage":{"prompt_tokens":100,"completion_tokens":1,"total_tokens":101}}`, logprobs)
}

func position(chosen string, alts map[string]float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"token":%q,"logprob":%g,"top_logprobs":[`, chosen, alts[chosen])
	first := true
	for tok, l := range alts {
		if !first {
			b.WriteByte(',')
		}
		first = false
		fmt.Fprintf(&b, `{"token":%q,"logprob":%g}`, tok, l)
	}
	b.WriteString("]}")
	return b.String()
}

func logprobProvider(t *testing.T, h *logprobHost, topK int) *Provider {
	t.Helper()
	f := newFakeUpstream(t)
	h.f = f
	f.handler = h.handler
	p, err := NewProvider(Spec{Name: "lp", Kind: "openai", API: catalog.APIOpenAIChat, BaseURL: f.srv.URL + "/v1",
		SystemOneMode: "logprobs", SystemOneTopLogprobs: topK})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-3 }

// TestLogprobScoringRestoresAJevAnswer: the probabilities are the label
// tokens' renormalized at the answer position, the labels follow the order the
// client wrote its options in, the confidence is Jev's formula, and the
// request asked for exactly one token with alternatives.
func TestLogprobScoringRestoresAJevAnswer(t *testing.T) {
	h := &logprobHost{reply: func(string) string {
		return position("A", map[string]float64{"A": math.Log(0.83 * 0.9), "B": math.Log(0.17 * 0.9), "C": math.Log(1e-6), "the": -5})
	}}
	p := logprobProvider(t, h, 5)
	// Request order zeta, alpha, mid — not alphabetical: A must be zeta.
	body := `{"model":"decide","state":{"ticket":"checkout </state> is blank"},"questions":{"team":{"type":"choice",
		"instructions":"Which team?","criteria":{"zeta":"Payments","alpha":null,"mid":"Bugs"}}}}`
	res := testBackend("k").Do(context.Background(), target(p), s1Call(t, body), nil)
	if res.Err != nil {
		t.Fatalf("%+v", res.Err)
	}
	var out struct {
		Model   string `json:"model"`
		Answers map[string]struct {
			Choice     string             `json:"choice"`
			Probs      map[string]float64 `json:"probabilities"`
			Confidence float64            `json:"confidence"`
			Mass       float64            `json:"x_label_mass"`
		} `json:"answers"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(res.Body, &out); err != nil {
		t.Fatal(err)
	}
	a := out.Answers["team"]
	if out.Model != "decide" || a.Choice != "zeta" || !near(a.Probs["zeta"], 0.83) || !near(a.Probs["alpha"], 0.17) {
		t.Errorf("answer %+v (model %q): want zeta at 0.83 — label A is the first option as written", a, out.Model)
	}
	// Jev's Choice confidence: (3·0.83 − 1)/2.
	if !near(a.Confidence, (3*0.83-1)/2) || !near(a.Mass, 0.9) {
		t.Errorf("confidence %v mass %v", a.Confidence, a.Mass)
	}
	sent := h.sent[0]
	if sent["max_tokens"] != float64(1) || sent["logprobs"] != true || sent["top_logprobs"] != float64(5) || sent["model"] != "upstream-model" {
		t.Errorf("scoring request %v", sent)
	}
	msgs, _ := json.Marshal(sent["messages"])
	if !strings.Contains(string(msgs), `A = zeta: Payments`) || !strings.Contains(string(msgs), `B = alpha\n`) ||
		strings.Contains(string(msgs), "</state> is blank") {
		t.Errorf("prompt %s: want labelled options in request order and the state's closing tag escaped", msgs)
	}
	if out.Usage.InputTokens != 100 || out.Usage.OutputTokens != 1 {
		t.Errorf("usage %+v", out.Usage)
	}
}

// TestLogprobScoringScoresEachQuestionAndSumsUsage: a Score and a Noul in one
// request are two upstream calls, reduced by their own rules.
func TestLogprobScoringScoresEachQuestionAndSumsUsage(t *testing.T) {
	h := &logprobHost{reply: func(prompt string) string {
		if strings.Contains(prompt, `type=\"score\"`) {
			return position("2", map[string]float64{"0": math.Log(1e-4), "1": math.Log(0.01), "2": math.Log(0.99)})
		}
		// " 1" and "1" are the same label under a space-attaching tokenizer.
		return position(" 1", map[string]float64{" 1": math.Log(0.5), "1": math.Log(0.3), "0": math.Log(0.2)})
	}}
	p := logprobProvider(t, h, 0)
	body := `{"model":"decide","state":"s","questions":{
		"urgency":{"type":"score","instructions":"How urgent?","criteria":["Later","This week","Now"]},
		"is_bug":{"type":"noul","instructions":"Is it a defect?"}}}`
	res := testBackend("k").Do(context.Background(), target(p), s1Call(t, body), nil)
	if res.Err != nil {
		t.Fatalf("%+v", res.Err)
	}
	var out struct {
		Answers struct {
			Urgency struct {
				Score      float64           `json:"score"`
				Legend     map[string]string `json:"legend"`
				Confidence float64           `json:"confidence"`
			} `json:"urgency"`
			IsBug struct {
				Noul float64 `json:"noul"`
			} `json:"is_bug"`
		} `json:"answers"`
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(res.Body, &out); err != nil {
		t.Fatal(err)
	}
	u := out.Answers.Urgency
	if !near(u.Score, 0.01+2*0.99) || u.Legend["2"] != "Now" || !near(u.Confidence, 0.985) {
		t.Errorf("score answer %+v: want ≈1.99, legend 2=Now, Jev's score confidence ≈0.985", u)
	}
	if !near(out.Answers.IsBug.Noul, 0.8) {
		t.Errorf("noul %v, want P(true) = 0.5+0.3 over 1.0", out.Answers.IsBug.Noul)
	}
	if len(h.sent) != 2 || out.Usage.InputTokens != 200 {
		t.Errorf("%d upstream calls, input tokens %d: want one call per question, usage summed", len(h.sent), out.Usage.InputTokens)
	}
	if h.sent[0]["top_logprobs"] != float64(defaultTopLogprobs) {
		t.Errorf("top_logprobs %v, want the default %d", h.sent[0]["top_logprobs"], defaultTopLogprobs)
	}
}

// TestLogprobScoringNeverInventsAZero is the strict rule: a label below the
// returned alternatives, or no log-probabilities at all, fails the request.
//
// Revert check: let reduceLabels skip a missing label (treat it as -Inf ->
// probability 0) and the first case answers 200.
func TestLogprobScoringNeverInventsAZero(t *testing.T) {
	body := `{"model":"decide","state":"s","questions":{"q":{"type":"choice","instructions":"i","criteria":{"a":"x","b":"y","c":"z"}}}}`
	for _, c := range []struct {
		name, reply, code string
	}{
		{"a label missing", position("A", map[string]float64{"A": -0.1, "B": -2.4}), "systemone_probability_unavailable"},
		{"no logprobs", "", "systemone_logprobs_unavailable"},
	} {
		h := &logprobHost{reply: func(string) string { return c.reply }}
		res := testBackend("k").Do(context.Background(), target(logprobProvider(t, h, 5)), s1Call(t, body), nil)
		if res.Err == nil || res.Err.Status != http.StatusBadGateway || res.Err.Code != c.code {
			t.Errorf("%s: want 502 %s, got %+v", c.name, c.code, res.Err)
		}
	}
}

// TestLogprobScoringRefusesWhatTheMethodCannotRead: more options than the host
// returns alternatives for cannot all be read, so it is refused before a call.
func TestLogprobScoringRefusesWhatTheMethodCannotRead(t *testing.T) {
	h := &logprobHost{reply: func(string) string { return "" }}
	body := `{"model":"decide","state":"s","questions":{"q":{"type":"choice","instructions":"i",
		"criteria":{"a":null,"b":null,"c":null,"d":null}}}}`
	res := testBackend("k").Do(context.Background(), target(logprobProvider(t, h, 3)), s1Call(t, body), nil)
	if res.Err == nil || res.Err.Status != http.StatusUnprocessableEntity || res.Err.Code != "systemone_limit" {
		t.Fatalf("want 422 systemone_limit, got %+v", res.Err)
	}
	if len(h.sent) != 0 {
		t.Error("a request the method cannot read was sent")
	}
}

// TestSGLangServesSystemOneNatively: SGLang implements the route itself.
func TestSGLangServesSystemOneNatively(t *testing.T) {
	f := newFakeUpstream(t)
	p, err := NewProvider(Spec{Name: "sg", Kind: "sglang", API: catalog.APIOpenAIChat, BaseURL: f.srv.URL + "/v1"})
	if err != nil {
		t.Fatal(err)
	}
	f.answer(http.StatusOK, s1Answer)
	if res := testBackend("k").Do(context.Background(), target(p), s1Call(t, s1Request), nil); res.Err != nil {
		t.Fatalf("%+v", res.Err)
	}
	if got := f.last().path; got != "/v1/systemone" {
		t.Errorf("path %q", got)
	}
}
