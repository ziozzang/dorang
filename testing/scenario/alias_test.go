package scenario

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/testing/fake"
)

// DESIGN §14 scenario 8 — alias.
//
//	upstream always receives the REAL model id (§7.2)
//	the response BODY carries the name the client asked for (§7.2)
//	the real model is exposed in a HEADER (§10.4)
//
// All three directions are asserted, and so are their inverses. Two of the
// three are satisfied by a gateway that forwards the request verbatim and never
// rewrites anything, so "the body carries model-small" on its own proves
// nothing at all.

const (
	aliasName = "model-small"  // what the client types
	realName  = "qwen3.5:397b" // what the backend is actually serving
	altReal   = "deepseek-v4-flash:cloud"
)

func aliasConfig() router.Config {
	return router.Config{
		Groups: []router.Group{{
			Name: "small-group", Class: "small",
			Deployments: []router.Deployment{{
				ID: "d1", Provider: "self-hosted", Kind: "vllm", UpstreamModel: realName,
				Credentials: []router.Credential{{ID: "k1"}},
			}},
		}},
		// Aliases do not chain, and nothing splits either name (§2.1, §7.2).
		Aliases: map[string]string{aliasName: "small-group"},
	}
}

func TestScenario08_AliasRealModelUpstreamRequestedNameInBody(t *testing.T) {
	g := newGateway(t, aliasConfig(), rigOpts{}, map[string]fake.Options{
		"d1": {
			Shape: fake.ShapeOpenAI,
			Script: func(r *fake.Recorded) fake.Script {
				// The backend echoes whatever model it was given, which is what
				// a real one does and what makes the rewrite visible.
				return fake.Script{
					Model: r.Model, Text: "aliased answer", TextChunks: 2,
					Usage: fake.Usage{InputTokens: 7, OutputTokens: 3},
				}
			},
		},
	})

	t.Run("non-streaming", func(t *testing.T) {
		rep, err := g.do(t, Call{Family: FamilyOpenAI,
			Body: []byte(`{"model":"` + aliasName + `","messages":[{"role":"user","content":"hi"}]}`)})
		if err != nil {
			t.Fatalf("Do: %v", err)
		}

		// 1. Upstream got the real id.
		up := g.ups["d1"].Last()
		if up == nil {
			t.Fatal("the deployment never received a request")
		}
		if up.Model != realName {
			t.Fatalf("upstream received model %q, want the real id %q", up.Model, realName)
		}

		// 2. The body carries the requested name.
		var body struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(rep.Body, &body); err != nil {
			t.Fatalf("response body is not JSON: %s", rep.Body)
		}
		if body.Model != aliasName {
			t.Fatalf("response body carries model %q, want the requested name %q", body.Model, aliasName)
		}

		// 3. The header carries the real one.
		if got := rep.Header.Get("x-dorang-upstream-model"); got != realName {
			t.Errorf("x-dorang-upstream-model = %q, want %q", got, realName)
		}
		if got := rep.Header.Get("x-dorang-model"); got != aliasName {
			t.Errorf("x-dorang-model = %q, want %q", got, aliasName)
		}
		if got := rep.Header.Get("x-dorang-deployment"); got != "d1" {
			t.Errorf("x-dorang-deployment = %q, want d1", got)
		}

		t.Run("inverse: neither name appears where the other belongs", func(t *testing.T) {
			// A verbatim forwarder passes "the body says model-small" and fails
			// here; a gateway that rewrote in the wrong direction passes
			// "upstream got qwen3.5:397b" and fails here.
			if strings.Contains(string(up.Body), aliasName) {
				t.Errorf("the client-facing alias reached the backend:\n%s", up.Body)
			}
			if strings.Contains(string(rep.Body), realName) {
				t.Errorf("the real upstream id leaked into the response body:\n%s", rep.Body)
			}
		})
	})

	t.Run("streaming: the model is restamped on every chunk", func(t *testing.T) {
		// COMPATIBILITY 2.5. Revision 1 of the design proposed patching the
		// first frame by byte offset; per-frame restamping is the required
		// behaviour, so every frame is checked rather than the first.
		rep, err := g.do(t, Call{Family: FamilyOpenAI,
			Body: []byte(`{"model":"` + aliasName + `","messages":[],"stream":true,` +
				`"stream_options":{"include_usage":true}}`)})
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		frames := sseFrames(rep.Body)
		if len(frames) < 4 {
			t.Fatalf("expected several frames, got %d:\n%s", len(frames), rep.Body)
		}
		checked := 0
		for _, f := range frames {
			payload := strings.TrimPrefix(f, "data: ")
			if payload == "[DONE]" {
				continue
			}
			var c struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal([]byte(payload), &c); err != nil {
				t.Fatalf("frame is not JSON: %q", payload)
			}
			if c.Model != aliasName {
				t.Errorf("frame carries model %q, want %q on EVERY chunk: %s", c.Model, aliasName, payload)
			}
			checked++
		}
		if checked < 4 {
			t.Fatalf("only %d frames carried a model field", checked)
		}
		if strings.Contains(string(rep.Body), realName) {
			t.Errorf("the real upstream id survived in the relayed stream:\n%s", rep.Body)
		}
		if g.ups["d1"].Last().Model != realName {
			t.Errorf("upstream received %q on the streaming path", g.ups["d1"].Last().Model)
		}
	})

	t.Run("the model name is opaque and is never split", func(t *testing.T) {
		// Both names contain characters a naive gateway treats as separators.
		// DESIGN §2.1 gives that exactly one rule and no exceptions.
		cfg := aliasConfig()
		cfg.Groups[0].Deployments[0].UpstreamModel = altReal
		g := newGateway(t, cfg, rigOpts{}, map[string]fake.Options{
			"d1": {Shape: fake.ShapeOpenAI, Script: func(r *fake.Recorded) fake.Script {
				return fake.Script{Model: r.Model, Text: "ok"}
			}},
		})
		rep, err := g.do(t, Call{Family: FamilyOpenAI,
			Body: []byte(`{"model":"` + aliasName + `","messages":[]}`)})
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		if got := g.ups["d1"].Last().Model; got != altReal {
			t.Fatalf("upstream received %q, want the whole opaque name %q", got, altReal)
		}
		if got := rep.Header.Get("x-dorang-upstream-model"); got != altReal {
			t.Fatalf("header = %q, want %q", got, altReal)
		}
	})

	t.Run("a name that is not an alias is passed through unchanged", func(t *testing.T) {
		r := newRig(t, aliasConfig(), rigOpts{})
		if got, _ := r.router.Resolve("small-group"); got != "small-group" {
			t.Errorf("Resolve(small-group) = %q, want it unchanged", got)
		}
		if got, _ := r.router.Resolve(aliasName); got != "small-group" {
			t.Errorf("Resolve(%s) = %q, want small-group", aliasName, got)
		}
	})
}
