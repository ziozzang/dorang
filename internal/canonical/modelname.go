package canonical

// Model identity: what the response body's own `model` field is evidence of.
//
// # Why this is a check and not documentation
//
// DESIGN §17.1 rule 3: for any control of the form "A disagrees with B", the
// first question is where A and B were each observed, and if the answer is "the
// same place, twice" the control is documentation rather than a check.
//
// Here A is `models[].deployments[].upstream_model` — a string dorang read out
// of its own configuration and put on the wire. B is the `model` member of the
// body the provider sent back. Two genuinely different origins, one of which
// dorang does not control, so the comparison can fail and the control can fire.
//
// It fires today. The z.ai coding plan (`kind: glm`) answers a request naming
// `glm-5.1` with HTTP 200 and a well-formed body whose `model` says `glm-5.2`;
// `glm-5` answers as `glm-5.2` and `glm-4.5-air` answers as `glm-4.7`. Three
// consecutive runs, two independent probes, on 2026-08-03. Five other ids on
// the SAME endpoint — `glm-5.2`, `glm-4.7`, `glm-4.5`, `glm-4.6`, `glm-5-turbo`
// — echo their own names, so substitution is per MODEL and not a property of
// the provider, and a per-provider allow-flag would be the wrong shape. Ollama
// Cloud and the qwen token plan echo exactly, so it is not universal either.
//
// `pkg/catalog/model_catalog.yaml` records the three under `probe:
// substituted`, which is the offline half of the same fact. This file is the
// online half, and it is the only one that can see a substitution that begins
// after the catalog was last verified.
//
// Nothing here rewrites, reroutes or refuses. Recording that an endpoint
// substitutes is not permission to substitute, and it is not permission to act
// either: a substitution is a fact to surface, and what an operator does about
// it is their decision.
//
// # This rule is half of the test, and it is the half that cannot be dropped
//
// [CompareModel] answers "are these two names the same model", and that is not
// the same question as "did a substitution happen". The caller — internal/app,
// which is the layer holding pkg/catalog — asks a second one: does the returned
// name resolve to a catalog ENTRY, under this deployment's kind, different from
// the one asked for. Both gates are required, and each rules out cases the
// other lets through.
//
// Without THIS rule the catalog test alone fires on ordinary version pinning,
// because the catalog carries dated builds as separate entries beside their
// family names — `claude-haiku-4-5` and `claude-haiku-4-5-20251001` are two
// kind:anthropic entries with identical figures, and Anthropic answers the
// first with the second on every request.
//
// Without the CATALOG gate this rule alone fires on names that are decorations
// rather than models. Two, both from a live operator configuration:
//
//	local                                     -> …-heretic.i1-Q6_K.gguf
//	nvidia/…-1b-v2:free                       -> private/openrouter/nvidia/…-1b-v2
//
// The first shares nothing at all with what was asked; the second drops a
// suffix and adds a prefix at once, so it is not a prefix, a suffix or a
// superstring of it. Both are reported [ModelSubstituted] here, correctly —
// they ARE different strings naming different things — and both are silenced
// upstream of this function, because neither returned name is a model dorang
// has ever had a price or a context window for. Nothing was mispriced against
// anything when there was no second figure to use instead.
//
// # What it does NOT try to detect
//
// Only identity. A provider that honours the model and discards a PARAMETER —
// the qwen token plan silently ignores `max_tokens` in favour of
// `max_completion_tokens`, serving 116 completion tokens for a request that
// asked for 16 — is a different phenomenon with a different and much weaker
// witness. Generalizing this into "the provider ignored something we sent"
// would cost the check the single unambiguous witness that makes it cheap and
// false-positive-free.

// ModelAgreement classifies the name a provider put in its response body
// against the name dorang asked for.
type ModelAgreement uint8

const (
	// ModelUnobserved is the absence of a reading: the response carried no
	// model name, or carried one too long to record. The absence of B is not a
	// disagreement with A, so this is never reported as a substitution.
	ModelUnobserved ModelAgreement = iota
	// ModelEcho is the asked name returned verbatim, up to ASCII case and
	// separator punctuation.
	ModelEcho
	// ModelRefined is a build of the asked model rather than a different model:
	// the two names differ only by a dated build tag. See [CompareModel].
	ModelRefined
	// ModelSubstituted is a DIFFERENT model. A 200, a body, tokens returned,
	// and the work was done by something other than what was asked for.
	ModelSubstituted
)

func (a ModelAgreement) String() string {
	switch a {
	case ModelEcho:
		return "echo"
	case ModelRefined:
		return "refined"
	case ModelSubstituted:
		return "substituted"
	}
	return "unobserved"
}

