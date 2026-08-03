package config

import (
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Warning is something an import could not represent. Import never drops
// silently: whatever does not map becomes one of these.
type Warning struct {
	Path    string
	Message string
}

func (w Warning) String() string {
	if w.Path == "" {
		return w.Message
	}
	return w.Path + ": " + w.Message
}

// envRefPrefix is how the foreign format references an environment variable.
const envRefPrefix = "os.environ/"

// kindByHint maps a foreign provider hint onto a dorang provider kind (§4.3).
// A hint that is not here is assumed OpenAI-compatible and warned about, which
// is the honest default: it is the wire shape almost every gateway speaks.
var kindByHint = map[string]string{
	"openai":                 "openai",
	"text-completion-openai": "openai",
	"azure":                  "azure",
	"azure_ai":               "azure",
	"anthropic":              "anthropic",
	"bedrock":                "bedrock",
	"vertex_ai":              "vertex",
	"vertex_ai_beta":         "vertex",
	"gemini":                 "google",
	"google":                 "google",
	"cohere":                 "cohere",
	"cohere_chat":            "cohere",
	"mistral":                "mistral",
	"deepseek":               "deepseek",
	"xai":                    "xai",
	"moonshot":               "moonshot",
	"minimax":                "minimax",
	"minimax-cn":             "minimax",
	"qwen":                   "qwen",
	"dashscope":              "qwen",
	"glm":                    "glm",
	"zhipu":                  "glm",
	"ollama":                 "ollama",
	"ollama_chat":            "ollama",
	"openrouter":             "openrouter",
	"jina":                   "jina",
	"jina_ai":                "jina",
	"vllm":                   "vllm",
	"hosted_vllm":            "vllm",
	"groq":                   "openai",
	"together_ai":            "openai",
	"fireworks_ai":           "openai",
}

// strategyByName maps a foreign routing strategy onto dorang's tie-break chain
// (§7.3).
var strategyByName = map[string][]string{
	"simple-shuffle":            {"weighted_random"},
	"least-busy":                {"least_busy"},
	"latency-based-routing":     {"lowest_latency"},
	"cost-based-routing":        {"lowest_cost"},
	"usage-based-routing":       {"least_busy"},
	"usage-based-routing-v2":    {"least_busy"},
	"lowest-tpm-rpm-routing-v2": {"least_busy"},
}

// ImportProxyConfig reads a declarative model-list configuration in the widely
// used proxy format and normalizes it into dorang's schema (§2.2).
//
// The returned configuration has defaults applied but is deliberately not
// validated: an imported file usually needs a decision or two from a human
// first, and every one of those is reported as a [Warning]. Call
// [Config.Validate] once those are settled.
//
// A "provider/model" model string is resolved only by matching it against the
// providers the source file actually declares. It is never split on a
// separator, and anything that cannot be resolved is reported and kept whole
// (§2.1 rule 5).
//
// Rates are CONVERTED, not copied. The source format quotes a token price per
// one token and dorang's `rates.input` is per a million of them, so a literal
// that came through unchanged was a price a millionth of the vendor's — silently,
// because the rule still matched. [priceFieldByKey] is the whole conversion
// table and the conversion itself is exact: a decimal point moves.
func ImportProxyConfig(data []byte) (*Config, []Warning, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, nil, fmt.Errorf("config: import: %w", err)
	}
	doc := &root
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil, nil, fmt.Errorf("config: import: the file is empty")
		}
		doc = doc.Content[0]
	}
	top, ok := asMap(doc)
	if !ok {
		return nil, nil, fmt.Errorf("config: import: the file must be a mapping at the top level")
	}

	im := &importer{cfg: &Config{Version: Version}}

	for _, k := range top.keys {
		switch k {
		case "model_list", "router_settings", "general_settings", "litellm_settings":
		case "environment_variables":
			im.warn(k, "environment variables are not copied into dorang's configuration; "+
				"set them in the process environment and reference them with key_env")
		default:
			im.warn(k, "top-level section is not part of dorang's schema and was not imported")
		}
	}

	if n, ok := top.get("model_list"); ok {
		if n.Kind != yaml.SequenceNode {
			return nil, nil, fmt.Errorf("config: import: model_list must be a sequence")
		}
		im.declareProviders(n)
		im.importModelList(n)
	} else {
		im.warn("model_list", "no model_list section: nothing to route")
	}
	if n, ok := top.get("litellm_settings"); ok {
		im.importLitellmSettings(n)
	}
	if n, ok := top.get("router_settings"); ok {
		im.importRouterSettings(n)
	}
	if n, ok := top.get("general_settings"); ok {
		im.importGeneralSettings(n)
	}

	im.reportRateConversions()
	im.cfg.ApplyDefaults()
	im.cfg.buildIndex()
	return im.cfg, im.warnings, nil
}

