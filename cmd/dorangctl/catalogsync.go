package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// catalog sync keeps the model catalog in step with what the configured
// providers actually serve, without editing embedded data or rebuilding.
//
// # Two questions, two sources
//
// WHICH MODELS EXIST is asked of the provider itself: its OpenAI-compatible
// `/models` listing, and for Ollama `/api/show`, which answers 410 for a model
// that has been retired. Every third-party registry lags on retirements —
// models.dev, openclaw and hermes-agent all still listed Ollama models on
// 2026-09-28 that answered 410 that day — so none of them is consulted for
// this.
//
// WHAT A MODEL LOOKS LIKE — context window, output ceiling, tool support — is
// taken from the provider when it publishes it (Ollama `/api/show`), and
// otherwise from the models.dev registry, matched by the provider's endpoint
// URL rather than guessed from a name. Registry values are written as a
// citation in the entry's note: they say what models.dev says, not what
// anybody here checked.
//
// # What it will not do
//
// It never writes `verified:`. A listing or a metadata endpoint is evidence
// that a name exists, not that it answers (see catalog verify, which asks and
// is billed for it). It never deletes: a retirement is reported for a human,
// as catalog verify does, because deleting catalog rows on one HTTP answer is
// the wrong direction to be wrong in. It never overrides a value an entry
// already declares at the model layer; it only fills what is unknown there.
// And it never prints a credential: keys come from the configuration's own
// references, the same way the gateway reads them.
//
// Every request it sends is a listing or a metadata read. Nothing is billed.

const defaultModelsDevURL = "https://models.dev/api.json"

// syncStatus is what the provider said about one configured model.
type syncStatus string

const (
	statusListed   syncStatus = "listed"
	statusUnlisted syncStatus = "unlisted" // absent from the listing; nothing else could be asked
	statusAlive    syncStatus = "unlisted, still served"
	statusRetired  syncStatus = "RETIRED"
	statusAbsent   syncStatus = "absent"
	statusUnknown  syncStatus = "unknown"
)

// modelMeta is metadata for one model, with where it came from.
type modelMeta struct {
	ContextWindow   int
	MaxOutputTokens int
	Tools           bool
	Source          string // "provider" or "models.dev"
}

func (m modelMeta) empty() bool { return m.ContextWindow == 0 && m.MaxOutputTokens == 0 && !m.Tools }

// configuredModel is one upstream model the configuration routes to.
type configuredModel struct {
	Name     string
	Disabled bool // every deployment naming it is disabled
	Status   syncStatus
	Detail   string
	Newer    string
}

// providerSync is one provider's result.
type providerSync struct {
	Name, Kind, BaseURL string
	Private             bool
	ListErr             error
	Listed              []string
	Configured          []configuredModel
	New                 []string
	Meta                map[string]modelMeta
	Registry            string // matched models.dev provider id, if any
}