// Substituted reports whether a is the one value worth acting on.
func (a ModelAgreement) Substituted() bool { return a == ModelSubstituted }

// MaxServedModelBytes bounds how long a returned name may be and still be
// recorded. The longest id in pkg/catalog/model_catalog.yaml is 47 bytes, so
// this is generous by more than a factor of two. A name past the bound is
// reported [ModelUnobserved] rather than truncated, because a truncated name
// compares as a different model and a control that INVENTS a substitution is
// worse than one that misses it.
const MaxServedModelBytes = 128

// CompareModel classifies served against asked.
//
// # Why a naive string inequality is not the rule
//
// Providers legitimately answer with a MORE SPECIFIC form of the name they were
// given. OpenAI answers `gpt-4o` with `gpt-4o-2024-08-06`; Anthropic answers
// `claude-haiku-4-5` with `claude-haiku-4-5-20251001`; Cohere dates its own ids
// `command-a-03-2025`; Mistral resolves `mistral-large-latest` to a build. A
// check that fired on any of those would fire on ordinary traffic, and a
// control that cries wolf gets switched off — which is how it ends up in the
// §17.1 ledger as one more control that exists and is not reached.
//
// So the rule has to separate "a build of what I asked for" from "a different
// model", and it has to do so without a table of provider quirks: the
// substituting endpoint above is honest about five of its eight ids, so no
// per-provider flag gets it right.
//
// # The rule
//
//	served == asked, up to case and separator punctuation      echo
//	one is the other plus a dated build tag                    refined
//	anything else                                              substituted
//
// A build tag is a separator (`-`, `_`, `.`, `:` or `@`) followed by one or
// more `-`/`_`/`.`-separated segments, every segment non-empty and all digits,
// carrying AT LEAST FOUR DIGITS in total.
//
// Two clauses do the work, and each is answering a measured case.
//
// **Separator punctuation is not identity.** `-`, `_` and `.` compare equal to
// each other. The catalog is the evidence: it carries `kimi-k2.5` and
// `kimi-k2-5`, `deepseek-v3.2` and `deepseek-v3-2-251201`, `glm-4.7` and
// `glm-4-7-251222` — the same model, spelled by different providers with
// different punctuation. A byte-exact compare would call every one of those a
// substitution. (`:` and `/` are NOT in this class: `:` is Ollama's tag
// delimiter and `/` is a namespace delimiter, and `zai-org/GLM-5.2` is not
// `zai-org-GLM-5.2`.)
//
// **Four digits is what tells a date from a version.** This is the whole check,
// and it is what makes the measured case fire while the ordinary case does not:
//
//	glm-5        -> glm-5.2               tag "2"          1 digit   SUBSTITUTED
//	claude-opus-4-> claude-opus-4-6        tag "6"          1 digit   SUBSTITUTED
//	mistral-medium->mistral-medium-3-5     tag "3-5"        2 digits  SUBSTITUTED
//	glm-5        -> glm-5-turbo            not digits                 SUBSTITUTED
//	glm-4.5-air  -> glm-4.7                no shared stem             SUBSTITUTED
//	gpt-4o       -> gpt-4o-2024-08-06      tag "2024-08-06" 8 digits  refined
//	claude-haiku-4-5 -> ...-20251001       tag "20251001"   8 digits  refined
//	glm-4.7      -> glm-4-7-251222         tag "251222"     6 digits  refined
//	mistral-medium -> mistral-medium-2508  tag "2508"       4 digits  refined
//	deepseek-v4-flash:0731 (round trip)                               echo
//
// Every dated build stamp any provider in the catalog uses — YYYY-MM-DD,
// YYYYMMDD, YYMMDD, MM-YYYY, YYMM, MMDD — carries four digits or more. Every
// version component any of them uses carries one to three. A variant (`-air`,
// `-turbo`, `-mini`, `-pro`, `-preview`, `-120b`) is not digits at all, and a
// variant is a different model: it is chosen, not stamped.
//
// # Symmetric
//
// Refinement counts in either direction. A provider that answers a dated id
// with the bare family name is being LESS specific, not serving something else,
// and firing on that would be the same cry-wolf failure. Only when neither name
// refines the other is the pair reported as a substitution.
//
// # `-latest`
//
// A name ending in `-latest`, `_latest`, `.latest`, `:latest` or `@latest` is a
// floating alias whose entire meaning is "resolve me to whatever build is
// current", so a resolved answer to it is the alias working. The stem is
// compared instead: `mistral-large-latest` answered by `mistral-large-2512` is
// refined, and answered by `mistral-large` is an echo of the stem.
//
// # Case
//
// The comparison folds ASCII case. The catalog carries `MiniMaxAI/MiniMax-M2.5`
// beside `minimaxai/minimax-m2.5`, and `deepseek-ai/DeepSeek-V4-Pro` beside
// `deepseek-ai/deepseek-v4-pro`, because providers disagree with THEMSELVES
// about capitalizing one model. No provider distinguishes two models by case
// alone, so folding costs nothing and not folding invents substitutions.
//
// CompareModel allocates nothing.
func CompareModel(asked, served string) ModelAgreement {
	return compareModel(asked, served)
}

