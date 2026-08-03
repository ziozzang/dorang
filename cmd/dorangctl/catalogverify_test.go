package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/pkg/catalog"
)

// TestCatalogUnverifiedDistinguishesTheStates is the reason the report was
// rewritten: the old output was one flat list of everything undated, in which
// an entry nobody had ever typed sat beside one that had been asked and had
// refused, and the operator could not tell which was their problem.
func TestCatalogUnverifiedDistinguishesTheStates(t *testing.T) {
	out, errOut, code := invoke("catalog", "unverified")
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, errOut)
	}
	for _, want := range []string{
		"verified", "denied", "substituted", "citation_only", "unchecked",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not name state %q:\n%s", want, out)
		}
	}
	// The findings, with their evidence — a state name alone is not a report.
	for _, want := range []string{
		"eligible",                        // the refusal text, which is what makes a denial legible
		"served instead",                  // the substituted id
		"reasoning capability is unknown", // the other axis, kept separate
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the report omits %q:\n%s", want, out)
		}
	}
	// Never asked is summarised by kind, not printed 195 times.
	if strings.Count(out, "kind=venice") > 1 {
		t.Errorf("citation-only entries are listed one per line; they should be counted per kind:\n%s", out)
	}
}

func TestCatalogUnverifiedFiltersByState(t *testing.T) {
	out, _, code := invoke("catalog", "unverified", "--state", "substituted")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "kind=glm") || strings.Contains(out, "kind=venice") {
		t.Errorf("--state substituted did not filter:\n%s", out)
	}

	_, errOut, code := invoke("catalog", "unverified", "--state", "nonsense")
	if code == 0 {
		t.Error("an unknown state exited 0")
	}
	if !strings.Contains(errOut, "unknown state") {
		t.Errorf("unhelpful error: %s", errOut)
	}
}

// fakeProvider answers like a real one: a map of model id to a handler
// decision, so classification is tested against wire shapes rather than mocks
// of the classifier.
func fakeProvider(t *testing.T, answers map[string]func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fn, ok := answers[body.Model]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"error":{"message":"model %s does not exist"}}`, body.Model)
			return
		}
		fn(w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func okAs(served string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"x","model":%q,"choices":[{"message":{"content":"hi"}}]}`, served)
	}
}