func (e env) catalogSync(args []string) int {
	fs := newFlagSet("catalog sync", e)
	cfgPath := fs.String("config", "", "gateway configuration whose providers to sync (default $"+EnvConfigPath+" or "+defaultConfigPath+")")
	overlays := fs.String("catalog", "", "extra model catalog files or directories, comma separated")
	only := fs.String("provider", "", "sync only these providers, comma separated")
	registry := fs.String("models-dev", defaultModelsDevURL, `models.dev registry URL or file, for metadata a provider does not publish; "off" to skip`)
	write := fs.String("write", "", "write the metadata found as a catalog overlay")
	includePrivate := fs.Bool("include-private", false, "also write overlay entries for providers on private or loopback addresses")
	timeout := fs.Duration("timeout", 30*time.Second, "per-request timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	path := firstNonEmpty(*cfgPath, os.Getenv(EnvConfigPath), defaultConfigPath)

	cfg, err := config.Load(path)
	if err != nil {
		return e.fail("%v\n(catalog sync reads the provider credentials the configuration references; "+
			"run it where the gateway runs, e.g. docker exec <container> dorangctl catalog sync)", err)
	}
	cat, err := catalog.Load(splitComma(*overlays)...)
	if err != nil {
		return e.fail("%v", err)
	}
	client := &http.Client{Timeout: *timeout}

	var reg modelsDev
	if *registry != "" && *registry != "off" {
		reg, err = loadModelsDev(client, *registry)
		if err != nil {
			// Metadata is optional; existence is the part that matters.
			fmt.Fprintf(e.stderr, "dorangctl: models.dev registry unavailable, continuing without it: %v\n", err)
		}
	}

	want := map[string]bool{}
	for _, p := range splitComma(*only) {
		want[p] = true
	}
	var results []*providerSync
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if len(want) > 0 && !want[p.Name] {
			continue
		}
		// A provider with no base_url uses its kind's catalogued endpoint, as
		// the gateway does; skipping it would leave it out of the report.
		base := p.BaseURL
		if base == "" {
			if kd, ok := cat.Kind(p.Kind); ok {
				base = kd.BaseURL
			}
		}
		if base == "" {
			fmt.Fprintf(e.stderr, "dorangctl: provider %s has no base_url and kind %s declares none; skipped\n",
				p.Name, p.Kind)
			continue
		}
		results = append(results, syncProvider(client, cfg, cat, reg, p, base))
	}
	if len(results) == 0 {
		return e.fail("no provider with a base_url to sync")
	}

	e.writeSyncReport(results)

	listedAny := false
	for _, r := range results {
		if r.ListErr == nil {
			listedAny = true
		}
	}
	if !listedAny {
		return e.fail("no provider's model listing could be read; nothing was learned")
	}

	if *write != "" {
		doc, n := buildSyncOverlay(cat, results, *includePrivate)
		if n == 0 {
			fmt.Fprintln(e.stdout, "\nnothing new to write: every listed model is already catalogued with what could be learned")
			return 0
		}
		if err := writeSyncOverlay(*write, doc); err != nil {
			return e.fail("%v", err)
		}
		fmt.Fprintf(e.stdout, "\nwrote %d entr(ies) to %s — load it with --catalog or $%s\n",
			n, *write, catalog.EnvCatalogPath)
	}
	return 0
}