// CompareModelBytes is [CompareModel] against a name still held as bytes, for
// the SSE relay: the scanner locates the value inside a frame it never decodes,
// and converting it to a string to ask this question would put an allocation on
// a per-stream path that has none. See [CompareModel] for the rule.
func CompareModelBytes(asked string, served []byte) ModelAgreement {
	return compareModel(asked, served)
}

// modelText is the two spellings a name arrives in. Both support len, index and
// slice, which is all the rule needs, so one implementation serves both without
// a conversion and therefore without an allocation.
type modelText interface{ ~string | ~[]byte }

func compareModel[A modelText, S modelText](asked A, served S) ModelAgreement {
	if len(served) == 0 || len(served) > MaxServedModelBytes {
		return ModelUnobserved
	}
	if len(asked) == 0 {
		// Nothing was asked for by name, so nothing can disagree with it.
		return ModelUnobserved
	}
	if foldEqual(asked, served) {
		return ModelEcho
	}
	// An alias on either side is compared by its stem, so a resolved answer to
	// `-latest` is the alias working rather than a substitution.
	if a, s := aliasStem(asked), aliasStem(served); a != len(asked) || s != len(served) {
		if a == s && foldEqualN(asked, served, a) {
			return ModelEcho
		}
	}
	if refines(asked, served) || refines(served, asked) {
		return ModelRefined
	}
	return ModelSubstituted
}

// refines reports whether long is base plus a dated build tag.
func refines[B modelText, L modelText](base B, long L) bool {
	n := aliasStem(base)
	if n == 0 || n >= len(long) {
		return false
	}
	if !foldEqualN(base, long, n) {
		return false
	}
	switch long[n] {
	case '-', '_', '.', ':', '@':
	default:
		return false
	}
	return buildTag(long, n+1)
}

// aliasStem returns how much of name is the model id, dropping a trailing
// floating-alias suffix. It is len(name) for every name that does not carry
// one.
func aliasStem[T modelText](name T) int {
	const suffix = "latest"
	n := len(name)
	if n <= len(suffix)+1 {
		return n
	}
	cut := n - len(suffix)
	switch name[cut-1] {
	case '-', '_', '.', ':', '@':
	default:
		return n
	}
	for i := 0; i < len(suffix); i++ {
		if lower(name[cut+i]) != suffix[i] {
			return n
		}
	}
	return cut - 1
}

// buildTag reports whether s[from:] is a dated build stamp: one or more
// separator-delimited segments, every segment non-empty and all digits, at
// least four digits in total.
func buildTag[T modelText](s T, from int) bool {
	digits, seg := 0, 0
	for i := from; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			digits++
			seg++
		case c == '-' || c == '_' || c == '.':
			if seg == 0 {
				return false
			}
			seg = 0
		default:
			return false
		}
	}
	return seg > 0 && digits >= 4
}

func foldEqual[A modelText, B modelText](a A, b B) bool {
	return len(a) == len(b) && foldEqualN(a, b, len(a))
}

// foldEqualN compares the first n bytes, folding ASCII case and treating `-`,
// `_` and `.` as one character.
func foldEqualN[A modelText, B modelText](a A, b B, n int) bool {
	if len(a) < n || len(b) < n {
		return false
	}
	for i := 0; i < n; i++ {
		x, y := a[i], b[i]
		if x == y {
			continue
		}
		if sepClass(x) && sepClass(y) {
			continue
		}
		if lower(x) != lower(y) {
			return false
		}
	}
	return true
}

// sepClass is the punctuation providers spell interchangeably within one id.
func sepClass(c byte) bool { return c == '-' || c == '_' || c == '.' }

// lower folds one ASCII byte. Non-ASCII bytes are left alone: a model id is not
// a place to apply Unicode case rules, and folding a UTF-8 continuation byte
// would equate names that differ.
func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}
