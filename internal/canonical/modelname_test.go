package canonical

import "testing"

// The measured cases. These are the three ids the z.ai coding plan answered as
// something else on 2026-08-03, and the five on the same endpoint that answered
// as themselves. The whole check exists for the first group and must be silent
// for the second, so they are asserted separately from the general table.
func TestMeasuredSubstitutionsFire(t *testing.T) {
	for _, c := range []struct{ asked, served string }{
		{"glm-5.1", "glm-5.2"},
		{"glm-5", "glm-5.2"},
		{"glm-4.5-air", "glm-4.7"},
	} {
		if got := CompareModel(c.asked, c.served); got != ModelSubstituted {
			t.Errorf("CompareModel(%q, %q) = %v, want substituted — this is the "+
				"measured failure the check exists for", c.asked, c.served, got)
		}
	}
}

func TestMeasuredEchoesAreSilent(t *testing.T) {
	// z.ai's honest five, Ollama Cloud's four, and the qwen token plan's three.
	// Every one of these was asked and answered by itself.
	for _, m := range []string{
		"glm-5.2", "glm-4.7", "glm-4.5", "glm-4.6", "glm-5-turbo",
		"glm-5.1", "deepseek-v4-flash:0731", "gpt-oss:120b",
		"qwen3.8-max-preview", "qwen3.6-flash",
	} {
		got := CompareModel(m, m)
		if got != ModelEcho {
			t.Errorf("CompareModel(%q, %q) = %v, want echo", m, m, got)
		}
		if got.Substituted() {
			t.Errorf("%q compared with itself reports a substitution", m)
		}
	}
}

// A tagged name must survive a round trip untouched. Ollama Cloud returns
// `deepseek-v4-flash:0731` byte-identical, and the `:` is a literal character
// that nothing in dorang splits on (DESIGN §2.1, REVIEW C3).
func TestTagIsNotSuspicious(t *testing.T) {
	for _, m := range []string{
		"deepseek-v4-flash:0731", "gpt-oss:120b", "gpt-oss:20b",
		"gemma4:31b", "mistral-large-3:675b",
	} {
		if got := CompareModel(m, m); got != ModelEcho {
			t.Errorf("tagged %q round trip = %v, want echo", m, got)
		}
	}
}

