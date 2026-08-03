package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ziozzang/dorang/pkg/catalog"
)

// catalog verify exists because the catalog drifted three entries in six days
// and nobody would have noticed without asking.
//
// # Why asking, and not reading a listing
//
// A `/models` listing is evidence and not proof, and both directions have now
// been observed on this catalog's own providers. It omitted a model Ollama was
// still serving, so absence from a listing does not mean absence. It named nine
// qwen models that refuse every request, so presence does not mean reachable.
// And it cannot see the third case at all: three z.ai names return 200 with a
// body naming a DIFFERENT model, which is invisible to anything short of
// reading the response.
//
// So this sends one real request per entry and reads the answer. The requests
// are minimal — one short user turn, max_tokens in the low tens — because they
// are billed to the operator's own plan.
//
// # Why it does not read the configuration
//
// The credential is named by env var, never taken as a flag value: a key in a
// flag lands in shell history and in every `ps` on the box. Reading the
// deployment's configuration instead would tie verification to a running
// deployment, and the common case is an operator who holds a key for one
// provider and wants that provider's rows checked.
//
// # What it refuses to conclude
//
// A previous pass hit a HuggingFace token whose scope lacked Inference
// Providers and got a 403 on every model. That 403 is a fact about the token,
// not about any model, and recording it as one would have poisoned the catalog
// with 200-odd false denials. So: an auth-shaped failure that hits EVERY model
// identically is reported as a credential problem and nothing is written. Only
// a refusal that names eligibility, on a route where something else succeeded,
// is a denial.

// verifyOutcome is what one probe concluded. It is deliberately wider than
// catalog.Verification: some answers are findings a human has to act on rather
// than data to write down.
type verifyOutcome string

const (
	outcomeVerified    verifyOutcome = "verified"
	outcomeSubstituted verifyOutcome = "substituted"
	outcomeDenied      verifyOutcome = "denied"

	// outcomeRetired and outcomeAbsent are never written to an overlay. The
	// catalog's answer to a model that serves nobody is to DELETE the entry,
	// with the reason in a comment, and a tool that deleted catalog rows on
	// the strength of one HTTP response would be the last thing this design
	// wants. They are reported for a human to act on.
	outcomeRetired verifyOutcome = "retired"
	outcomeAbsent  verifyOutcome = "absent"

	// outcomeError is anything that says nothing about the model: a timeout, a
	// 5xx, a rejected credential, a wire shape this tool does not speak.
	outcomeError verifyOutcome = "error"
)

// probeReport is one model's result.
type probeReport struct {
	Model   string
	Outcome verifyOutcome
	Served  string
	Status  int
	Detail  string
}

func (e env) catalogVerify(args []string) int {
	fs := newFlagSet("catalog verify", e)
	overlays := fs.String("catalog", "", "extra model catalog files or directories, comma separated")
	kind := fs.String("kind", "", "provider kind whose entries to check (required)")
	baseURL := fs.String("base-url", "", "endpoint to ask; defaults to the kind's catalogued base_url")
	keyEnv := fs.String("key-env", "", "environment variable holding the credential (required)")
	only := fs.String("model", "", "check only these models, comma separated")
	maxTokens := fs.Int("max-tokens", 16, "max_tokens per probe; these requests bill to your plan")
	timeout := fs.Duration("timeout", 120*time.Second, "per-request timeout")
	write := fs.String("write", "", "write the results to this file as a catalog overlay")
	dryRun := fs.Bool("dry-run", false, "list what would be asked, and ask nothing")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *kind == "" {
		return e.fail("catalog verify needs --kind")
	}

	cat, err := catalog.Load(splitComma(*overlays)...)
	if err != nil {
		return e.fail("%v", err)
	}
	kd, ok := cat.Kind(*kind)
	if !ok {
		return e.fail("no such kind %q", *kind)
	}
	endpoint := strings.TrimRight(firstNonEmpty(*baseURL, kd.BaseURL), "/")
	if endpoint == "" {
		return e.fail("kind %q declares no base_url; pass --base-url", *kind)
	}

	models := catalogModelsOfKind(cat, kd.Name, splitComma(*only))
	if len(models) == 0 {
		return e.fail("kind %q has no catalogued models to check", *kind)
	}

	if *dryRun {
		fmt.Fprintf(e.stdout, "would ask %s for %d model(s), max_tokens=%d:\n",
			endpoint, len(models), *maxTokens)
		for _, m := range models {
			fmt.Fprintf(e.stdout, "  %s  (%s)\n", m.Model, cat.Model(kd.Name, m.Model).Category)
		}
		return 0
	}

	if *keyEnv == "" {
		return e.fail("catalog verify needs --key-env NAME, the environment variable holding the " +
			"credential. The key is never a flag value: a flag lands in shell history and in ps")
	}
	key := os.Getenv(*keyEnv)
	if key == "" {
		return e.fail("$%s is empty; nothing can be asked without a credential, and an "+
			"unasked model must stay unasked rather than acquire a date", *keyEnv)
	}

	client := &http.Client{Timeout: *timeout}
	today := time.Now().UTC().Format(time.DateOnly)
	reports := make([]probeReport, 0, len(models))
	for _, ref := range models {
		info := cat.Model(kd.Name, ref.Model)
		r := probeOne(client, endpoint, key, kd.API, info, *maxTokens, *timeout)
		reports = append(reports, r)
	}

	e.writeVerifyReport(reports, endpoint, kd.Name)

	// The HuggingFace lesson, enforced: a credential that fails on everything
	// is a fact about the credential.
	if bad, why := credentialLooksWrong(reports); bad {
		fmt.Fprintf(e.stderr, "\ndorangctl: %s\n", why)
		fmt.Fprintf(e.stderr, "dorangctl: nothing written; that is a fact about $%s, not about any model\n", *keyEnv)
		return 1
	}

	if *write != "" {
		if err := writeOverlay(*write, kd.Name, endpoint, today, reports); err != nil {
			return e.fail("%v", err)
		}
		fmt.Fprintf(e.stdout, "\nwrote %s — review it, then load it with --catalog or $%s\n",
			*write, catalog.EnvCatalogPath)
	}
	return 0
}