// syncProvider lists one provider and classifies what the configuration routes
// to it.
func syncProvider(client *http.Client, cfg *config.Config, cat *catalog.Catalog,
	reg modelsDev, p *config.Provider, base string,
) *providerSync {
	r := &providerSync{
		Name: p.Name, Kind: p.Kind, BaseURL: strings.TrimRight(base, "/"),
		Private: isPrivateEndpoint(base), Meta: map[string]modelMeta{},
	}
	key := providerKey(cfg, p.Name)

	r.Listed, r.ListErr = fetchListing(client, r.BaseURL, key)
	listed := map[string]bool{}
	for _, m := range r.Listed {
		listed[m] = true
	}
	ollama := isOllamaKind(p.Kind)
	regProv, regModels := reg.forEndpoint(r.BaseURL)
	r.Registry = regProv

	// Configured models, in configuration order, each once.
	configured := configuredModels(cfg, p.Name)
	for _, cm := range configured {
		switch {
		case r.ListErr != nil:
			cm.Status, cm.Detail = statusUnknown, "listing failed"
		case listed[cm.Name]:
			cm.Status = statusListed
		case ollama:
			code, _, err := ollamaShow(client, r.BaseURL, key, cm.Name)
			switch {
			case err != nil:
				cm.Status, cm.Detail = statusUnknown, err.Error()
			case code == http.StatusOK:
				cm.Status, cm.Detail = statusAlive, "/api/show 200"
			case code == http.StatusGone:
				cm.Status, cm.Detail = statusRetired, "/api/show 410 Gone"
			case code == http.StatusNotFound:
				cm.Status, cm.Detail = statusAbsent, "/api/show 404"
			default:
				cm.Status, cm.Detail = statusUnknown, "/api/show "+strconv.Itoa(code)
			}
		default:
			// Absence from a listing is evidence, not proof: a listing has
			// omitted a model its provider still served. Report it; conclude
			// nothing.
			cm.Status, cm.Detail = statusUnlisted, "run catalog verify to ask"
		}
		if r.ListErr == nil {
			cm.Newer = newerListed(cm.Name, r.Listed)
		}
		r.Configured = append(r.Configured, cm)
	}

	if r.ListErr != nil {
		return r
	}
	inConfig := map[string]bool{}
	for _, cm := range configured {
		inConfig[cm.Name] = true
	}
	for _, m := range r.Listed {
		if !inConfig[m] {
			r.New = append(r.New, m)
		}
	}

	// Metadata for listed models the catalog does not already describe at the
	// model layer. Provider-published values first; the registry fills the
	// rest. Only what is missing is asked for, so a synced catalog costs no
	// requests on the next run.
	for _, m := range r.Listed {
		need := missingFields(cat, p.Kind, m)
		if len(need) == 0 {
			continue
		}
		var meta modelMeta
		// /api/show is asked only when the context window is unknown: it is the
		// field it reliably publishes. Tool support rides along, but "no tools"
		// cannot be recorded (supports_tools: false is the default), so gating
		// on it — or on an output ceiling Ollama never publishes — would re-ask
		// the same models on every run.
		if ollama && slices.Contains(need, catalog.FieldContextWindow) {
			if code, show, err := ollamaShow(client, r.BaseURL, key, m); err == nil && code == http.StatusOK {
				meta = show
				meta.Source = "provider"
			}
		}
		if rm, ok := regModels[m]; ok {
			if meta.ContextWindow == 0 && rm.Limit.Context > 0 {
				meta.ContextWindow = rm.Limit.Context
				meta.Source = joinSource(meta.Source, "models.dev")
			}
			if meta.MaxOutputTokens == 0 && rm.Limit.Output > 0 {
				meta.MaxOutputTokens = rm.Limit.Output
				meta.Source = joinSource(meta.Source, "models.dev")
			}
			if !meta.Tools && rm.ToolCall {
				meta.Tools = true
				meta.Source = joinSource(meta.Source, "models.dev")
			}
		}
		r.Meta[m] = meta
	}
	return r
}

func joinSource(have, add string) string {
	switch {
	case have == "":
		return add
	case strings.Contains(have, add):
		return have
	}
	return have + "+" + add
}

// configuredModels lists the upstream models the configuration routes to one
// provider, each once, in configuration order.
func configuredModels(cfg *config.Config, provider string) []configuredModel {
	idx := map[string]int{}
	var out []configuredModel
	for _, m := range cfg.Models {
		for _, d := range m.Deployments {
			if d.Provider != provider || d.UpstreamModel == "" {
				continue
			}
			disabled := d.Enabled != nil && !*d.Enabled
			if i, ok := idx[d.UpstreamModel]; ok {
				out[i].Disabled = out[i].Disabled && disabled
				continue
			}
			idx[d.UpstreamModel] = len(out)
			out = append(out, configuredModel{Name: d.UpstreamModel, Disabled: disabled})
		}
	}
	return out
}

// providerKey is the first resolved API key the configuration holds for the
// provider. OAuth credentials are skipped: their token is not a static bearer.
func providerKey(cfg *config.Config, provider string) string {
	for _, c := range cfg.Credentials {
		if c.Provider != provider || c.IsOAuth() {
			continue
		}
		if v, ok := c.Key.Value(); ok && v != "" {
			return v
		}
	}
	return ""
}

// fetchListing reads an OpenAI-compatible `/models` listing.
func fetchListing(client *http.Client, base, key string) ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw := readAtMost(resp.Body, 8<<20)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /models: %d %s", resp.StatusCode, firstLine(string(raw)))
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("GET /models: not an OpenAI-compatible listing: %v", err)
	}
	if doc.Data == nil {
		return nil, fmt.Errorf("GET /models: no data array; not an OpenAI-compatible listing")
	}
	out := make([]string, 0, len(doc.Data))
	for _, m := range doc.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// ollamaShow asks Ollama's `/api/show` about one model. It returns the status