func TestCompareModel(t *testing.T) {
	cases := []struct {
		name         string
		asked, serve string
		want         ModelAgreement
	}{
		// ---- refinement: a dated build of the asked model ----
		{"openai snapshot", "gpt-4o", "gpt-4o-2024-08-06", ModelRefined},
		{"anthropic snapshot", "claude-haiku-4-5", "claude-haiku-4-5-20251001", ModelRefined},
		{"cohere dated", "command-a", "command-a-03-2025", ModelRefined},
		{"mistral yymm", "mistral-medium", "mistral-medium-2508", ModelRefined},
		{"mistral yymm 2", "mistral-small", "mistral-small-2603", ModelRefined},
		{"doubao dated", "doubao-seed-1-8", "doubao-seed-1-8-251228", ModelRefined},
		{"ollama style tag", "deepseek-v4-flash", "deepseek-v4-flash:0731", ModelRefined},
		{"at separator", "gemini-3.1-pro", "gemini-3.1-pro@20260115", ModelRefined},
		// The catalog carries both spellings of this pair, which is the whole
		// reason separator punctuation is normalized.
		{"dot vs dash plus date", "glm-4.7", "glm-4-7-251222", ModelRefined},
		{"deepseek dot vs dash", "deepseek-v3.2", "deepseek-v3-2-251201", ModelRefined},
		// Less specific is not a substitution.
		{"generalized", "gpt-4o-2024-08-06", "gpt-4o", ModelRefined},
		{"generalized dated", "kimi-k2-5-260127", "kimi-k2.5", ModelRefined},

		// ---- echo: same model, different spelling ----
		{"case only", "deepseek-ai/deepseek-v4-pro", "deepseek-ai/DeepSeek-V4-Pro", ModelEcho},
		{"case only 2", "minimaxai/minimax-m2.5", "MiniMaxAI/MiniMax-M2.5", ModelEcho},
		{"separator only", "kimi-k2-5", "kimi-k2.5", ModelEcho},
		{"underscore", "hy3_preview", "hy3-preview", ModelEcho},

		// ---- floating alias ----
		{"latest resolved", "mistral-large-latest", "mistral-large-2512", ModelRefined},
		{"latest to stem", "mistral-large-latest", "mistral-large", ModelEcho},
		{"latest echo", "codestral-latest", "codestral-latest", ModelEcho},
		{"latest colon", "llama3:latest", "llama3:20260101", ModelRefined},
		{"latest both sides", "gpt-5.3-chat-latest", "gpt-5.3-chat", ModelEcho},
		// A `-latest` answer to a specific ask is still the alias, not a swap.
		{"specific asked alias served", "mistral-large-2512", "mistral-large-latest", ModelRefined},

		// ---- substitution: a version bump ----
		{"minor bump", "glm-5", "glm-5.2", ModelSubstituted},
		{"minor bump dash", "claude-opus-4", "claude-opus-4-6", ModelSubstituted},
		{"two version parts", "mistral-medium", "mistral-medium-3-5", ModelSubstituted},
		{"patch bump", "llama-3", "llama-3.1", ModelSubstituted},
		{"sibling version", "gpt-5.4", "gpt-5.5", ModelSubstituted},
		{"sibling version 2", "claude-opus-4-7", "claude-opus-4-8", ModelSubstituted},

		// ---- substitution: a variant is chosen, not stamped ----
		{"air variant", "glm-4.5", "glm-4.5-air", ModelSubstituted},
		{"turbo variant", "glm-5", "glm-5-turbo", ModelSubstituted},
		{"mini variant", "gpt-5.4", "gpt-5.4-mini", ModelSubstituted},
		{"pro variant", "gpt-5.5", "gpt-5.5-pro", ModelSubstituted},
		{"preview variant", "hy3", "hy3-preview", ModelSubstituted},
		{"size variant", "gpt-oss", "gpt-oss-120b", ModelSubstituted},
		{"named variant", "gpt-5.6", "gpt-5.6-luna", ModelSubstituted},
		{"tee variant", "MiniMaxAI/MiniMax-M2.5", "MiniMaxAI/MiniMax-M2.5-TEE", ModelSubstituted},

		// ---- substitution: unrelated names ----
		{"unrelated", "glm-4.5-air", "glm-4.7", ModelSubstituted},
		{"unrelated family", "gpt-5.4", "claude-opus-4-8", ModelSubstituted},
		{"namespace stripped", "moonshotai/kimi-k2.5", "kimi-k2.5", ModelSubstituted},
		{"namespace added", "kimi-k2.5", "moonshotai/kimi-k2.5", ModelSubstituted},

		// ---- the two cases this rule gets RIGHT and must not decide alone ----
		//
		// Both are from a live operator configuration and both are correct
		// upstream behaviour. This function reports them as substitutions, which
		// is what they are as strings; the caller's catalog gate is what
		// silences them, because neither returned name is a model dorang holds
		// figures for. internal/app's TestAnAliasResolvedByTheUpstreamIsNotA-
		// Substitution and TestADecoratedNameIsNotASubstitution are the halves
		// that pin the silence.
		{"operator alias resolved to a gguf", "local",
			"Gemma-4-Garnet-V2-31B-it-ultra-uncensored-heretic.i1-Q6_K.gguf", ModelSubstituted},
		{"suffix dropped and prefix added", "nvidia/llama-nemotron-embed-vl-1b-v2:free",
			"private/openrouter/nvidia/llama-nemotron-embed-vl-1b-v2", ModelSubstituted},

		// ---- no reading ----
		{"empty served", "glm-5.1", "", ModelUnobserved},
		{"empty asked", "", "glm-5.2", ModelUnobserved},
		{"both empty", "", "", ModelUnobserved},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CompareModel(c.asked, c.serve); got != c.want {
				t.Errorf("CompareModel(%q, %q) = %v, want %v", c.asked, c.serve, got, c.want)
			}
			if got := CompareModelBytes(c.asked, []byte(c.serve)); got != c.want {
				t.Errorf("CompareModelBytes(%q, %q) = %v, want %v", c.asked, c.serve, got, c.want)
			}
		})
	}
}

// A name past the record bound is unobserved rather than truncated: a truncated
// name compares as a different model, and a control that invents a substitution
// is worse than one that misses it.
func TestOverlongServedNameIsUnobserved(t *testing.T) {
	long := make([]byte, MaxServedModelBytes+1)
	for i := range long {
		long[i] = 'a'
	}
	if got := CompareModel("gpt-5.4", string(long)); got != ModelUnobserved {
		t.Errorf("overlong served name = %v, want unobserved", got)
	}
}

// Every id in the shipped catalog must echo itself. This is the cry-wolf guard:
// if any real model name compares as anything but an echo against itself, the
// check fires on ordinary traffic and gets switched off.
func TestEveryCatalogIDEchoesItself(t *testing.T) {
	for _, m := range catalogIDs {
		if got := CompareModel(m, m); got != ModelEcho {
			t.Errorf("catalog id %q does not echo itself: %v", m, got)
		}
	}
}