type importer struct {
	cfg      *Config
	warnings []Warning

	// providerByKey deduplicates providers by (hint, base URL).
	providerByKey map[string]string
	// credByKey deduplicates credentials by (provider, secret source).
	credByKey map[string]string
	// priceSeen guards against two entries pricing the same (provider, model)
	// differently.
	priceSeen map[string]string
	// priceKeyByComp records which of the incumbent's keys a component's rate
	// came from, so a collision between two of them can name both.
	priceKeyByComp map[string]string

	// convertedRates counts the rates whose quantity was not dorang's, and
	// firstConversion holds one of them verbatim for the summary line.
	convertedRates  int
	firstConversion string
}

// suggestEnvName invents the environment variable a literal secret should move
// to. Distinct literals for one provider get distinct names, so two accounts do
// not silently collapse onto one variable.
func (im *importer) suggestEnvName(provider string) string {
	base := "DORANG_" + envIdent(provider) + "_API_KEY"
	n := 0
	for i := range im.cfg.Credentials {
		if im.cfg.Credentials[i].Provider == provider {
			n++
		}
	}
	if n == 0 {
		return base
	}
	return base + "_" + strconv.Itoa(n+1)
}

// envIdent renders a provider id as an environment-variable fragment.
func envIdent(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
			out = append(out, c-32)
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "PROVIDER"
	}
	return string(out)
}

func (im *importer) warn(path, format string, args ...any) {
	im.warnings = append(im.warnings, Warning{Path: path, Message: fmt.Sprintf(format, args...)})
}

// declareProviders is the first pass: it builds the provider list from the
// fields that actually name a provider — custom_llm_provider and api_base —
// so the second pass has something to resolve a "provider/model" string
// against.
func (im *importer) declareProviders(list *yaml.Node) {
	im.providerByKey = map[string]string{}
	for i, entry := range list.Content {
		em, ok := asMap(entry)
		if !ok {
			continue
		}
		params, ok := entryParams(em)
		if !ok {
			continue
		}
		hint := strings.TrimSpace(scalarOf(params, "custom_llm_provider"))
		base := strings.TrimSpace(firstScalar(params, "api_base", "base_url"))
		if hint == "" && base == "" {
			continue
		}
		im.declareProvider(fmt.Sprintf("model_list[%d]", i), hint, base)
	}
}

// declareProvider returns the name of the provider for (hint, base), creating
// it if this is the first time it has been seen.
func (im *importer) declareProvider(path, hint, base string) string {
	if im.providerByKey == nil {
		im.providerByKey = map[string]string{}
	}
	key := hint + "\x00" + base
	if name, ok := im.providerByKey[key]; ok {
		return name
	}

	name := hint
	if name == "" {
		name = providerNameFromURL(base)
	}
	if name == "" {
		name = "imported"
	}
	// Two different base URLs under one hint are two providers.
	base0 := name
	for n := 2; im.providerDeclared(name); n++ {
		name = fmt.Sprintf("%s-%d", base0, n)
		im.warn(path, "provider %q is already declared with a different base URL; "+
			"this deployment's provider was named %q", base0, name)
	}

	kind, known := kindByHint[strings.ToLower(hint)]
	if !known {
		kind = "openai"
		if hint != "" {
			im.warn(path, "provider hint %q is not a known kind; provider %q was declared as an "+
				"OpenAI-compatible kind %q — check it against §4.3", hint, name, kind)
		} else {
			im.warn(path, "no provider hint for base URL %q; provider %q was declared as an "+
				"OpenAI-compatible kind %q — check it against §4.3", base, name, kind)
		}
	}
	im.cfg.Providers = append(im.cfg.Providers, Provider{Name: name, Kind: kind, BaseURL: base})
	im.providerByKey[key] = name
	return name
}

func (im *importer) providerDeclared(name string) bool {
	for i := range im.cfg.Providers {
		if im.cfg.Providers[i].Name == name {
			return true
		}
	}
	return false
}

// providerNameFromURL derives a provider name from a base URL's host. This
// reads the URL, not the model string: model names are never parsed (§2.1).
func providerNameFromURL(base string) string {
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return ""
	}
	host := u.Hostname()
	return strings.Trim(strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		default:
			return '-'
		}
	}, host), "-")
}

// resolveModelString maps a model field onto (provider, upstream model).
//
// The only way a provider prefix is recognized is by matching a declared
// provider name followed by "/". Nothing is split on a separator, and a string
// that cannot be resolved is kept whole and reported (§2.1 rule 5).
func (im *importer) resolveModelString(path, model, fallbackProvider string) (string, string) {
	names := make([]string, 0, len(im.cfg.Providers))
	for i := range im.cfg.Providers {
		names = append(names, im.cfg.Providers[i].Name)
	}
	// Longest declared name first, so "azure-eu" wins over "azure".
	sort.Slice(names, func(i, j int) bool {
		if len(names[i]) != len(names[j]) {
			return len(names[i]) > len(names[j])
		}
		return names[i] < names[j]
	})
	for _, name := range names {
		prefix := name + "/"
		if strings.HasPrefix(model, prefix) && len(model) > len(prefix) {
			return name, model[len(prefix):]
		}
	}
	if fallbackProvider != "" {
		return fallbackProvider, model
	}
	im.warn(path, "no declared provider matches model %q, and the entry names none of its own "+
		"(no custom_llm_provider, no api_base): the whole string was kept as the upstream model "+
		"and assigned to provider %q. A model name is opaque and is never split to guess a "+
		"provider (§2.1); declare the provider if the prefix names one",
		model, importedProviderName)
	return im.fallbackProvider(), model
}

