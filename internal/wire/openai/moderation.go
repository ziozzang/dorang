package openai

import (
	"encoding/json"
	"sort"

	"github.com/ziozzang/dorang/internal/canonical"
)

// The moderations surface: POST /v1/moderations.
//
// The response is three parallel objects keyed by category name — categories,
// category_scores, category_applied_input_types — and the category set is the
// backend's, not a fixed enumeration. dorang does not model the names: a
// gateway that knows the list is a gateway that silently drops the next
// category a provider adds, and clients read this object by iterating it.

// ObjectModerationList is the object value of a moderation response.
const ObjectModerationList = "list"

// ModerationRequest is a moderations request.
type ModerationRequest struct {
	// Input is a string, an array of strings, or an array of typed parts.
	Input ModerationInput `json:"input"`
	Model string          `json:"model,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var moderationRequestKnown = knownKeys("input", "model")

// MarshalJSON implements [encoding/json.Marshaler].
func (r ModerationRequest) MarshalJSON() ([]byte, error) {
	type alias ModerationRequest
	return marshalWithExtra(alias(r), r.Extra, moderationRequestKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler] with case-SENSITIVE
// field matching (COMPATIBILITY 2.0).
func (r *ModerationRequest) UnmarshalJSON(b []byte) error {
	type alias ModerationRequest
	var a alias
	b = strictBytes(b, &a)
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtra(b, moderationRequestKnown)
	if err != nil {
		return err
	}
	*r = ModerationRequest(a)
	r.Extra = extra
	return nil
}

// ModerationInput is the string-or-array input field.
type ModerationInput struct {
	Items []canonical.ModerationInput
	// String records that the wire carried a bare string, so a same-protocol
	// crossing re-emits the form the caller sent.
	String bool
}

// MarshalJSON implements [encoding/json.Marshaler].
func (m ModerationInput) MarshalJSON() ([]byte, error) {
	if m.String && len(m.Items) == 1 {
		return Marshal(m.Items[0].Text)
	}
	allText := true
	for i := range m.Items {
		if m.Items[i].Kind != canonical.ModerationText {
			allText = false
			break
		}
	}
	if allText {
		texts := make([]string, len(m.Items))
		for i := range m.Items {
			texts[i] = m.Items[i].Text
		}
		return Marshal(texts)
	}
	parts := make([]moderationPart, len(m.Items))
	for i := range m.Items {
		it := &m.Items[i]
		switch it.Kind {
		case canonical.ModerationImage:
			parts[i] = moderationPart{Type: string(canonical.ModerationImage)}
			if it.Source != nil {
				parts[i].ImageURL = &ImageURL{URL: it.Source.DataURL()}
			}
		default:
			parts[i] = moderationPart{Type: PartText, Text: it.Text}
		}
	}
	return Marshal(parts)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
func (m *ModerationInput) UnmarshalJSON(b []byte) error {
	b = trimSpace(b)
	*m = ModerationInput{}
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	switch b[0] {
	case '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		m.String = true
		m.Items = []canonical.ModerationInput{{Kind: canonical.ModerationText, Text: s}}
		return nil
	case '[':
		var raw []json.RawMessage
		if err := json.Unmarshal(b, &raw); err != nil {
			return err
		}
		m.Items = make([]canonical.ModerationInput, 0, len(raw))
		for _, el := range raw {
			el = trimSpace(el)
			if len(el) == 0 {
				continue
			}
			if el[0] == '"' {
				var s string
				if err := json.Unmarshal(el, &s); err != nil {
					return err
				}
				m.Items = append(m.Items, canonical.ModerationInput{
					Kind: canonical.ModerationText, Text: s,
				})
				continue
			}
			var p moderationPart
			if err := json.Unmarshal(el, &p); err != nil {
				return err
			}
			in := canonical.ModerationInput{Kind: canonical.ModerationInputKind(p.Type), Text: p.Text}
			if p.Type == string(canonical.ModerationImage) && p.ImageURL != nil {
				b := canonical.ImageURLBlock(p.ImageURL.URL)
				in.Source = b.Source
			}
			m.Items = append(m.Items, in)
		}
		return nil
	default:
		return errorString("openai: moderation input must be a string or an array")
	}
}

type moderationPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ModerationResponse is a moderations answer.
type ModerationResponse struct {
	ID      string             `json:"id"`
	Model   string             `json:"model"`
	Results []ModerationResult `json:"results"`
}

// ModerationResult is the verdict for one input.
//
// The three category objects are ordered maps rendered from a slice, so the
// backend's ordering survives the crossing. encoding/json would sort a real map
// alphabetically and quietly reorder every response.
type ModerationResult struct {
	Flagged           bool              `json:"flagged"`
	Categories        moderationFlags   `json:"categories"`
	CategoryScores    moderationScores  `json:"category_scores"`
	CategoryApplied   moderationApplied `json:"category_applied_input_types,omitempty"`
	categoriesBacking []canonical.ModerationCategory
}

type moderationFlags struct {
	cats []canonical.ModerationCategory
}
type moderationScores struct {
	cats []canonical.ModerationCategory
}
type moderationApplied struct {
	cats []canonical.ModerationCategory
}

// MarshalJSON implements [encoding/json.Marshaler].
func (f moderationFlags) MarshalJSON() ([]byte, error) {
	return appendCategoryObject(f.cats, func(c *canonical.ModerationCategory) (any, bool) {
		return c.Flagged, true
	})
}

// MarshalJSON implements [encoding/json.Marshaler].
func (s moderationScores) MarshalJSON() ([]byte, error) {
	return appendCategoryObject(s.cats, func(c *canonical.ModerationCategory) (any, bool) {
		return c.Score, c.Scored
	})
}

// MarshalJSON implements [encoding/json.Marshaler].
func (a moderationApplied) MarshalJSON() ([]byte, error) {
	return appendCategoryObject(a.cats, func(c *canonical.ModerationCategory) (any, bool) {
		return c.Applied, len(c.Applied) > 0
	})
}

func appendCategoryObject(cats []canonical.ModerationCategory, pick func(*canonical.ModerationCategory) (any, bool)) ([]byte, error) {
	out := []byte{'{'}
	first := true
	for i := range cats {
		v, ok := pick(&cats[i])
		if !ok {
			continue
		}
		if !first {
			out = append(out, ',')
		}
		first = false
		k, err := Marshal(cats[i].Name)
		if err != nil {
			return nil, err
		}
		out = append(out, k...)
		out = append(out, ':')
		vb, err := Marshal(v)
		if err != nil {
			return nil, err
		}
		out = append(out, vb...)
	}
	return append(out, '}'), nil
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
//
// The three parallel objects are merged into one ordered category list. Go maps
// have no order, so the names are sorted: the backend's own ordering is not
// recoverable from a decoded map and a stable order beats a random one, which
// is what ranging over a map would give.
func (r *ModerationResult) UnmarshalJSON(b []byte) error {
	var raw struct {
		Flagged         bool                `json:"flagged"`
		Categories      map[string]bool     `json:"categories"`
		CategoryScores  map[string]float64  `json:"category_scores"`
		CategoryApplied map[string][]string `json:"category_applied_input_types"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	names := make([]string, 0, len(raw.Categories))
	seen := make(map[string]struct{}, len(raw.Categories))
	add := func(n string) {
		if _, dup := seen[n]; dup {
			return
		}
		seen[n] = struct{}{}
		names = append(names, n)
	}
	for n := range raw.Categories {
		add(n)
	}
	for n := range raw.CategoryScores {
		add(n)
	}
	for n := range raw.CategoryApplied {
		add(n)
	}
	sort.Strings(names)

	cats := make([]canonical.ModerationCategory, 0, len(names))
	for _, n := range names {
		c := canonical.ModerationCategory{Name: n, Flagged: raw.Categories[n]}
		if s, ok := raw.CategoryScores[n]; ok {
			c.Score, c.Scored = s, true
		}
		c.Applied = raw.CategoryApplied[n]
		cats = append(cats, c)
	}
	*r = ModerationResult{Flagged: raw.Flagged}
	r.setCategories(cats)
	return nil
}

