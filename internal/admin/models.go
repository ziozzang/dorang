package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
)

// deploymentView is a deployments row on the wire.
//
// The incumbent's /model/* surface calls a deployment a "model" and nests its
// routing parameters under `litellm_params`; dorang's own vocabulary separates
// the model *group* a client asks for from the *deployment* that serves it
// (§7.2). Both names are present: `model_name` for the group, `id` for the
// deployment, and the routing parameters under `dorang_params` — plus a
// `litellm_params` alias so that an existing script reading that key still
// finds the same object.
type deploymentView struct {
	ID           string           `json:"id"`
	ModelName    string           `json:"model_name"`
	Params       deploymentParams `json:"dorang_params"`
	ParamsLegacy deploymentParams `json:"litellm_params"`
	Info         deploymentInfo   `json:"model_info"`
}

type deploymentParams struct {
	Provider        string   `json:"provider"`
	UpstreamModel   string   `json:"model"`
	CredentialIDs   []string `json:"credential_ids"`
	Weight          int      `json:"weight"`
	Priority        int      `json:"priority"`
	RPMLimit        *int64   `json:"rpm"`
	TPMLimit        *int64   `json:"tpm"`
	MaxParallel     *int64   `json:"max_parallel_requests"`
	TimeoutMS       *int64   `json:"timeout_ms"`
	StreamTimeoutMS *int64   `json:"stream_timeout_ms"`
	// Extra is the deployment's parameter overlay, verbatim.
	Extra json.RawMessage `json:"params,omitempty"`
}

type deploymentInfo struct {
	ID        string `json:"id"`
	Enabled   bool   `json:"enabled"`
	CreatedAt Stamp  `json:"created_at"`
	UpdatedAt Stamp  `json:"updated_at"`
}

func viewDeployment(d *Deployment) deploymentView {
	p := deploymentParams{
		Provider:        d.ProviderID,
		UpstreamModel:   d.UpstreamModel,
		CredentialIDs:   orEmpty(d.CredentialIDs),
		Weight:          d.Weight,
		Priority:        d.Priority,
		RPMLimit:        d.RPMLimit,
		TPMLimit:        d.TPMLimit,
		MaxParallel:     d.MaxParallel,
		TimeoutMS:       d.TimeoutMS,
		StreamTimeoutMS: d.StreamTimeoutMS,
	}
	if s := strings.TrimSpace(d.Params); s != "" && s != "{}" {
		p.Extra = json.RawMessage(s)
	}
	return deploymentView{
		ID:           d.ID,
		ModelName:    d.ModelGroup,
		Params:       p,
		ParamsLegacy: p,
		Info: deploymentInfo{
			ID:        d.ID,
			Enabled:   d.Enabled,
			CreatedAt: Stamp(d.CreatedAt),
			UpdatedAt: Stamp(d.UpdatedAt),
		},
	}
}

// deploymentSpec is the mutable surface of a deployment.
type deploymentSpec struct {
	ID        *string `json:"id"`
	ModelName *string `json:"model_name"`

	Params       *deploymentSpecParams `json:"dorang_params"`
	ParamsLegacy *deploymentSpecParams `json:"litellm_params"`

	Enabled *bool `json:"enabled"`
}

type deploymentSpecParams struct {
	Provider        *string          `json:"provider"`
	UpstreamModel   *string          `json:"model"`
	CredentialIDs   *[]string        `json:"credential_ids"`
	Weight          *int             `json:"weight"`
	Priority        *int             `json:"priority"`
	RPMLimit        *int64           `json:"rpm"`
	TPMLimit        *int64           `json:"tpm"`
	MaxParallel     *int64           `json:"max_parallel_requests"`
	TimeoutMS       *int64           `json:"timeout_ms"`
	StreamTimeoutMS *int64           `json:"stream_timeout_ms"`
	Extra           *json.RawMessage `json:"params"`
}

// params returns whichever parameter block the caller sent, preferring dorang's
// own name.
func (s *deploymentSpec) params() *deploymentSpecParams {
	if s.Params != nil {
		return s.Params
	}
	return s.ParamsLegacy
}