// importedProviderName holds deployments whose provider could not be
// determined from the source file.
const importedProviderName = "imported"

func (im *importer) fallbackProvider() string {
	if !im.providerDeclared(importedProviderName) {
		im.cfg.Providers = append(im.cfg.Providers,
			Provider{Name: importedProviderName, Kind: "openai"})
		if im.providerByKey == nil {
			im.providerByKey = map[string]string{}
		}
		im.providerByKey["\x00"] = importedProviderName
	}
	return importedProviderName
}

func (im *importer) importModelList(list *yaml.Node) {
	byName := map[string]int{}
	for i, entry := range list.Content {
		path := fmt.Sprintf("model_list[%d]", i)
		em, ok := asMap(entry)
		if !ok {
			im.warn(path, "entry is not a mapping and was not imported")
			continue
		}
		name := strings.TrimSpace(scalarOf(em, "model_name"))
		if name == "" {
			im.warn(path, "entry has no model_name and was not imported")
			continue
		}
		params, ok := entryParams(em)
		if !ok {
			im.warn(path, "entry has no litellm_params and was not imported")
			continue
		}
		for _, k := range em.keys {
			switch k {
			case "model_name", "litellm_params", "params":
			case "model_info":
				if mi, ok := asMap(em.vals[k]); ok {
					for _, ik := range mi.keys {
						im.warn(path+".model_info."+ik,
							"model_info is not part of dorang's schema and was not imported")
					}
				}
			default:
				im.warn(path+"."+k, "field is not part of dorang's schema and was not imported")
			}
		}

		hint := strings.TrimSpace(scalarOf(params, "custom_llm_provider"))
		base := strings.TrimSpace(firstScalar(params, "api_base", "base_url"))
		entryProvider := ""
		if hint != "" || base != "" {
			entryProvider = im.declareProvider(path, hint, base)
		}

		model := scalarOf(params, "model")
		if model == "" {
			im.warn(path+".litellm_params.model", "entry has no model and was not imported")
			continue
		}
		provider, upstream := im.resolveModelString(path+".litellm_params.model", model, entryProvider)

		dep := Deployment{Provider: provider, UpstreamModel: upstream}
		im.importParams(path, params, provider, &dep, upstream)

		gi, ok := byName[name]
		if !ok {
			im.cfg.Models = append(im.cfg.Models, Model{Name: name})
			gi = len(im.cfg.Models) - 1
			byName[name] = gi
		}
		im.cfg.Models[gi].Deployments = append(im.cfg.Models[gi].Deployments, dep)
	}
}

// importParams maps one litellm_params block onto the deployment, the
// provider, and the pricing rules.
func (im *importer) importParams(path string, params *yamlMap, provider string, dep *Deployment, upstream string) {
	p := path + ".litellm_params"
	for _, k := range params.keys {
		n := params.vals[k]
		switch k {
		case "model", "custom_llm_provider", "api_base", "base_url":
			// handled by the caller

		case "api_key":
			raw := scalarValue(n)
			if raw == "" {
				continue
			}
			if id, ok := im.credential(p, provider, raw); ok {
				dep.Credentials = append(dep.Credentials, id)
			}

		case "rpm":
			im.limit(p+".rpm", dep, MetricRPM, n)
		case "tpm":
			im.limit(p+".tpm", dep, MetricTPM, n)
		case "max_parallel_requests":
			im.limit(p+".max_parallel_requests", dep, MetricMaxConcurrent, n)

		case "weight":
			if v, ok := intOf(n); ok {
				dep.Weight = int(v)
			} else {
				im.warn(p+".weight", "value %q is not a number and was not imported", scalarValue(n))
			}
		case "timeout":
			dep.Timeout = im.duration(p+".timeout", n)
		case "stream_timeout":
			dep.StreamTimeout = im.duration(p+".stream_timeout", n)

		case "max_retries":
			if v, ok := intOf(n); ok {
				im.setProviderRetry(provider, int(v))
			}

		case "drop_params":
			im.applyDropParams(p+".drop_params", n, provider)
		case "additional_drop_params":
			im.applyDropList(p+".additional_drop_params", n, provider)

		case "input_cost_per_second", "output_cost_per_second":
			// The incumbent's own semantic for these keys is the request's ELAPSED
			// TIME — it multiplies the rate by the response time — so that is what
			// they import as, faithfully. It is also the key an incumbent
			// configuration uses for a transcription model that bills the length
			// of the recording, and those two are different quantities (§10.7);
			// the one key cannot say which, so the import says so rather than
			// choosing silently.
			im.warn(p+"."+k, "imported as compute_seconds, which prices "+
				"the request's own wall time. If this model bills by the length of the "+
				"AUDIO it was given, change the component to audio_seconds — the two are "+
				"different quantities and dorang will not substitute one for the other")
			im.price(p, provider, upstream, k, n)

		default:
			if _, priced := priceFieldByKey[k]; priced {
				im.price(p, provider, upstream, k, n)
				continue
			}
			if advice, known := unpricedFieldAdvice[k]; known {
				im.warn(p+"."+k, "%s", advice)
				continue
			}
			im.warn(p+"."+k, "parameter has no equivalent in dorang's schema and was not imported")
		}
	}
}