// catalogModelsOfKind lists the kind's entries, optionally filtered to a named
// subset. Names are compared whole: nothing here splits a model name.
func catalogModelsOfKind(cat *catalog.Catalog, kind string, only []string) []catalog.ModelRef {
	want := map[string]bool{}
	for _, m := range only {
		want[m] = true
	}
	var out []catalog.ModelRef
	for _, ref := range cat.Models() {
		if ref.Kind != kind {
			continue
		}
		if len(want) > 0 && !want[ref.Model] {
			continue
		}
		out = append(out, ref)
	}
	return out
}

// probeOne sends exactly one request and classifies the answer.
func probeOne(client *http.Client, endpoint, key string, api catalog.API,
	info catalog.ModelInfo, maxTokens int, timeout time.Duration,
) probeReport {
	rep := probeReport{Model: info.Model}

	path, body, headers, err := probeRequest(endpoint, api, info, maxTokens)
	if err != nil {
		rep.Outcome, rep.Detail = outcomeError, err.Error()
		return rep
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		rep.Outcome, rep.Detail = outcomeError, err.Error()
		return rep
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		rep.Outcome, rep.Detail = outcomeError, err.Error()
		return rep
	}
	defer resp.Body.Close()
	// Bounded: a probe reads enough to classify, not a whole completion.
	raw := readAtMost(resp.Body, 1<<16)
	rep.Status = resp.StatusCode
	classify(&rep, info.Model, resp.StatusCode, raw)
	return rep
}

// classify turns one HTTP answer into an outcome.
//
// The order matters. A success that names a different model is a substitution,
// not a success — that is the case a status code cannot see. A failure is read
// from its TEXT, because the text is the only thing that separates "your plan
// is not entitled" from "there is no such model": Ollama's retirements name a
// date and a reference id, and an entitlement refusal talks about eligibility.
func classify(rep *probeReport, asked string, status int, raw []byte) {
	body := string(raw)
	lower := strings.ToLower(body)

	if status >= 200 && status < 300 {
		if served, ok := servedModel(raw); ok && served != asked {
			rep.Outcome, rep.Served = outcomeSubstituted, served
			rep.Detail = fmt.Sprintf("%d OK, response model field says %q", status, served)
			return
		} else if !ok {
			// Answered, but named nothing. Established, with the gap in the
			// evidence recorded rather than smoothed over.
			rep.Outcome = outcomeVerified
			rep.Detail = fmt.Sprintf("%d OK; the response names no model, so this "+
				"establishes that the endpoint answers to the name and no more", status)
			return
		}
		rep.Outcome, rep.Detail = outcomeVerified, fmt.Sprintf("%d OK, answered as itself", status)
		return
	}

	// Absence is read from LANGUAGE, never from the model name appearing in the
	// body. A name is an arbitrary string: testing whether a one-character
	// model id occurs somewhere in an error message matches almost any message,
	// and the failure mode is a live entry reported as missing.
	absence := strings.Contains(lower, "model") && containsAny(lower,
		"does not exist", "not found", "unknown model", "no such model",
		"invalid model", "unsupported model")

	switch {
	case containsAny(lower, "was retired", "has been retired", "is retired", "no longer available", "deprecated and removed"):
		rep.Outcome, rep.Detail = outcomeRetired, firstLine(body)
	case containsAny(lower, "eligible", "unpurchased", "not entitled", "no permission", "not authorized to use", "not subscribed", "purchase"):
		rep.Outcome, rep.Detail = outcomeDenied, firstLine(body)
	case status == http.StatusUnauthorized:
		rep.Outcome, rep.Detail = outcomeError, "credential rejected: "+firstLine(body)
	case status == http.StatusForbidden:
		// A bare 403 with no eligibility language is ambiguous between a model
		// this account cannot reach and a credential whose scope is wrong. The
		// caller decides using the whole run; alone it concludes nothing.
		rep.Outcome, rep.Detail = outcomeError, "403 with no eligibility language: "+firstLine(body)
	case absence:
		rep.Outcome, rep.Detail = outcomeAbsent, firstLine(body)
	case status == http.StatusNotFound:
		// A 404 that does not talk about a model is the PATH, not the model —
		// which is what a Responses-only endpoint answers to a chat request.
		rep.Outcome, rep.Detail = outcomeError,
			"404 that names no missing model; wrong endpoint path for this kind? "+firstLine(body)
	default:
		rep.Outcome, rep.Detail = outcomeError, fmt.Sprintf("%d %s", status, firstLine(body))
	}
}

