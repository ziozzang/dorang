package catalog

import (
	"strings"
	"testing"
)

// The kind whose host serves only /responses says so, and nothing else does.
//
// The flag selects the backend adapter and gates two provider settings, so a
// kind that lost it would send chat-completions requests at a host that 403s
// them, and a kind that gained it by accident would send Responses bodies at a
// chat route. Both directions are asserted against the embedded data.
func TestOnlyTheCodexKindIsResponsesOnly(t *testing.T) {
	c := loadWith(t, "version: 1\n")
	kd, ok := c.Kind("codex-responses")
	if !ok {
		t.Fatal("codex-responses is not a kind")
	}
	if !kd.ResponsesOnly {
		t.Error("codex-responses is not declared responses_only; the chat adapter would be " +
			"selected and the host answers 403 on that route (measured 2026-08-06)")
	}
	via, ok := c.Kind("codex")
	if !ok || !via.ResponsesOnly {
		t.Errorf("the codex alias does not carry the flag: ok=%t %+v", ok, via)
	}
	for _, name := range c.Kinds() {
		if name == "codex-responses" {
			continue
		}
		if kd, _ := c.Kind(name); kd.ResponsesOnly {
			t.Errorf("kind %q is declared responses_only; no measured host but Codex is", name)
		}
	}
}

// responses_only next to any api but openai-responses is a contradiction.
func TestResponsesOnlyRequiresTheResponsesShape(t *testing.T) {
	c := loadWith(t, `
version: 1
kinds:
  odd-host:
    api: openai-chat
    category: chat
    responses_only: true
`)
	var found bool
	for _, p := range c.Validate() {
		if p.Kind == "odd-host" && p.Field == FieldResponsesOnly && p.Severity == SeverityError {
			found = true
			if !strings.Contains(p.Message, "openai-responses") {
				t.Errorf("the problem does not say which shape the flag implies: %s", p.Message)
			}
		}
	}
	if !found {
		t.Error("a responses_only kind speaking openai-chat validated cleanly; the adapter the " +
			"flag selects would encode a shape the api field contradicts")
	}
}

// Ollama Cloud declares the three surfaces it was measured to serve, and the
// declaration is validated: an unknown surface name and a surface beside
// `responses` on a responses_only kind are both refused at load.
func TestSurfacesAreDeclaredAndValidated(t *testing.T) {
	c := loadWith(t, "version: 1\n")
	kd, _ := c.Kind("ollama-cloud")
	if strings.Join(kd.Surfaces, ",") != "chat,messages,responses" {
		t.Errorf("ollama-cloud surfaces = %v, want the three measured routes", kd.Surfaces)
	}
	if kd, _ := c.Kind("codex-responses"); len(kd.Surfaces) != 0 {
		t.Errorf("codex-responses declares surfaces %v; it serves one route and says so with responses_only", kd.Surfaces)
	}
	mustLoadFail(t, `
version: 1
kinds:
  odd:
    api: openai-chat
    category: chat
    surfaces: [chat, completions]
`, "completions")
	mustLoadFail(t, `
version: 1
kinds:
  odd:
    api: openai-responses
    category: chat
    responses_only: true
    surfaces: [messages]
`, "responses_only")
}

// The azure kind speaks the Azure wire shape, which its aliases inherit.
func TestTheAzureKindSpeaksTheAzureShape(t *testing.T) {
	c := loadWith(t, "version: 1\n")
	for _, name := range []string{"azure", "azure-openai", "microsoft-foundry"} {
		kd, ok := c.Kind(name)
		if !ok || kd.API != APIAzureOpenAI {
			t.Errorf("%s: api = %q ok=%t, want %q", name, kd.API, ok, APIAzureOpenAI)
		}
	}
}

// The vertex kind speaks the Vertex shape; Claude on Vertex stays its own kind.
func TestTheVertexKindSpeaksTheVertexShape(t *testing.T) {
	c := loadWith(t, "version: 1\n")
	if kd, ok := c.Kind("vertex"); !ok || kd.API != APIVertex {
		t.Errorf("vertex api = %q ok=%t", kd.API, ok)
	}
	if kd, ok := c.Kind("anthropic-vertex"); !ok || kd.API != APIAnthropicMessages {
		t.Errorf("anthropic-vertex api = %q ok=%t", kd.API, ok)
	}
}