func (im *importer) limit(path string, dep *Deployment, metric string, n *yaml.Node) {
	v, ok := intOf(n)
	if !ok {
		im.warn(path, "value %q is not a number and was not imported", scalarValue(n))
		return
	}
	dep.Limits = append(dep.Limits, Limit{Metric: metric, Value: v})
}

func (im *importer) duration(path string, n *yaml.Node) Duration {
	d, err := ParseDuration(scalarValue(n))
	if err != nil {
		im.warn(path, "%v; not imported", err)
		return 0
	}
	return Duration(d)
}

// credential turns an api_key field into a credential. "os.environ/NAME"
// becomes a key_env reference; a literal is imported as an inline key and
// warned about, because an inline literal only loads when server.env is
// development (§4.1).
func (im *importer) credential(path, provider, raw string) (string, bool) {
	if im.credByKey == nil {
		im.credByKey = map[string]string{}
	}
	var ref SecretRef
	var key string
	if env, ok := strings.CutPrefix(raw, envRefPrefix); ok {
		if env == "" {
			im.warn(path+".api_key", "empty environment reference; no credential was imported")
			return "", false
		}
		ref, key = SecretRef{Env: env}, provider+"\x00env\x00"+env
	} else {
		// A literal secret in the source file becomes a key_env REFERENCE in
		// the generated one, never a copy of the literal.
		//
		// Importing it inline used to be the behaviour, on the reasoning that
		// an inline literal only loads when server.env is development. That
		// reasoning is about loading; the problem is writing. The generated
		// configuration is printed to stdout — into a new file, into terminal
		// scrollback, into CI logs if the migration is scripted — and the one
		// tool whose entire job is to produce a configuration was the one
		// putting secrets in it (DESIGN §4.1).
		//
		// The operator still has the value: it is in the file they are
		// migrating FROM. What they need from us is where to put it, which the
		// warning names. The secret itself is not echoed, because the fix for
		// "a secret ended up somewhere it should not" is not to print it
		// somewhere else.
		envName := im.suggestEnvName(provider)
		ref, key = SecretRef{Env: envName}, provider+"\x00inline\x00"+raw
		im.warn(path+".api_key", "the api_key is a literal secret and was NOT copied into the "+
			"generated configuration. It was replaced with key_env: %s — set that environment "+
			"variable to the value from your source file before starting dorang (§4.1)", envName)
	}
	if id, ok := im.credByKey[key]; ok {
		return id, true
	}
	n := 1
	for i := range im.cfg.Credentials {
		if im.cfg.Credentials[i].Provider == provider {
			n++
		}
	}
	id := fmt.Sprintf("%s-key-%d", provider, n)
	im.cfg.Credentials = append(im.cfg.Credentials, Credential{ID: id, Provider: provider, Key: ref})
	im.credByKey[key] = id
	return id, true
}

// priceField is one of the incumbent's cost keys, mapped onto the component
// dorang prices and the power of ten between the two quantities.
type priceField struct {
	component string
	// pow10 converts a rate quoted per ONE of the incumbent's units into a rate
	// per one of dorang's. The incumbent quotes tokens per token; dorang's
	// `rates.input` is per MILLION tokens, so the factor is 10^6.
	pow10 int
}

// priceFieldByKey is the conversion table between the incumbent's rate keys and
// dorang's priced components — the table whose absence was the defect.
//
// Every key here crosses a unit boundary or is written down as crossing none,
// because the failure this table prevents is not a missing mapping, it is a
// mapping that looks right. `input_cost_per_token: 0.0000025` copied verbatim
// into `rates.input` parses, validates, matches, and prices a request at a
// MILLIONTH of the vendor's figure — exactly zero below about two hundred
// tokens — while `POST /spend/calculate` still answers `"missing": false`,
// because `missing` reports that no rule matched and one did (CONFIG §13.1c).
// An operator hits this on the migration path, at the moment they are moving
// off a gateway whose numbers they trusted.
//
// The scale is stated for every key, including the ones where it is 1. A blank
// would be indistinguishable from a key nobody thought about, which is how the
// token keys came to be copied through in the first place.
var priceFieldByKey = map[string]priceField{
	// Per token → per 1,000,000 tokens.
	"input_cost_per_token":            {"input", 6},
	"output_cost_per_token":           {"output", 6},
	"cache_read_input_token_cost":     {"cached_read", 6},
	"cache_creation_input_token_cost": {"cache_write", 6},
	"output_cost_per_reasoning_token": {"reasoning", 6},
	// Per character → per 1,000 characters.
	"input_cost_per_character":  {"characters", 3},
	"output_cost_per_character": {"characters", 3},
	// Per second → per second, and per request → per request: the incumbent's
	// quantity and dorang's are the same one, so the factor is 1.
	"input_cost_per_second":           {"compute_seconds", 0},
	"output_cost_per_second":          {"compute_seconds", 0},
	"input_cost_per_audio_per_second": {"audio_seconds", 0},
	"input_cost_per_request":          {"request", 0},
}