func (s *deploymentSpec) apply(d *Deployment) error {
	setString(&d.ModelGroup, s.ModelName)
	if s.Enabled != nil {
		d.Enabled = *s.Enabled
	}
	p := s.params()
	if p == nil {
		return nil
	}
	setString(&d.ProviderID, p.Provider)
	setString(&d.UpstreamModel, p.UpstreamModel)
	setStrings(&d.CredentialIDs, p.CredentialIDs)
	if p.Weight != nil {
		if *p.Weight < 0 {
			return badRequest("weight must not be negative").withParam("weight")
		}
		d.Weight = *p.Weight
	}
	if p.Priority != nil {
		d.Priority = *p.Priority
	}
	if p.RPMLimit != nil {
		d.RPMLimit = p.RPMLimit
	}
	if p.TPMLimit != nil {
		d.TPMLimit = p.TPMLimit
	}
	if p.MaxParallel != nil {
		d.MaxParallel = p.MaxParallel
	}
	if p.TimeoutMS != nil {
		d.TimeoutMS = p.TimeoutMS
	}
	if p.StreamTimeoutMS != nil {
		d.StreamTimeoutMS = p.StreamTimeoutMS
	}
	if p.Extra != nil {
		raw := strings.TrimSpace(string(*p.Extra))
		if raw == "" || raw == "null" {
			raw = "{}"
		}
		if !validJSONObject(raw) {
			return badRequest("params must be a JSON object").withParam("params")
		}
		d.Params = raw
	}
	return nil
}

// POST /model/new
func (c *call) modelNew() error {
	reg, err := c.a.models()
	if err != nil {
		return err
	}
	if err := c.requireAudit(); err != nil {
		return err
	}
	var spec deploymentSpec
	if err := decodeBody(c.w, c.r, &spec); err != nil {
		return err
	}
	now := c.a.now().UTC()
	d := &Deployment{
		ID:        c.a.cfg.NewID(),
		Weight:    1,
		Params:    "{}",
		Enabled:   true,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if spec.ID != nil && strings.TrimSpace(*spec.ID) != "" {
		d.ID = strings.TrimSpace(*spec.ID)
	}
	if err := spec.apply(d); err != nil {
		return err
	}
	if d.ModelGroup == "" {
		return badRequest("model_name is required").withParam("model_name")
	}
	if d.ProviderID == "" {
		return badRequest("dorang_params.provider is required").withParam("provider")
	}
	if d.UpstreamModel == "" {
		return badRequest("dorang_params.model is required").withParam("model")
	}
	if err := reg.CreateDeployment(c.ctx(), d); err != nil {
		if errors.Is(err, ErrConflict) {
			return newFault(http.StatusConflict, CodeConflict, typeInvalidRequest,
				"a deployment with id %q already exists", d.ID)
		}
		return err
	}
	view := viewDeployment(d)
	if err := c.recordAudit("model.new", "deployment", d.ID, nil, view); err != nil {
		return err
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{"model": view})
	return nil
}

// GET|POST /model/info
//
// With no id it lists every deployment, which is what the incumbent's
// /model/info does and what the UI needs; with one it returns that deployment.
func (c *call) modelInfo() error {
	reg, err := c.a.models()
	if err != nil {
		return err
	}
	var ref struct {
		ID      string `json:"id"`
		ModelID string `json:"model_id"`
	}
	if err := decodeOptionalBody(c.w, c.r, &ref); err != nil {
		return err
	}
	id := firstNonEmpty(ref.ID, ref.ModelID, queryString(c.r, "id", "model_id"))
	if id != "" {
		d, err := reg.GetDeployment(c.ctx(), id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return notFound("deployment", id)
			}
			return err
		}
		writeJSON(c.w, c.r, http.StatusOK, map[string]any{"data": []deploymentView{viewDeployment(d)}})
		return nil
	}
	list, err := reg.ListDeployments(c.ctx())
	if err != nil {
		return err
	}
	out := make([]deploymentView, 0, len(list))
	for _, d := range list {
		out = append(out, viewDeployment(d))
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{"data": out})
	return nil
}

// POST /model/update
func (c *call) modelUpdate() error {
	reg, err := c.a.models()
	if err != nil {
		return err
	}
	if err := c.requireAudit(); err != nil {
		return err
	}
	var spec deploymentSpec
	if err := decodeBody(c.w, c.r, &spec); err != nil {
		return err
	}
	if spec.ID == nil || strings.TrimSpace(*spec.ID) == "" {
		return badRequest("id is required").withParam("id")
	}
	id := strings.TrimSpace(*spec.ID)
	d, err := reg.GetDeployment(c.ctx(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return notFound("deployment", id)
		}
		return err
	}
	before := viewDeployment(d)
	updated := *d
	if err := spec.apply(&updated); err != nil {
		return err
	}
	updated.UpdatedAt = c.a.now().UTC()
	if err := reg.UpdateDeployment(c.ctx(), &updated); err != nil {
		return err
	}
	after := viewDeployment(&updated)
	if err := c.recordAudit("model.update", "deployment", id, before, after); err != nil {
		return err
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{"model": after})
	return nil
}