// servedModel pulls the model id out of a response body. Every wire shape this
// tool speaks names the served model at the top level.
func servedModel(raw []byte) (string, bool) {
	var envelope struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Model == "" {
		return "", false
	}
	return envelope.Model, true
}

// probeRequest builds the single minimal request for one entry.
//
// It dispatches on the model's CATEGORY as well as the kind's wire adapter: a
// chat request tells you nothing about an embeddings model, and jina's three
// entries live on two different surfaces. An adapter with no probe shape here
// is refused rather than guessed at — sending an invented request shape and
// reading the resulting error as a fact about the model is exactly the mistake
// this command exists to stop.
func probeRequest(endpoint string, api catalog.API, info catalog.ModelInfo, maxTokens int) (
	url string, body []byte, headers map[string]string, err error,
) {
	switch info.Category {
	case catalog.CategoryEmbedding:
		if api != catalog.APIJina && api != catalog.APIOpenAIChat {
			return "", nil, nil, fmt.Errorf("no embedding probe shape for api %q", api)
		}
		b, _ := json.Marshal(map[string]any{"model": info.Model, "input": []string{"hi"}})
		return endpoint + "/embeddings", b, nil, nil

	case catalog.CategoryRerank:
		if api != catalog.APIJina && api != catalog.APICohere {
			return "", nil, nil, fmt.Errorf("no rerank probe shape for api %q", api)
		}
		b, _ := json.Marshal(map[string]any{
			"model": info.Model, "query": "hi", "documents": []string{"a", "b"}, "top_n": 1,
		})
		return endpoint + "/rerank", b, nil, nil

	case catalog.CategoryChat, catalog.CategoryCompletion, "":
		switch api {
		case catalog.APIOpenAIChat, catalog.APIOpenAIResponses:
			// The compatible-mode surface, even for kinds whose adapter is
			// Responses: a 404 from it is classified as a path problem, never
			// as a missing model.
			b, _ := json.Marshal(map[string]any{
				"model":      info.Model,
				"messages":   []map[string]string{{"role": "user", "content": "hi"}},
				"max_tokens": maxTokens,
				"stream":     false,
			})
			return endpoint + "/chat/completions", b, nil, nil
		case catalog.APIAnthropicMessages:
			b, _ := json.Marshal(map[string]any{
				"model":      info.Model,
				"messages":   []map[string]string{{"role": "user", "content": "hi"}},
				"max_tokens": maxTokens,
			})
			return endpoint + "/v1/messages", b, map[string]string{
				"anthropic-version": "2023-06-01",
			}, nil
		}
		return "", nil, nil, fmt.Errorf("no chat probe shape for api %q", api)
	}
	return "", nil, nil, fmt.Errorf("no probe shape for category %q", info.Category)
}

// credentialLooksWrong reports whether the run says more about the credential
// than about the models.
//
// One model refusing on a route where others answer is a fact about that model.
// EVERY model failing the same way is a fact about the key — the HuggingFace
// token whose scope lacked Inference Providers produced exactly that, a 403 on
// every model, and writing 200-odd denials from it would have been the worst
// possible outcome for a catalog whose absences are supposed to mean something.
func credentialLooksWrong(reports []probeReport) (bool, string) {
	if len(reports) == 0 {
		return false, ""
	}
	authish := 0
	for _, r := range reports {
		if r.Outcome == outcomeVerified || r.Outcome == outcomeSubstituted {
			return false, ""
		}
		if r.Status == http.StatusUnauthorized || r.Status == http.StatusForbidden {
			authish++
		}
	}
	if authish == len(reports) {
		return true, fmt.Sprintf("all %d probes failed with 401/403 and none succeeded; "+
			"a credential that cannot reach ANY model on a route is a scope or key problem, "+
			"not %d separate model facts", len(reports), len(reports))
	}
	return false, ""
}