// unpricedFieldAdvice covers the incumbent's remaining rate keys: the ones
// dorang cannot express. They are named individually because the generic "this
// parameter has no equivalent" is not true of a RATE — the money is real, it
// simply has no axis here — and an operator who reads that line about a price
// will assume the price was covered by another key.
var unpricedFieldAdvice = map[string]string{
	"input_cost_per_image": "dorang has no per-image unit: §8.3 lists images among the priced " +
		"quantities and internal/pricing implements no rate for them, so the rate was NOT " +
		"imported. Price the request instead (rates.request) if every request carries the " +
		"same number of images",
	"output_cost_per_image": "dorang has no per-image unit and the rate was NOT imported; " +
		"see rates.request",
	"input_cost_per_pixel": "dorang has no per-pixel unit and the rate was NOT imported",
	"input_cost_per_video_per_second": "dorang prices a second of RECORDED AUDIO " +
		"(audio_seconds) and a second of the request's own wall time (compute_seconds), " +
		"and neither is a second of video: the rate was NOT imported (§10.7)",
	"input_cost_per_audio_token": "an audio token is counted as an input token here and is " +
		"charged at the input rate; the separate audio-token rate was NOT imported and this " +
		"model will be priced as if its audio tokens were text",
	"output_cost_per_audio_token": "an audio token is counted as an output token here and is " +
		"charged at the output rate; the separate audio-token rate was NOT imported",
	"input_cost_per_query": "dorang prices a request (rates.request), not a query; the rate " +
		"was NOT imported. Write it as input_cost_per_request if one query is one request",
	"input_cost_per_token_batches": "dorang has no batch rate card: a batch request is priced " +
		"at the same rates as an interactive one and this rate was NOT imported (§9.4)",
	"output_cost_per_token_batches": "dorang has no batch rate card: a batch request is priced " +
		"at the same rates as an interactive one and this rate was NOT imported (§9.4)",
	"input_cost_per_token_above_128k_tokens": "a size-dependent rate is a TIER, which the " +
		"external price catalog expresses (tiers[].up_to_input_tokens) and the inline rules " +
		"do not: the rate was NOT imported and the model is priced at its base rate for " +
		"every request size (§13.3)",
	"output_cost_per_token_above_128k_tokens": "a size-dependent rate is a TIER, which only " +
		"the external price catalog expresses: the rate was NOT imported (§13.3)",
	"input_cost_per_character_above_128k_tokens": "a size-dependent rate is a TIER, which only " +
		"the external price catalog expresses: the rate was NOT imported (§13.3)",
	"output_cost_per_character_above_128k_tokens": "a size-dependent rate is a TIER, which only " +
		"the external price catalog expresses: the rate was NOT imported (§13.3)",
}

// price imports one of the incumbent's rate keys, converting its quantity into
// dorang's on the way (see [priceFieldByKey]). The conversion is exact — a
// decimal point moves — because §8.3 forbids a price from passing through a
// float, and a rate that was rounded on import is a rate nobody can reconcile
// against the vendor's card.
func (im *importer) price(path, provider, model, key string, n *yaml.Node) {
	field, ok := priceFieldByKey[key]
	if !ok {
		im.warn(path, "%q is not a priced key and was not imported", key)
		return
	}
	component := field.component
	lit := strings.TrimSpace(scalarValue(n))
	if lit == "" {
		return
	}
	// The source file is written by a program that prints floats, so a rate can
	// arrive as "2.5e-06". That is a decimal this package refuses to STORE and a
	// decimal it can read: the conversion writes the digits out either way.
	scaled, ok := scaleDecimal(lit, field.pow10)
	if !ok {
		im.warn(path+"."+key, "price %q is not a decimal and was not imported", lit)
		return
	}
	if err := checkDecimal(scaled); err != nil {
		im.warn(path+"."+key, "price %q converts to %s for rates.%s, and %v; it was not imported",
			lit, scaled, component, err)
		return
	}
	if im.priceSeen == nil {
		im.priceSeen = map[string]string{}
	}
	// Keyed by UNIT as well as by provider and model, so an imported model that
	// quotes both a token price and a per-second price lands in two rules. A rule
	// carries one `unit`, and merging the two into one would produce a
	// configuration this package then refuses — which is worse than two rules,
	// because the operator did not write either of them.
	unit, ok := pricingComponentUnit[CanonicalComponent(component)]
	if !ok {
		im.warn(path, "%q is not a priced component and was not imported", component)
		return
	}
	ruleKey := provider + "\x00" + model + "\x00" + unit
	id, seen := im.priceSeen[ruleKey]
	if !seen {
		id = fmt.Sprintf("imported-%s-%d", provider, len(im.cfg.Pricing.Rules)+1)
		im.cfg.Pricing.Rules = append(im.cfg.Pricing.Rules, PricingRule{
			ID:    id,
			Class: PricingMarginalUsage,
			Match: PricingMatch{Provider: provider, Model: model},
			Rates: map[string]Decimal{},
		})
		im.priceSeen[ruleKey] = id
	}
	if im.priceKeyByComp == nil {
		im.priceKeyByComp = map[string]string{}
	}
	compKey := ruleKey + "\x00" + component
	for i := range im.cfg.Pricing.Rules {
		r := &im.cfg.Pricing.Rules[i]
		if r.ID != id {
			continue
		}
		if prev, dup := r.Rates[component]; dup && string(prev) != scaled {
			// Two keys, one axis. The incumbent multiplies input_cost_per_second
			// and output_cost_per_second by the SAME elapsed time and adds the
			// results, so the equivalent single rate is their sum — dorang prices
			// wall time once. Keeping the first silently would be this section's
			// own defect a second time: a plausible figure that is too low.
			im.warn(path+"."+key, "%s and %s both price %s for provider %q model %q, and dorang "+
				"has one rate for it: %s (from %s) was kept and %s was NOT added to it. If the "+
				"source charges both, write their SUM into rates.%s by hand",
				im.priceKeyByComp[compKey], key, component, provider, model,
				prev, im.priceKeyByComp[compKey], scaled, component)
			return
		}
		r.Rates[component] = Decimal(scaled)
		im.priceKeyByComp[compKey] = key
		// Counted here rather than at the conversion, so the summary counts the
		// rates that are IN the generated file and not the ones that were
		// converted on the way to being dropped.
		if field.pow10 != 0 {
			im.convertedRates++
			if im.firstConversion == "" {
				im.firstConversion = fmt.Sprintf("%s %s became rates.%s %s",
					key, lit, component, scaled)
			}
		}
	}
}