// POST /model/delete
func (c *call) modelDelete() error {
	reg, err := c.a.models()
	if err != nil {
		return err
	}
	if err := c.requireAudit(); err != nil {
		return err
	}
	var body struct {
		ID      string `json:"id"`
		ModelID string `json:"model_id"`
	}
	if err := decodeBody(c.w, c.r, &body); err != nil {
		return err
	}
	id := firstNonEmpty(body.ID, body.ModelID)
	if id == "" {
		return badRequest("id is required").withParam("id")
	}
	d, err := reg.GetDeployment(c.ctx(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return notFound("deployment", id)
		}
		return err
	}
	before := viewDeployment(d)
	if err := reg.DeleteDeployment(c.ctx(), id); err != nil {
		return err
	}
	if err := c.recordAudit("model.delete", "deployment", id, before, nil); err != nil {
		return err
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{"deleted": true, "id": id})
	return nil
}

// modelGroupView is one client-facing model group.
type modelGroupView struct {
	ModelGroup     string   `json:"model_group"`
	Providers      []string `json:"providers"`
	UpstreamModels []string `json:"upstream_models"`
	Aliases        []string `json:"aliases"`
	Deployments    int      `json:"deployments"`
	// EnabledDeployments is the count that can actually serve traffic. A group
	// whose every deployment is disabled is routable in configuration and dead
	// in practice, and one number that hides the other is how that goes
	// unnoticed.
	EnabledDeployments int `json:"enabled_deployments"`
}

// GET|POST /model_group/info
func (c *call) modelGroupInfo() error {
	reg, err := c.a.models()
	if err != nil {
		return err
	}
	list, err := reg.ListDeployments(c.ctx())
	if err != nil {
		return err
	}
	aliases, err := reg.ListAliases(c.ctx())
	if err != nil {
		return err
	}
	groups := buildGroups(list, aliases)
	if want := queryString(c.r, "model_group", "name"); want != "" {
		for _, g := range groups {
			if g.ModelGroup == want {
				writeJSON(c.w, c.r, http.StatusOK, map[string]any{"data": []modelGroupView{g}})
				return nil
			}
		}
		return notFound("model group", want)
	}
	writeJSON(c.w, c.r, http.StatusOK, map[string]any{"data": groups})
	return nil
}

// buildGroups folds deployments and aliases into the client-facing view.
//
// It is computed here rather than asked of the store because it is a pure
// function of two lists the store already returns, and a third query that can
// disagree with the first two is a bug waiting for a race.
func buildGroups(list []*Deployment, aliases []Alias) []modelGroupView {
	type acc struct {
		providers map[string]bool
		upstream  map[string]bool
		aliases   []string
		total     int
		enabled   int
	}
	byGroup := map[string]*acc{}
	get := func(g string) *acc {
		a := byGroup[g]
		if a == nil {
			a = &acc{providers: map[string]bool{}, upstream: map[string]bool{}}
			byGroup[g] = a
		}
		return a
	}
	for _, d := range list {
		a := get(d.ModelGroup)
		a.total++
		if d.Enabled {
			a.enabled++
		}
		if d.ProviderID != "" {
			a.providers[d.ProviderID] = true
		}
		if d.UpstreamModel != "" {
			a.upstream[d.UpstreamModel] = true
		}
	}
	for _, al := range aliases {
		a := get(al.ModelGroup)
		a.aliases = append(a.aliases, al.Alias)
	}
	out := make([]modelGroupView, 0, len(byGroup))
	for name, a := range byGroup {
		sort.Strings(a.aliases)
		out = append(out, modelGroupView{
			ModelGroup:         name,
			Providers:          sortedKeys(a.providers),
			UpstreamModels:     sortedKeys(a.upstream),
			Aliases:            orEmpty(a.aliases),
			Deployments:        a.total,
			EnabledDeployments: a.enabled,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModelGroup < out[j].ModelGroup })
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validJSONObject reports whether s is a JSON object. Metadata and parameter
// overlays are stored verbatim, so they are checked at the boundary rather than
// discovered to be malformed by whatever reads them next.
func validJSONObject(s string) bool {
	var v map[string]json.RawMessage
	return json.Unmarshal([]byte(s), &v) == nil
}