// and, on 200, the context window and tool support the provider publishes.
func ollamaShow(client *http.Client, base, key, model string) (int, modelMeta, error) {
	root := strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1")
	body, _ := json.Marshal(map[string]string{"model": model})
	ctx, cancel := context.WithTimeout(context.Background(), client.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, root+"/api/show", bytes.NewReader(body))
	if err != nil {
		return 0, modelMeta{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, modelMeta{}, err
	}
	defer resp.Body.Close()
	raw := readAtMost(resp.Body, 4<<20)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, modelMeta{}, nil
	}
	var doc struct {
		ModelInfo    map[string]any `json:"model_info"`
		Capabilities []string       `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return resp.StatusCode, modelMeta{}, nil
	}
	var meta modelMeta
	for k, v := range doc.ModelInfo {
		// The architecture prefixes the key: "deepseek_v41.context_length".
		if strings.HasSuffix(k, ".context_length") {
			if f, ok := v.(float64); ok && f > 0 {
				meta.ContextWindow = int(f)
			}
		}
	}
	meta.Tools = slices.Contains(doc.Capabilities, "tools")
	return resp.StatusCode, meta, nil
}

func isOllamaKind(kind string) bool { return kind == "ollama-cloud" || kind == "ollama" }

// isPrivateEndpoint reports whether a provider sits on a loopback, private or
// link-local address, or a name that only resolves on a LAN. Such a backend's
// "models" are the operator's own local aliases — `local`, a gguf filename —
// and writing them under a public kind like `openai` would describe that kind
// wrongly for every other deployment.
func isPrivateEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	h := strings.ToLower(host)
	return h == "localhost" || strings.HasSuffix(h, ".local") ||
		strings.HasSuffix(h, ".lan") || strings.HasSuffix(h, ".internal") || !strings.Contains(h, ".")
}

// missingFields lists which of the fields sync can fill are not declared for
// this model at the model or prefix layer. A kind-wide default does not count
// as knowing the model.
func missingFields(cat *catalog.Catalog, kind, model string) []string {
	info := cat.Model(kind, model)
	if !info.ModelKnown {
		return []string{catalog.FieldContextWindow, catalog.FieldMaxOutputTokens, catalog.FieldSupportsTools}
	}
	declared := map[string]bool{}
	for _, o := range cat.Explain(kind, model) {
		if o.Layer == catalog.LayerModel || o.Layer == catalog.LayerPrefix {
			declared[o.Field] = true
		}
	}
	var out []string
	for _, f := range []string{catalog.FieldContextWindow, catalog.FieldMaxOutputTokens, catalog.FieldSupportsTools} {
		if !declared[f] {
			out = append(out, f)
		}
	}
	return out
}

// ---- newer-version suggestion ------------------------------------------------

// versionShape splits a model name into its version-free skeleton and its
// version numbers, looking only at the part before any ":" tag. Tags are sizes
// and builds ("gpt-oss:120b", "deepseek-v4-flash:0731"), not versions: treating
// them as versions would call a 120b model the successor of a 20b one.
//
//	qwen3.8-flash       -> "qwen#-flash",       [3 8]
//	deepseek-v4.1-flash -> "deepseek-v#-flash", [4 1]
//	gpt-oss:120b        -> "gpt-oss",           []
func versionShape(name string) (string, []int) {
	base, _, _ := strings.Cut(name, ":")
	var skel strings.Builder
	var ver []int
	for i := 0; i < len(base); {
		c := base[i]
		if c < '0' || c > '9' {
			skel.WriteByte(c)
			i++
			continue
		}
		// A run of digits and dots that starts with a digit is one version.
		j := i
		for j < len(base) && ((base[j] >= '0' && base[j] <= '9') || base[j] == '.') {
			j++
		}
		run := strings.TrimRight(base[i:j], ".")
		for _, part := range strings.Split(run, ".") {
			n, _ := strconv.Atoi(part)
			ver = append(ver, n)
		}
		skel.WriteByte('#')
		skel.WriteString(base[i+len(run) : j])
		i = j
	}
	return skel.String(), ver
}

func compareVersions(a, b []int) int {
	for i := 0; i < max(len(a), len(b)); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return compareInt(x, y)
		}
	}
	return 0
}

func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// newerListed returns the newest listed model of the same family with a higher
// version than name, or "". Only listed models are suggested: a successor the
// provider does not serve is not a successor here.
func newerListed(name string, listed []string) string {
	skel, ver := versionShape(name)
	if len(ver) == 0 {
		return ""
	}
	best, bestVer := "", ver
	for _, m := range listed {
		s, v := versionShape(m)
		if s != skel || len(v) == 0 {
			continue
		}
		if compareVersions(v, bestVer) > 0 || (best != "" && compareVersions(v, bestVer) == 0 && len(m) < len(best)) {
			best, bestVer = m, v
		}
	}
	return best
}

// ---- models.dev --------------------------------------------------------------

type modelsDevModel struct {
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
	ToolCall bool `json:"tool_call"`
}

type modelsDevProvider struct {
	API    string                    `json:"api"`
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDev map[string]modelsDevProvider

// forEndpoint finds the registry's provider for an endpoint by its API URL.
// Matching the URL, not the name, is what separates "alibaba-token-plan" from
// "alibaba" and "alibaba-coding-plan", which share a vendor and differ in
// every limit.
func (m modelsDev) forEndpoint(base string) (string, map[string]modelsDevModel) {
	want := normalizeEndpoint(base)
	for id, p := range m {
		if p.API != "" && normalizeEndpoint(p.API) == want {
			return id, p.Models
		}
	}
	return "", nil
}

func normalizeEndpoint(s string) string {
	s = strings.ToLower(strings.TrimRight(strings.TrimSpace(s), "/"))
	s = strings.TrimPrefix(s, "https://")
	return strings.TrimPrefix(s, "http://")
}

func loadModelsDev(client *http.Client, src string) (modelsDev, error) {
	var raw []byte
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		resp, err := client.Get(src)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: %d", src, resp.StatusCode)
		}
		raw = readAtMost(resp.Body, 64<<20)
	} else {
		b, err := os.ReadFile(src)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	var reg modelsDev
	if err := json.Unmarshal(raw, &reg); err != nil {
		return nil, fmt.Errorf("models.dev registry: %v", err)
	}
	return reg, nil
}

// ---- report ------------------------------------------------------------------

func (e env) writeSyncReport(results []*providerSync) {
	for i, r := range results {
		if i > 0 {
			fmt.Fprintln(e.stdout)
		}
		fmt.Fprintf(e.stdout, "provider %s (kind %s)  %s\n", r.Name, r.Kind, r.BaseURL)
		if r.ListErr != nil {
			fmt.Fprintf(e.stdout, "  listing failed: %s\n", truncate(collapseSpace(r.ListErr.Error()), 160))
		} else {
			line := fmt.Sprintf("  listed: %d model(s)", len(r.Listed))
			if r.Registry != "" {
				line += "; metadata registry: models.dev/" + r.Registry
			}
			if r.Private {
				line += "; private endpoint (report only)"
			}
			fmt.Fprintln(e.stdout, line)
		}
		if len(r.Configured) > 0 {
			tw := tabwriter.NewWriter(e.stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "  CONFIGURED\tSTATUS\tNOTE")
			for _, cm := range r.Configured {
				note := cm.Detail
				if cm.Newer != "" {
					note = strings.TrimPrefix(note+"; newer listed: "+cm.Newer, "; ")
				}
				name := cm.Name
				if cm.Disabled {
					name += " (disabled)"
				}
				fmt.Fprintf(tw, "  %s\t%s\t%s\n", name, cm.Status, note)
			}
			_ = tw.Flush()
		}
		if len(r.New) > 0 {
			fmt.Fprintf(e.stdout, "  listed but not configured: %s\n", strings.Join(r.New, ", "))
		}
	}

	retired := 0
	for _, r := range results {
		for _, cm := range r.Configured {
			if cm.Status == statusRetired || cm.Status == statusAbsent {
				retired++
			}
		}
	}
	if retired > 0 {
		fmt.Fprintf(e.stdout, "\n%d configured model(s) are retired or absent. Nothing is changed for them: "+
			"repoint or alias them in the configuration, and delete their catalog entries by hand.\n", retired)
	}
}

// ---- overlay -----------------------------------------------------------------

type syncOverlayModel struct {
	Kind            string `yaml:"kind"`
	Model           string `yaml:"model"`
	ContextWindow   int    `yaml:"context_window,omitempty"`
	MaxOutputTokens int    `yaml:"max_output_tokens,omitempty"`
	SupportsTools   bool   `yaml:"supports_tools,omitempty"`
	Note            string `yaml:"note,omitempty"`
}

type syncOverlayDoc struct {
	Version int                `yaml:"version"`
	Models  []syncOverlayModel `yaml:"models"`
}

// buildSyncOverlay turns the run into catalog entries: every listed model the
// catalog does not know yet, and every field it does not declare at the model
// layer that a source supplied. Private endpoints are left out unless asked.
func buildSyncOverlay(cat *catalog.Catalog, results []*providerSync, includePrivate bool) (syncOverlayDoc, int) {
	doc := syncOverlayDoc{Version: 1}
	today := time.Now().UTC().Format(time.DateOnly)
	for _, r := range results {
		if r.ListErr != nil || (r.Private && !includePrivate) {
			continue
		}
		for _, m := range r.Listed {
			known := cat.Model(r.Kind, m).ModelKnown
			need := missingFields(cat, r.Kind, m)
			meta := r.Meta[m]
			entry := syncOverlayModel{Kind: r.Kind, Model: m}
			for _, f := range need {
				switch f {
				case catalog.FieldContextWindow:
					entry.ContextWindow = meta.ContextWindow
				case catalog.FieldMaxOutputTokens:
					entry.MaxOutputTokens = meta.MaxOutputTokens
				case catalog.FieldSupportsTools:
					entry.SupportsTools = meta.Tools
				}
			}
			filled := entry.ContextWindow != 0 || entry.MaxOutputTokens != 0 || entry.SupportsTools
			if known && !filled {
				continue
			}
			var note []string
			if !known {
				note = append(note, "listed by "+r.Name+" on "+today+"; not asked (catalog verify asks)")
			}
			if filled {
				src := meta.Source
				if strings.Contains(src, "models.dev") {
					src += " (citation, not checked here)"
				}
				note = append(note, "metadata from "+src+" on "+today)
			}
			entry.Note = strings.Join(note, "; ")
			doc.Models = append(doc.Models, entry)
		}
	}
	return doc, len(doc.Models)
}

func writeSyncOverlay(path string, doc syncOverlayDoc) error {
	body, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	header := fmt.Sprintf(`# Written by dorangctl catalog sync on %s.
#
# Each entry is a model a configured provider LISTS, or a field the catalog did
# not declare for it. Listings and metadata reads establish that a name exists,
# not that it answers: nothing here is `+"`verified:`"+` (catalog verify asks).
# A note naming models.dev is a citation of that registry, not a check.
#
# Retired models are never written or removed here; the sync report names them.
# Regenerating this file replaces it — keep hand edits in a separate overlay.
`, time.Now().UTC().Format(time.RFC3339))
	return writeFileAtomic(path, append([]byte(header), body...))
}

// writeFileAtomic writes through a temporary file in the same directory and
// renames it into place, so a gateway reloading the catalog never reads half a
// file.
func writeFileAtomic(path string, data []byte) error {
	dir := "."
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		dir = path[:i+1]
	}
	f, err := os.CreateTemp(dir, ".catalog-sync-*.yaml")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := io.Copy(f, bytes.NewReader(data)); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