// reportRateConversions says once, at the end, that the rates were rescaled.
//
// Once and not per rate: a file with fifty models would bury every other
// warning, and the conversion is not a decision the operator has to make. What
// they do have to do is check ONE request against the vendor's card, because
// this is the number nothing else in the migration can verify for them.
func (im *importer) reportRateConversions() {
	if im.convertedRates == 0 {
		return
	}
	im.warn("model_list", "%d rate(s) were CONVERTED to dorang's quantities: a token rate is "+
		"per MILLION tokens here and a character rate is per 1,000 characters, where the source "+
		"file quotes both per one (§13.1c). For example %s. The figures in the generated file are "+
		"therefore not the figures in the source file; check one request through "+
		"POST /spend/calculate against the vendor's card",
		im.convertedRates, im.firstConversion)
}

func (im *importer) setProviderRetry(provider string, attempts int) {
	for i := range im.cfg.Providers {
		if provider == "" || im.cfg.Providers[i].Name == provider {
			im.cfg.Providers[i].Retry.MaxAttempts = attempts
		}
	}
}

func (im *importer) applyDropParams(path string, n *yaml.Node, provider string) {
	if b, ok := boolOf(n); ok {
		for i := range im.cfg.Providers {
			if provider == "" || im.cfg.Providers[i].Name == provider {
				im.cfg.Providers[i].Params.DropUnsupported = boolPtr(b)
			}
		}
		return
	}
	if n.Kind == yaml.SequenceNode {
		im.applyDropList(path, n, provider)
		return
	}
	im.warn(path, "value %q is neither a boolean nor a list and was not imported", scalarValue(n))
}

func (im *importer) applyDropList(path string, n *yaml.Node, provider string) {
	if n.Kind != yaml.SequenceNode {
		im.warn(path, "value is not a list and was not imported")
		return
	}
	var names []string
	for _, item := range n.Content {
		if v := scalarValue(item); v != "" {
			names = append(names, v)
		}
	}
	for i := range im.cfg.Providers {
		if provider == "" || im.cfg.Providers[i].Name == provider {
			im.cfg.Providers[i].Params.Drop = append(im.cfg.Providers[i].Params.Drop, names...)
		}
	}
}

func (im *importer) importLitellmSettings(n *yaml.Node) {
	m, ok := asMap(n)
	if !ok {
		im.warn("litellm_settings", "section is not a mapping and was not imported")
		return
	}
	for _, k := range m.keys {
		v := m.vals[k]
		path := "litellm_settings." + k
		switch k {
		case "drop_params":
			im.applyDropParams(path, v, "")
		case "additional_drop_params":
			im.applyDropList(path, v, "")
		case "num_retries":
			if iv, ok := intOf(v); ok {
				im.setProviderRetry("", int(iv))
			}
		case "request_timeout", "timeout":
			if d := im.duration(path, v); d > 0 {
				im.cfg.Server.RequestTimeout = d
			}
		case "json_logs":
			if b, ok := boolOf(v); ok && b {
				im.cfg.Observability.LogFormat = "json"
			}
		case "set_verbose":
			if b, ok := boolOf(v); ok && b {
				im.cfg.Observability.LogLevel = "debug"
			}
		case "max_budget", "budget_duration":
			im.warn(path, "budgets are configured per credential, key, user or team in dorang (§6.4) "+
				"and were not imported from this global setting")
		case "success_callback", "failure_callback", "callbacks", "service_callback":
			im.warn(path, "callback integrations have no equivalent: dorang meters in process and "+
				"exports through Prometheus or OTLP (§12)")
		case "cache", "cache_params":
			im.warn(path, "response caching is not part of dorang's schema and was not imported")
		default:
			im.warn(path, "setting has no equivalent in dorang's schema and was not imported")
		}
	}
}

