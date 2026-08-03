package config

import (
	"strings"
	"testing"
)

// Three settings a real migration of 56 deployments exposed, and the discipline
// they share: the importer MOVED data and lost meaning, twice reporting success
// while doing it (an object_permission_id carried into a column nothing reads,
// per-token rates copied into per-million fields).
//
// So every test here asserts the REPORT as well as the value. A translation the
// importer cannot make must be named, with what the operator should do instead;
// silence is what made the other two expensive.

// ---------------------------------------------------------------------------
// 1. additional_drop_params — now load-bearing, so the import is faithful
// ---------------------------------------------------------------------------

// TestImportedDropListLoads is the whole point of the field no longer being
// inert: what the importer writes must survive validation, or the operator's
// first `dorangctl config lint` undoes the migration.
func TestImportedDropListLoads(t *testing.T) {
	c, warnings, err := ImportProxyConfig([]byte(`
model_list:
  - model_name: g
    litellm_params:
      model: m
      custom_llm_provider: openai
      api_base: https://api.example.invalid/v1
      api_key: os.environ/K
      additional_drop_params: [max_tokens, max_completion_tokens]
`))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	p, _ := c.Provider("openai")
	if len(p.Params.Drop) != 2 {
		t.Fatalf("params.drop = %v, want both names", p.Params.Drop)
	}
	// Both names are ones dorang models, so neither draws the pass-through
	// caveat: a warning on a setting that translated cleanly is noise, and noise
	// is how the warnings that matter get skimmed past.
	if warningsContain(warnings, "", "not a parameter dorang models") {
		t.Errorf("a clean translation was warned about:\n%s", warningStrings(warnings))
	}
	if err := c.Validate(); err != nil {
		for _, p := range Problems(err) {
			if strings.HasPrefix(p.Path, "providers[0].params.drop") {
				t.Fatalf("the importer wrote a drop list its own validator refuses: %v", p)
			}
		}
	}
}

// TestUndroppableNameIsRefusedAtLoadAndNamedAtImport. §10.1 already answers
// "this deployment cannot express the construct" — it refuses, and
// x-dorang-allow-lossy is how a caller consents. A drop list that could delete
// the caller's tools and answer 200 would be a second, disagreeing answer.
func TestUndroppableNameIsRefusedAtLoadAndNamedAtImport(t *testing.T) {
	_, err := LoadBytes([]byte(buildYAML(fragments{
		providers: "  - {name: p2, kind: openai, base_url: \"https://x.invalid\", params: {drop: [tools]}}\n",
	})))
	mustRefuse(t, err, "providers[1].params.drop[0]", "x-dorang-allow-lossy")

	_, warnings, ierr := ImportProxyConfig([]byte(`
model_list:
  - model_name: g
    litellm_params:
      model: m
      custom_llm_provider: openai
      api_key: os.environ/K
      additional_drop_params: [tools]
`))
	if ierr != nil {
		t.Fatalf("import: %v", ierr)
	}
	if !warningsContain(warnings, "", "NOT imported") {
		t.Errorf("an undroppable name was imported in silence:\n%s", warningStrings(warnings))
	}
}

// TestDropUnsupportedFalseIsRefused closes the other half of the §23.1 row
// params.drop was on.
//
// It cannot be wired: `false` asks for a parameter the target's wire shape has
// no field for to be forwarded anyway, and the encoder writes struct fields —
// the knob it lacks is a field that does not exist. The proxy this spelling
// comes from re-emits the caller's own JSON, which is the only shape in which
// the setting means anything. Leaving it accepted-and-inert beside a params.drop
// that now works is the state a reader can least afford.
func TestDropUnsupportedFalseIsRefused(t *testing.T) {
	_, err := LoadBytes([]byte(buildYAML(fragments{
		providers: "  - {name: p2, kind: openai, base_url: \"https://x.invalid\", params: {drop_unsupported: false}}\n",
	})))
	mustRefuse(t, err, "providers[1].params.drop_unsupported", "dorang converts rather than relaying")
	if !strings.Contains(err.Error(), "params.drop") {
		t.Errorf("the refusal names no working alternative: %v", err)
	}

	// true is the truth and stays accepted, so the refusal is about the claim
	// and not about the key.
	if _, err := LoadBytes([]byte(buildYAML(fragments{
		providers: "  - {name: p2, kind: openai, base_url: \"https://x.invalid\", params: {drop_unsupported: true}}\n",
	}))); err != nil {
		t.Errorf("drop_unsupported: true was refused: %v", err)
	}
}

// TestUnmodelledDropNamesTheCaveat. A vendor knob dorang has no struct field for
// is still worth dropping — it crosses a same-family hop in the pass-through map
// — but on a crossing between families dorang never sends it, so the drop is
// already the effect there. An operator migrating a setting they believe is
// load-bearing is owed that distinction.
func TestUnmodelledDropNamesTheCaveat(t *testing.T) {
	_, warnings, err := ImportProxyConfig([]byte(`
model_list:
  - model_name: g
    litellm_params:
      model: m
      custom_llm_provider: openai
      api_key: os.environ/K
      additional_drop_params: [repetition_penalty]
`))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !warningsContain(warnings, "", "not a parameter dorang models") {
		t.Errorf("the pass-through caveat was not reported:\n%s", warningStrings(warnings))
	}
}