func (r *ModerationResult) setCategories(cats []canonical.ModerationCategory) {
	r.categoriesBacking = cats
	r.Categories = moderationFlags{cats: cats}
	r.CategoryScores = moderationScores{cats: cats}
	r.CategoryApplied = moderationApplied{cats: cats}
}

// ---------------------------------------------------------------------------
// Conversion
// ---------------------------------------------------------------------------

// DecodeModerationRequest parses moderation request bytes into the neutral form.
func DecodeModerationRequest(b []byte) (*canonical.ModerationRequest, error) {
	var w ModerationRequest
	if err := strictUnmarshal(b, &w); err != nil {
		return nil, err
	}
	return &canonical.ModerationRequest{
		Model:      w.Model,
		Inputs:     w.Input.Items,
		StringForm: w.Input.String,
		Extra:      w.Extra,
	}, nil
}

// MarshalModerationRequest encodes a neutral moderation request.
func MarshalModerationRequest(req *canonical.ModerationRequest, model string) ([]byte, error) {
	if req == nil {
		return nil, errNilRequest
	}
	w := ModerationRequest{
		Model: req.Model,
		Input: ModerationInput{Items: req.Inputs, String: req.StringForm},
		Extra: req.Extra,
	}
	if model != "" {
		w.Model = model
	}
	return Marshal(w)
}

// DecodeModerationResponse parses a moderation answer into the neutral form.
func DecodeModerationResponse(b []byte, model string) (*canonical.ModerationResponse, error) {
	var w ModerationResponse
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	out := &canonical.ModerationResponse{ID: w.ID, Model: w.Model}
	if model != "" {
		// DESIGN §7.2: the body carries the name the client asked for.
		out.Model = model
	}
	out.Results = make([]canonical.ModerationResult, 0, len(w.Results))
	for i := range w.Results {
		out.Results = append(out.Results, canonical.ModerationResult{
			Flagged:    w.Results[i].Flagged,
			Categories: w.Results[i].categoriesBacking,
		})
	}
	return out, nil
}

// MarshalModerationResponse encodes a neutral moderation answer.
func MarshalModerationResponse(r *canonical.ModerationResponse) ([]byte, error) {
	if r == nil {
		return nil, errorString("openai: nil moderation response")
	}
	w := ModerationResponse{ID: r.ID, Model: r.Model}
	w.Results = make([]ModerationResult, len(r.Results))
	for i := range r.Results {
		w.Results[i].Flagged = r.Results[i].Flagged
		w.Results[i].setCategories(r.Results[i].Categories)
	}
	return Marshal(w)
}