func (im *importer) importRouterSettings(n *yaml.Node) {
	m, ok := asMap(n)
	if !ok {
		im.warn("router_settings", "section is not a mapping and was not imported")
		return
	}
	for _, k := range m.keys {
		v := m.vals[k]
		path := "router_settings." + k
		switch k {
		case "routing_strategy":
			name := strings.TrimSpace(scalarValue(v))
			chain, known := strategyByName[name]
			if !known {
				im.warn(path, "routing strategy %q is not known; the model groups were left without "+
					"an explicit strategy (§7.3)", name)
				continue
			}
			for i := range im.cfg.Models {
				if len(im.cfg.Models[i].Strategy) == 0 {
					im.cfg.Models[i].Strategy = append([]string(nil), chain...)
				}
			}
		case "num_retries":
			if iv, ok := intOf(v); ok {
				im.setProviderRetry("", int(iv))
			}
		case "timeout", "request_timeout":
			if d := im.duration(path, v); d > 0 {
				for i := range im.cfg.Providers {
					if im.cfg.Providers[i].Timeout == 0 {
						im.cfg.Providers[i].Timeout = d
					}
				}
			}
		case "model_group_alias":
			im.importAliases(path, v)
		case "fallbacks":
			im.importFallbacks(path, v, []string{CauseRateLimit, CauseQuotaExhausted, CauseUpstream5xx, CauseTimeout},
				[]string{TargetSameGroup, TargetSameClass})
		case "context_window_fallbacks", "context_window_fallback_dict":
			im.importFallbacks(path, v, []string{CauseContextWindow}, []string{TargetSameClassLarger})
		case "content_policy_fallbacks", "content_policy_fallback_dict":
			im.importFallbacks(path, v, []string{CauseContentPolicy}, []string{TargetSameClass})
		case "default_fallbacks":
			im.warn(path, "a flat default fallback list has no equivalent: dorang delegates to the "+
				"same group and then the same class (§7.6). Put the listed models in one class")
		case "allowed_fails", "allowed_fails_policy", "cooldown_time", "disable_cooldowns":
			im.warn(path, "circuit-breaker thresholds are not configurable in this schema yet; "+
				"dorang applies per-deployment breakers with a half-open probe (§7.6)")
		case "redis_host", "redis_port", "redis_password", "redis_url":
			im.warn(path, "dorang reads the Redis URL from an environment variable named by "+
				"cluster.redis_url_env, and shared state needs cluster.enabled with a shared "+
				"capacity_mode (§5.6)")
		default:
			im.warn(path, "setting has no equivalent in dorang's schema and was not imported")
		}
	}
}

func (im *importer) importAliases(path string, n *yaml.Node) {
	m, ok := asMap(n)
	if !ok {
		im.warn(path, "value is not a mapping and was not imported")
		return
	}
	for _, alias := range m.keys {
		target := scalarValue(m.vals[alias])
		if target == "" {
			im.warn(path+"."+alias, "alias has no target and was not imported")
			continue
		}
		if im.cfg.Aliases == nil {
			im.cfg.Aliases = map[string]string{}
		}
		im.cfg.Aliases[alias] = target
	}
}

// importFallbacks turns per-model fallback lists into classes. dorang delegates
// by class rather than by an explicit per-model list (§7.6), so every list
// becomes a class and the mapping is reported.
func (im *importer) importFallbacks(path string, n *yaml.Node, causes, chain []string) {
	entries := map[string][]string{}
	order := []string{}
	collect := func(primary string, alts *yaml.Node) {
		var list []string
		for _, a := range alts.Content {
			if v := scalarValue(a); v != "" {
				list = append(list, v)
			}
		}
		if _, seen := entries[primary]; !seen {
			order = append(order, primary)
		}
		entries[primary] = append(entries[primary], list...)
	}
	switch n.Kind {
	case yaml.SequenceNode:
		for _, item := range n.Content {
			m, ok := asMap(item)
			if !ok {
				im.warn(path, "entry is not a mapping and was not imported")
				continue
			}
			for _, primary := range m.keys {
				collect(primary, m.vals[primary])
			}
		}
	case yaml.MappingNode:
		m, _ := asMap(n)
		for _, primary := range m.keys {
			collect(primary, m.vals[primary])
		}
	default:
		im.warn(path, "value is neither a list nor a mapping and was not imported")
		return
	}
	if len(order) == 0 {
		return
	}

	for _, primary := range order {
		class := "imported-fallback-" + primary
		members := []string{}
		add := func(name string) {
			mi := im.modelIndex(name)
			if mi < 0 {
				im.warn(path, "fallback list for %q names model %q, which is not in model_list; "+
					"it was left out of class %q", primary, name, class)
				return
			}
			switch im.cfg.Models[mi].Class {
			case "":
				im.cfg.Models[mi].Class = class
			case class:
			default:
				im.warn(path, "model %q is already in class %q, so it could not also join %q: "+
					"a model belongs to one class (§3)", name, im.cfg.Models[mi].Class, class)
				return
			}
			if !containsString(members, name) {
				members = append(members, name)
			}
		}
		add(primary)
		for _, alt := range entries[primary] {
			add(alt)
		}
		if len(members) < 2 {
			for _, name := range members {
				if mi := im.modelIndex(name); mi >= 0 && im.cfg.Models[mi].Class == class {
					im.cfg.Models[mi].Class = ""
				}
			}
			im.warn(path, "the fallback list for %q has no usable alternative and was not imported", primary)
			continue
		}
		if im.cfg.Classes == nil {
			im.cfg.Classes = map[string][]string{}
		}
		im.cfg.Classes[class] = members
		im.warn(path, "the per-model fallback list for %q became class %q with members %s: "+
			"dorang delegates to the same group and then to the same class rather than to a "+
			"named list (§7.6)", primary, class, strings.Join(members, ", "))
	}

	if im.cfg.Fallbacks.On == nil {
		im.cfg.Fallbacks.On = map[string][]string{}
	}
	for _, cause := range causes {
		im.cfg.Fallbacks.On[cause] = append([]string(nil), chain...)
	}
}