// catalogIDs is a transcription of pkg/catalog/model_catalog.yaml's model ids.
// internal/canonical must not import the catalog — the dependency runs the
// other way — so the names are copied rather than loaded.
var catalogIDs = []string{
	"accounts/fireworks/models/kimi-k2p6", "accounts/fireworks/routers/kimi-k2p6-turbo",
	"anthropic/claude-sonnet-4.6", "ark-code-latest", "claude-fable-5",
	"claude-haiku-4-5", "claude-haiku-4-5-20251001", "claude-mythos-5",
	"claude-opus-4-6", "claude-opus-4-7", "claude-opus-4-8", "claude-sonnet-4-6",
	"claude-sonnet-5", "codestral-latest", "command-a-03-2025",
	"command-a-plus-05-2026", "command-a-reasoning-08-2025", "command-a-vision-07-2025",
	"deepseek-ai/DeepSeek-V3.2", "deepseek-ai/DeepSeek-V3.2-TEE",
	"deepseek-ai/DeepSeek-V4-Flash", "deepseek-ai/deepseek-v4-pro",
	"deepseek-chat", "deepseek/deepseek-r1-0528", "deepseek/deepseek-v3-0324",
	"deepseek-reasoner", "deepseek-v3.2", "deepseek-v3-2-251201",
	"deepseek-v4-flash", "deepseek-v4-flash:0731", "deepseek-v4-pro",
	"devstral-medium-latest", "doubao-seed-1-8-251228", "doubao-seed-code",
	"doubao-seed-code-preview-251028", "ernie-5.0-thinking-preview",
	"gemini-3.1-pro", "gemini-3-1-pro-preview", "gemini-3-flash-preview",
	"gemma4:31b", "glm-4.5", "glm-4.5-air", "glm-4.6", "glm-4.7",
	"glm-4-7-251222", "glm-5", "glm-5.1", "glm-5.2", "glm-5-turbo",
	"google/gemini-3.1-flash-lite", "google-gemma-3-27b-it",
	"gpt-5.3-chat-latest", "gpt-5.3-codex", "gpt-5.4", "gpt-5.4-mini",
	"gpt-5.4-nano", "gpt-5.4-pro", "gpt-5.5", "gpt-5.5-pro", "gpt-5.6",
	"gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-oss-120b",
	"gpt-oss:120b", "gpt-oss:20b", "grok-4.3", "grok-4.5", "groq/compound",
	"groq/compound-mini", "hermes-3-llama-3.1-405b", "hf:moonshotai/Kimi-K2.7-Code",
	"hf:nvidia/NVIDIA-Nemotron-3-Super-120B-A12B-NVFP4", "hf:openai/gpt-oss-120b",
	"hf:Qwen/Qwen3.6-27B", "hf:zai-org/GLM-4.7-Flash", "hf:zai-org/GLM-5.2",
	"hy3", "hy3-preview", "jina-embeddings-v5-text-nano",
	"jina-embeddings-v5-text-small", "jina-reranker-v3", "k3[1m]",
	"kilo-auto/balanced", "kimi-k2-5", "kimi-k2.5", "kimi-k2-5-260127",
	"kimi-k2.6", "kimi-k2.7-code", "kimi-k2.7-code-highspeed", "kimi-k3",
	"llama-3.1-8b-instant", "llama-3.3-70b-versatile", "LongCat-2.0",
	"magistral-small", "meta-llama/Llama-3.3-70B-Instruct-Turbo",
	"meta-llama/llama-4-scout-17b-16e-instruct", "mimo-v2.5", "mimo-v2.5-pro",
	"minimaxai/minimax-m2.5", "MiniMaxAI/MiniMax-M2.5", "MiniMaxAI/MiniMax-M2.5-TEE",
	"minimaxai/minimax-m2.7", "minimaxai/minimax-m3", "minimax-m25",
	"MiniMax-M2.5", "minimax-m2.7", "minimax-m3", "minimax/minimax-m2.7",
	"mistral-large-3:675b", "mistral-large-latest", "mistral-medium-2508",
	"mistral-medium-3-5", "mistral-small-2603", "mistral-small-latest",
	"moonshotai/kimi-k2.5", "moonshotai/Kimi-K2.5", "moonshotai/Kimi-K2.5-TEE",
	"moonshotai/kimi-k2.6", "qwen3.8-max-preview", "qwen3.7-max",
	"nemotron-3-nano:30b", "nemotron-3-super", "nemotron-3-ultra",
}

// Nothing here may allocate: it runs once per non-streaming response and once
// per stream, on the path DESIGN §15 keeps free of per-request allocation.
func TestCompareModelDoesNotAllocate(t *testing.T) {
	served := []byte("glm-5.2")
	sink := ModelUnobserved
	if n := testing.AllocsPerRun(1000, func() {
		sink |= CompareModel("glm-5.1", "glm-5.2")
		sink |= CompareModelBytes("glm-5.1", served)
		sink |= CompareModel("gpt-4o", "gpt-4o-2024-08-06")
		sink |= CompareModel("mistral-large-latest", "mistral-large-2512")
	}); n != 0 {
		t.Errorf("CompareModel allocated %v times per run, want 0", n)
	}
}

func TestModelAgreementString(t *testing.T) {
	for _, c := range []struct {
		a    ModelAgreement
		want string
	}{
		{ModelUnobserved, "unobserved"},
		{ModelEcho, "echo"},
		{ModelRefined, "refined"},
		{ModelSubstituted, "substituted"},
	} {
		if got := c.a.String(); got != c.want {
			t.Errorf("%d.String() = %q, want %q", c.a, got, c.want)
		}
	}
}