// ---------------------------------------------------------------------------
// 2. Per-deployment max_tokens — a field that exists, and a translation refused
// ---------------------------------------------------------------------------

// TestPerDeploymentMaxTokensIsRefusedByName is the case the discipline is for.
//
// The number is real and dorang now has a field shaped like it, and it still
// must not be copied: the source applies max_tokens as a DEFAULT for a caller
// who named none, and max_output_tokens is a CEILING that refuses a caller who
// named more. They constrain opposite halves of the traffic. An import that
// moved the literal would take every request over 65536 that succeeds today and
// start failing it — while reporting success, which is exactly the failure that
// was just closed twice.
func TestPerDeploymentMaxTokensIsRefusedByName(t *testing.T) {
	c, warnings, err := ImportProxyConfig([]byte(`
model_list:
  - model_name: g
    litellm_params:
      model: m
      custom_llm_provider: openai
      api_key: os.environ/K
      max_tokens: 65536
`))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if got := c.Models[0].Deployments[0].MaxOutputTokens; got != 0 {
		t.Fatalf("max_tokens was copied into max_output_tokens as %d; the two are different settings", got)
	}
	if !warningsContain(warnings, "model_list[0].litellm_params.max_tokens", "DEFAULT") {
		t.Fatalf("the refusal does not say what the source setting means:\n%s", warningStrings(warnings))
	}
	// Both remedies, or the operator is told what dorang will not do and not
	// what they should.
	for _, want := range []string{"CEILING", "model catalog", "max_output_tokens: 65536"} {
		if !warningsContain(warnings, "", want) {
			t.Errorf("the report never mentions %q:\n%s", want, warningStrings(warnings))
		}
	}
}

// TestMaxOutputTokensLoads pins the schema half: the ceiling an operator is told
// to write has to be a key the file accepts.
func TestMaxOutputTokensLoads(t *testing.T) {
	c, err := LoadBytes([]byte(buildYAML(fragments{
		models: "  - name: m2\n    deployments:\n" +
			"      - {provider: p1, upstream_model: u, credentials: [c1], max_output_tokens: 65536}\n",
	})))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := c.Models[1].Deployments[0].MaxOutputTokens; got != 65536 {
		t.Errorf("max_output_tokens = %d", got)
	}

	// Negative is refused. Zero is not: zero is "unset", and it is the only
	// value that has to keep meaning "take the catalog's figure".
	_, err = LoadBytes([]byte(buildYAML(fragments{
		models: "  - name: m2\n    deployments:\n" +
			"      - {provider: p1, upstream_model: u, credentials: [c1], max_output_tokens: -1}\n",
	})))
	mustRefuse(t, err, "models[1].deployments[0].max_output_tokens", "negative")
}

// ---------------------------------------------------------------------------
// 3. access_via_team_ids — the capability exists; the expression is elsewhere
// ---------------------------------------------------------------------------

// TestTeamGatedModelIsNamedPerRow. 26 of 56 real rows carried this. dorang's
// model allow-list IS consulted per key, per user and per team — the mechanism
// is not missing — but a team is a directory row and no configuration file can
// express one, so the importer cannot write it anywhere. That makes the report
// the entire deliverable for these rows, and "model_info is not part of dorang's
// schema" reads as "cosmetic metadata was skipped".
func TestTeamGatedModelIsNamedPerRow(t *testing.T) {
	_, warnings, err := ImportProxyConfig([]byte(`
model_list:
  - model_name: g1
    litellm_params: {model: m1, custom_llm_provider: openai, api_key: os.environ/K}
    model_info:
      access_via_team_ids: ["team-a", "team-b"]
  - model_name: g2
    litellm_params: {model: m2, custom_llm_provider: openai, api_key: os.environ/K}
    model_info:
      access_via_team_ids: ["team-a"]
`))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	// Per ROW. A single summary line at the end is how 26 restrictions become
	// one sentence an operator scrolls past.
	for _, path := range []string{
		"model_list[0].model_info.access_via_team_ids",
		"model_list[1].model_info.access_via_team_ids",
	} {
		if !warningsContain(warnings, path, "NOT imported") {
			t.Errorf("row %s was not named:\n%s", path, warningStrings(warnings))
		}
	}
	// The three things the operator has to know: that the capability exists,
	// where it lives, and that the relation inverts in a way that is not
	// mechanical — an empty dorang list allows EVERYTHING, so a team nobody
	// gives a list to still reaches the model.
	for _, want := range []string{"/team/update", "inverts", "EMPTY dorang list allows everything"} {
		if !warningsContain(warnings, "", want) {
			t.Errorf("the report never mentions %q:\n%s", want, warningStrings(warnings))
		}
	}
}