func (im *importer) modelIndex(name string) int {
	for i := range im.cfg.Models {
		if im.cfg.Models[i].Name == name {
			return i
		}
	}
	return -1
}

func (im *importer) importGeneralSettings(n *yaml.Node) {
	m, ok := asMap(n)
	if !ok {
		im.warn("general_settings", "section is not a mapping and was not imported")
		return
	}
	for _, k := range m.keys {
		v := m.vals[k]
		path := "general_settings." + k
		switch k {
		case "master_key":
			raw := scalarValue(v)
			if env, ok := strings.CutPrefix(raw, envRefPrefix); ok && env != "" {
				im.cfg.Server.MasterKeyEnv = env
				continue
			}
			im.warn(path, "the master key is a literal. dorang reads it from the environment "+
				"variable named by server.master_key_env and never stores it in the file (§2.4); "+
				"set that variable and remove the literal")
		case "database_url":
			raw := scalarValue(v)
			im.cfg.Storage.Driver = "postgres"
			if env, ok := strings.CutPrefix(raw, envRefPrefix); ok && env != "" {
				im.cfg.Storage.Postgres.URLEnv = env
				continue
			}
			im.warn(path, "the database URL is a literal and usually carries a password. "+
				"dorang reads it from the environment variable named by storage.postgres.url_env; "+
				"set that variable and remove the literal")
		case "database_connection_pool_limit":
			if iv, ok := intOf(v); ok {
				im.cfg.Storage.Postgres.MaxConns = int(iv)
			}
		case "alerting", "alerting_threshold":
			im.warn(path, "alerting is configured under notifications in dorang (§11.5) and was "+
				"not imported")
		case "store_model_in_db":
			im.warn(path, "dorang always overlays the database on the file (§4.1); this setting "+
				"has no equivalent")
		default:
			im.warn(path, "setting has no equivalent in dorang's schema and was not imported")
		}
	}
}

// --- small YAML helpers -----------------------------------------------------

// yamlMap is a mapping node with its key order preserved, so import output is
// deterministic.
type yamlMap struct {
	keys []string
	vals map[string]*yaml.Node
}

func asMap(n *yaml.Node) (*yamlMap, bool) {
	if n == nil {
		return nil, false
	}
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	if n.Kind != yaml.MappingNode {
		return nil, false
	}
	m := &yamlMap{vals: map[string]*yaml.Node{}}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i].Value
		if _, dup := m.vals[k]; !dup {
			m.keys = append(m.keys, k)
		}
		m.vals[k] = n.Content[i+1]
	}
	return m, true
}

func (m *yamlMap) get(k string) (*yaml.Node, bool) {
	v, ok := m.vals[k]
	return v, ok
}

// entryParams returns a model_list entry's parameter block.
func entryParams(em *yamlMap) (*yamlMap, bool) {
	for _, k := range []string{"litellm_params", "params"} {
		if n, ok := em.get(k); ok {
			return asMap(n)
		}
	}
	return nil, false
}

func scalarOf(m *yamlMap, key string) string {
	if m == nil {
		return ""
	}
	if n, ok := m.get(key); ok {
		return scalarValue(n)
	}
	return ""
}

func firstScalar(m *yamlMap, keys ...string) string {
	for _, k := range keys {
		if v := scalarOf(m, k); v != "" {
			return v
		}
	}
	return ""
}

// scalarValue returns a scalar node's literal text, which keeps a price exactly
// as it was written (§8.3).
func scalarValue(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		return ""
	}
	return n.Value
}

func intOf(n *yaml.Node) (int64, bool) {
	s := strings.TrimSpace(scalarValue(n))
	if s == "" {
		return 0, false
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		return v, true
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f == float64(int64(f)) {
		return int64(f), true
	}
	return 0, false
}

func boolOf(n *yaml.Node) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(scalarValue(n))) {
	case "true", "yes", "on":
		return true, true
	case "false", "no", "off":
		return false, true
	}
	return false, false
}