func (e env) writeVerifyReport(reports []probeReport, endpoint, kind string) {
	fmt.Fprintf(e.stdout, "asked %s for %d model(s) on kind %s\n\n", endpoint, len(reports), kind)
	tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  MODEL\tOUTCOME\tDETAIL")
	counts := map[verifyOutcome]int{}
	for _, r := range reports {
		counts[r.Outcome]++
		detail := r.Detail
		if r.Served != "" {
			detail = "served " + r.Served + "; " + detail
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", r.Model, r.Outcome, truncate(collapseSpace(detail), 90))
	}
	_ = tw.Flush()

	fmt.Fprintln(e.stdout)
	for _, o := range []verifyOutcome{outcomeVerified, outcomeSubstituted, outcomeDenied, outcomeRetired, outcomeAbsent, outcomeError} {
		if counts[o] > 0 {
			fmt.Fprintf(e.stdout, "  %-12s %d\n", o, counts[o])
		}
	}
	if counts[outcomeRetired]+counts[outcomeAbsent] > 0 {
		fmt.Fprintf(e.stdout, "\n%d entry(s) came back retired or absent. Those are NOT written: "+
			"the catalog's answer to a model that serves nobody is to delete the entry with the "+
			"reason in a comment, and no tool should delete catalog rows on one HTTP response.\n",
			counts[outcomeRetired]+counts[outcomeAbsent])
	}
}

// overlayDoc is the shape written by --write: a catalog overlay, loadable by
// the same loader that reads the embedded data, so the output of verification
// is input to the catalog with no transcription step in between.
type overlayDoc struct {
	Version int            `yaml:"version"`
	Models  []overlayModel `yaml:"models"`
}

type overlayModel struct {
	Kind     string        `yaml:"kind"`
	Model    string        `yaml:"model"`
	Verified string        `yaml:"verified,omitempty"`
	Probe    *overlayProbe `yaml:"probe,omitempty"`
}

type overlayProbe struct {
	Result string `yaml:"result"`
	Date   string `yaml:"date"`
	Served string `yaml:"served,omitempty"`
	Note   string `yaml:"note,omitempty"`
}

// writeOverlay turns the run into loadable catalog data.
//
// Only the three outcomes that are facts about a model are written. Errors are
// omitted because they are facts about the network or the key; retirements and
// absences are omitted because acting on them is a deletion, and a deletion is
// a human's call.
func writeOverlay(path, kind, endpoint, date string, reports []probeReport) error {
	doc := overlayDoc{Version: 1}
	for _, r := range reports {
		switch r.Outcome {
		case outcomeVerified:
			doc.Models = append(doc.Models, overlayModel{Kind: kind, Model: r.Model, Verified: date})
		case outcomeSubstituted:
			doc.Models = append(doc.Models, overlayModel{Kind: kind, Model: r.Model, Probe: &overlayProbe{
				Result: string(catalog.ProbeSubstituted), Date: date, Served: r.Served, Note: r.Detail,
			}})
		case outcomeDenied:
			doc.Models = append(doc.Models, overlayModel{Kind: kind, Model: r.Model, Probe: &overlayProbe{
				Result: string(catalog.ProbeDenied), Date: date, Note: r.Detail,
			}})
		}
	}
	if len(doc.Models) == 0 {
		return fmt.Errorf("no probe established anything about a model; nothing to write")
	}

	body, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	header := fmt.Sprintf(`# Written by dorangctl catalog verify on %s.
#
#   kind:     %s
#   endpoint: %s
#   probes:   %d, one minimal request each
#
# Review before loading. A `+"`verified:`"+` here says the endpoint answered to
# the name on that date — not that the numbers on the entry are right, and not
# that the reasoning control is known (DESIGN §10.2, a separate probe).
#
# This layer SUPERSEDES the embedded answer for each entry it names: verified:
# and probe: share one slot, so a date here clears an inherited probe and a
# probe here clears an inherited date. Everything else on the entry — the
# context window, its citation — is untouched.
`, date, kind, endpoint, len(reports))
	return os.WriteFile(path, append([]byte(header), body...), 0o644)
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	s = collapseSpace(s)
	return truncate(s, 300)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// readAtMost reads a bounded prefix of a probe response. A probe reads enough
// to classify, not a whole completion, and an endpoint that streams forever is
// a hung probe rather than an out-of-memory.
func readAtMost(r io.Reader, n int64) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, n))
	return b
}