func refuse(status int, msg string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"error":{"message":%q}}`, msg)
	}
}

// overlayFor writes a catalog overlay declaring a kind pointed at srv, with the
// named models, so verify has something to walk.
func overlayFor(t *testing.T, kind string, models ...string) string {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "version: 1\nkinds:\n  %s:\n    api: openai-chat\nmodels:\n", kind)
	for _, m := range models {
		fmt.Fprintf(&b, "  - { kind: %s, model: %q }\n", kind, m)
	}
	p := filepath.Join(t.TempDir(), "overlay.yaml")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestVerifyClassifiesWhatTheEndpointSaid covers the four answers that a
// listing cannot produce, against a server that produces them.
//
// The substitution case is the one worth the whole exercise: it is a 200 with a
// body, indistinguishable from success to anything that reads the status code.
func TestVerifyClassifiesWhatTheEndpointSaid(t *testing.T) {
	srv := fakeProvider(t, map[string]func(http.ResponseWriter){
		"answers": okAs("answers"),
		"aliased": okAs("answers"),
		"nopurchase": refuse(http.StatusForbidden,
			"Access to model denied. Please make sure you are eligible for using the model."),
		"gone": refuse(http.StatusBadRequest, "kimi-k2.5 was retired at 2026-07-31"),
	})
	path := overlayFor(t, "acme", "answers", "aliased", "nopurchase", "gone", "never-heard-of-it")

	t.Setenv("FAKE_KEY", "sk-test")
	out, errOut, code := invoke("catalog", "verify", "--kind", "acme",
		"--catalog", path, "--base-url", srv.URL, "--key-env", "FAKE_KEY")
	if code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}

	for _, want := range []string{
		"answers      verified",
		"aliased      substituted",
		"nopurchase   denied",
		"gone         retired",
		"never-heard-of-it  absent",
	} {
		if !strings.Contains(collapseSpace(out), collapseSpace(want)) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// A retired or absent entry is a deletion, and a deletion is a human's call.
	if !strings.Contains(out, "NOT written") {
		t.Errorf("the report does not say retired/absent results are not written:\n%s", out)
	}
}

// TestVerifyWritesLoadableCatalogData closes the loop: what verification found
// becomes catalog data with no transcription step, and the data it writes is
// accepted by the same loader that reads the embedded files.
func TestVerifyWritesLoadableCatalogData(t *testing.T) {
	srv := fakeProvider(t, map[string]func(http.ResponseWriter){
		"answers": okAs("answers"),
		"aliased": okAs("answers"),
		"nope": refuse(http.StatusForbidden,
			"Access to model denied. Please make sure you are eligible for using the model."),
	})
	path := overlayFor(t, "acme", "answers", "aliased", "nope")
	written := filepath.Join(t.TempDir(), "verified.yaml")

	t.Setenv("FAKE_KEY", "sk-test")
	_, errOut, code := invoke("catalog", "verify", "--kind", "acme",
		"--catalog", path, "--base-url", srv.URL, "--key-env", "FAKE_KEY", "--write", written)
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, errOut)
	}

	cat, err := catalog.Loader{Paths: []string{path, written}}.Load()
	if err != nil {
		body, _ := os.ReadFile(written)
		t.Fatalf("the file verify wrote does not load: %v\n%s", err, body)
	}
	for model, want := range map[string]catalog.Verification{
		"answers": catalog.VerificationVerified,
		"aliased": catalog.VerificationSubstituted,
		"nope":    catalog.VerificationDenied,
	} {
		if got := cat.Verification("acme", model); got != want {
			t.Errorf("after loading what verify wrote, %s is %q, want %q", model, got, want)
		}
	}
	if got := cat.Probe("acme", "aliased").Served; got != "answers" {
		t.Errorf("served id did not survive the round trip: %q", got)
	}
}

// TestVerifyRefusesToBlameModelsForACredential is the HuggingFace lesson.
//
// A token whose scope lacked Inference Providers returned 403 on every model. A
// tool that recorded that as a model fact would have written 200-odd false
// denials into a catalog whose entire contract is that its absences mean
// something. One model refusing where others answer is a model fact; every
// model refusing is a key fact.
func TestVerifyRefusesToBlameModelsForACredential(t *testing.T) {
	srv := fakeProvider(t, map[string]func(http.ResponseWriter){
		"a": refuse(http.StatusForbidden, "This authentication method does not have sufficient permissions"),
		"b": refuse(http.StatusForbidden, "This authentication method does not have sufficient permissions"),
		"c": refuse(http.StatusForbidden, "This authentication method does not have sufficient permissions"),
	})
	path := overlayFor(t, "acme", "a", "b", "c")
	written := filepath.Join(t.TempDir(), "verified.yaml")

	t.Setenv("FAKE_KEY", "sk-scopeless")
	_, errOut, code := invoke("catalog", "verify", "--kind", "acme",
		"--catalog", path, "--base-url", srv.URL, "--key-env", "FAKE_KEY", "--write", written)
	if code == 0 {
		t.Error("a credential that reached nothing exited 0")
	}
	if !strings.Contains(errOut, "scope or key problem") {
		t.Errorf("the failure was not attributed to the credential: %s", errOut)
	}
	if _, err := os.Stat(written); err == nil {
		body, _ := os.ReadFile(written)
		t.Errorf("it wrote model facts from a credential failure:\n%s", body)
	}
}

// TestVerifyNeedsACredentialByName: an unasked model must stay unasked rather
// than acquire a date, and a key must never be a flag value.
func TestVerifyNeedsACredentialByName(t *testing.T) {
	path := overlayFor(t, "acme", "a")

	_, errOut, code := invoke("catalog", "verify", "--kind", "acme", "--catalog", path, "--base-url", "http://127.0.0.1:1")
	if code == 0 {
		t.Error("verify ran without a credential")
	}
	if !strings.Contains(errOut, "--key-env") || !strings.Contains(errOut, "shell history") {
		t.Errorf("the error does not explain why the key is named, not passed: %s", errOut)
	}

	t.Setenv("EMPTY_KEY", "")
	_, errOut, code = invoke("catalog", "verify", "--kind", "acme", "--catalog", path,
		"--base-url", "http://127.0.0.1:1", "--key-env", "EMPTY_KEY")
	if code == 0 {
		t.Error("verify ran with an empty credential")
	}
	if !strings.Contains(errOut, "must stay unasked") {
		t.Errorf("unhelpful error: %s", errOut)
	}

	// --dry-run is the costed preview: it needs no credential and sends nothing.
	out, _, code := invoke("catalog", "verify", "--kind", "acme", "--catalog", path,
		"--base-url", "http://127.0.0.1:1", "--dry-run")
	if code != 0 {
		t.Errorf("--dry-run needed a credential")
	}
	if !strings.Contains(out, "would ask") {
		t.Errorf("--dry-run output: %s", out)
	}
}

// TestVerifyDoesNotReadAModelFactOutOfAPathError: a 404 that does not name the
// model is the endpoint path, which is what a Responses-only route answers to a
// chat request. Reading it as "the model does not exist" would delete rows for
// a wrong flag.
func TestVerifyDoesNotReadAModelFactOutOfAPathError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"message":"Unrecognized request URL"}}`)
	}))
	t.Cleanup(srv.Close)
	path := overlayFor(t, "acme", "a")

	t.Setenv("FAKE_KEY", "sk-test")
	out, _, _ := invoke("catalog", "verify", "--kind", "acme", "--catalog", path,
		"--base-url", srv.URL, "--key-env", "FAKE_KEY")
	if strings.Contains(out, "absent") {
		t.Errorf("a path 404 was read as a missing model:\n%s", out)
	}
	if !strings.Contains(out, "wrong endpoint path") {
		t.Errorf("the report does not name the likely cause:\n%s", out)
	}
}

// TestVerifyProbesEachCategoryOnItsOwnSurface: a chat request says nothing
// about an embeddings model, and jina's three entries live on two surfaces.
func TestVerifyProbesEachCategoryOnItsOwnSurface(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fmt.Fprintf(w, `{"model":%q,"data":[]}`, body.Model)
	}))
	t.Cleanup(srv.Close)

	p := filepath.Join(t.TempDir(), "overlay.yaml")
	if err := os.WriteFile(p, []byte(`version: 1
kinds:
  fakejina:
    api: jina
    category: embedding
models:
  - { kind: fakejina, model: "emb", category: embedding }
  - { kind: fakejina, model: "rank", category: rerank }
`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("FAKE_KEY", "sk-test")
	out, errOut, code := invoke("catalog", "verify", "--kind", "fakejina", "--catalog", p,
		"--base-url", srv.URL, "--key-env", "FAKE_KEY")
	if code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, errOut)
	}
	if !strings.Contains(paths[0], "/embeddings") || !strings.Contains(paths[1], "/rerank") {
		t.Errorf("probes went to %v; each category must be asked on its own surface", paths)
	}
	if strings.Count(out, "verified") == 0 {
		t.Errorf("neither surface verified:\n%s", out)
	}
}
