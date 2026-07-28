// Package rerank is the wire adapter for the rerank surface.
//
// Rerank is the one inference protocol in DESIGN §2.1 that is not a variant of
// either family this repository already speaks, which is why it has its own
// package rather than a file inside internal/wire/openai. Three spellings are
// deployed — `/v1/rerank`, `/rerank` and `/v2/rerank` — and dorang serves all
// three on the front, because a client configured against any one of them is a
// client that must keep working (COMPATIBILITY §0).
//
// The families disagree only about the ANSWER's billing block: one reports
// search units under meta.billed_units, the other reports tokens under usage.
// That disagreement is exactly the kind §10.1's neutral form exists to absorb:
// the decoder reads whichever arrived, the neutral response carries both as
// distinct fields, and cost never sees a search unit priced as a token.
package rerank

import (
	"encoding/json"
	"errors"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/wire/wirejson"
)

// Flavor names a backend's rerank dialect.
type Flavor uint8

// The rerank dialects.
const (
	// FlavorGeneric is the shape served by the OpenAI-compatible self-hosted
	// engines (vLLM, SGLang, TEI, Infinity) at /rerank and /v1/rerank.
	FlavorGeneric Flavor = iota
	// FlavorCohere is the vendor's own /v2/rerank, which bills in search units.
	FlavorCohere
	// FlavorJina is the embedding vendor's /v1/rerank, which bills in tokens.
	FlavorJina
)

// Request is the rerank wire request. All three dialects accept it.
type Request struct {
	Model     string     `json:"model"`
	Query     string     `json:"query"`
	Documents []Document `json:"documents"`

	TopN            *int  `json:"top_n,omitempty"`
	ReturnDocuments *bool `json:"return_documents,omitempty"`
	MaxTokensPerDoc *int  `json:"max_tokens_per_doc,omitempty"`

	RankFields []string `json:"rank_fields,omitempty"`
	User       string   `json:"user,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var requestKnown = wirejson.KnownKeys("model", "query", "documents", "top_n",
	"return_documents", "max_tokens_per_doc", "rank_fields", "user")

// MarshalJSON implements [encoding/json.Marshaler].
func (r Request) MarshalJSON() ([]byte, error) {
	type alias Request
	return wirejson.MarshalWithExtra(alias(r), r.Extra, requestKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler] with case-SENSITIVE
// field matching (COMPATIBILITY 2.0): the authorization gate scanned these
// same bytes for "model" with an exact key comparison, and a decoder that
// resolves a model the gate never saw is an allow-list bypass.
func (r *Request) UnmarshalJSON(b []byte) error {
	type alias Request
	var a alias
	b = canonical.StrictBytes(b, &a)
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := wirejson.SplitExtra(b, requestKnown)
	if err != nil {
		return err
	}
	*r = Request(a)
	r.Extra = extra
	return nil
}

// Document is one corpus entry: a bare string or a structured object.
type Document canonical.RerankDocument

// MarshalJSON implements [encoding/json.Marshaler].
func (d Document) MarshalJSON() ([]byte, error) {
	if len(d.Fields) > 0 {
		return d.Fields, nil
	}
	return wirejson.Marshal(d.Text)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
func (d *Document) UnmarshalJSON(b []byte) error {
	b = wirejson.TrimSpace(b)
	*d = Document{}
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	switch b[0] {
	case '"':
		return json.Unmarshal(b, &d.Text)
	case '{':
		// The structured form is kept raw so member order survives, and its
		// "text" member is lifted out so a backend that only takes strings has
		// something to send.
		var probe struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(b, &probe); err != nil {
			return err
		}
		d.Fields = append(json.RawMessage(nil), b...)
		d.Text = probe.Text
		return nil
	default:
		return errors.New("rerank: a document must be a string or an object")
	}
}

// Result is one scored document on the wire.
type Result struct {
	Index          int       `json:"index"`
	RelevanceScore float64   `json:"relevance_score"`
	Document       *Document `json:"document,omitempty"`
}

// Response is the rerank wire response.
//
// It is the union of the two vendor shapes rather than one of them, because
// both are read in the field: clients written against one vendor look at
// meta.billed_units and clients written against the other look at usage. A
// block is emitted only when the backend actually reported it — a zeroed
// billing block is worse than an absent one, since it reads as "this was free".
type Response struct {
	ID      string   `json:"id,omitempty"`
	Model   string   `json:"model,omitempty"`
	Results []Result `json:"results"`

	Usage *Usage `json:"usage,omitempty"`
	Meta  *Meta  `json:"meta,omitempty"`
}

// Usage is the token-billing block.
type Usage struct {
	TotalTokens  int `json:"total_tokens"`
	PromptTokens int `json:"prompt_tokens,omitempty"`
}

// Meta is the search-unit billing block.
type Meta struct {
	BilledUnits *BilledUnits `json:"billed_units,omitempty"`
}

// BilledUnits is what the vendor charged for.
type BilledUnits struct {
	SearchUnits int `json:"search_units"`
}

// ---------------------------------------------------------------------------
// Decode
// ---------------------------------------------------------------------------

// DecodeRequest parses rerank request bytes into the neutral form.
func DecodeRequest(b []byte) (*canonical.RerankRequest, error) {
	var w Request
	if err := canonical.StrictUnmarshal(b, &w); err != nil {
		return nil, err
	}
	return RequestToCanonical(&w)
}

// RequestToCanonical converts a decoded wire request.
func RequestToCanonical(w *Request) (*canonical.RerankRequest, error) {
	if w == nil {
		return nil, errors.New("rerank: nil request")
	}
	out := &canonical.RerankRequest{
		// The model name is copied verbatim; nothing splits it (DESIGN §2.1).
		Model:           w.Model,
		Query:           w.Query,
		TopN:            w.TopN,
		ReturnDocuments: w.ReturnDocuments,
		MaxTokensPerDoc: w.MaxTokensPerDoc,
		RankFields:      w.RankFields,
		User:            w.User,
		Extra:           w.Extra,
	}
	if w.Documents != nil {
		out.Documents = make([]canonical.RerankDocument, len(w.Documents))
		for i := range w.Documents {
			out.Documents[i] = canonical.RerankDocument(w.Documents[i])
		}
	}
	return out, nil
}

// DecodeResponse parses a rerank answer into the neutral form.
//
// json.Unmarshal and not the strict filter: these bytes are a backend's, not a
// caller's, and nothing is authorized against a response.
func DecodeResponse(b []byte, flavor Flavor, model string) (*canonical.RerankResponse, error) {
	var w Response
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	return ResponseToCanonical(&w, flavor, model)
}

// ResponseToCanonical converts a decoded wire response.
//
// model is the CLIENT-FACING name and overrides whatever the backend reported
// (DESIGN §7.2): the body never carries the upstream id.
func ResponseToCanonical(w *Response, flavor Flavor, model string) (*canonical.RerankResponse, error) {
	if w == nil {
		return nil, errors.New("rerank: nil response")
	}
	out := &canonical.RerankResponse{ID: w.ID, Model: w.Model}
	if model != "" {
		out.Model = model
	}
	if w.Usage != nil && (w.Usage.TotalTokens > 0 || w.Usage.PromptTokens > 0) {
		// Inclusive input accounting, like every other surface (DESIGN §10.7).
		// A rerank backend reports one number; it is the whole prompt, so it is
		// InputTokens and the total derives from it rather than the other way
		// round.
		in := w.Usage.PromptTokens
		if in == 0 {
			in = w.Usage.TotalTokens
		}
		out.Usage = &canonical.Usage{InputTokens: in}
	}
	if w.Meta != nil && w.Meta.BilledUnits != nil {
		out.SearchUnits = w.Meta.BilledUnits.SearchUnits
	}
	_ = flavor // the dialects differ in which block they fill, not in how it reads
	out.Results = make([]canonical.RerankResult, 0, len(w.Results))
	for i := range w.Results {
		r := &w.Results[i]
		cr := canonical.RerankResult{Index: r.Index, RelevanceScore: r.RelevanceScore}
		if r.Document != nil {
			d := canonical.RerankDocument(*r.Document)
			cr.Document = &d
		}
		out.Results = append(out.Results, cr)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Encode
// ---------------------------------------------------------------------------

// EncodeOptions controls a canonical -> rerank conversion.
type EncodeOptions struct {
	// Model overrides the model field, which is how the real upstream id
	// reaches the backend while the client-facing name stays in the response
	// (DESIGN §7.2).
	Model  string
	Flavor Flavor
}

// MarshalRequest encodes a neutral rerank request as wire bytes.
func MarshalRequest(req *canonical.RerankRequest, opt *EncodeOptions) ([]byte, error) {
	w, err := EncodeRequest(req, opt)
	if err != nil {
		return nil, err
	}
	return wirejson.Marshal(w)
}

// EncodeRequest converts a neutral request to the wire shape.
func EncodeRequest(req *canonical.RerankRequest, opt *EncodeOptions) (*Request, error) {
	if req == nil {
		return nil, errors.New("rerank: nil request")
	}
	out := &Request{
		Model:           req.Model,
		Query:           req.Query,
		TopN:            req.TopN,
		ReturnDocuments: req.ReturnDocuments,
		MaxTokensPerDoc: req.MaxTokensPerDoc,
		RankFields:      req.RankFields,
		User:            req.User,
		Extra:           req.Extra,
	}
	if opt != nil && opt.Model != "" {
		out.Model = opt.Model
	}
	out.Documents = make([]Document, len(req.Documents))
	for i := range req.Documents {
		d := Document(req.Documents[i])
		if opt != nil && opt.Flavor == FlavorCohere && len(d.Fields) > 0 && len(req.RankFields) == 0 {
			// The v2 surface takes strings only unless rank_fields names the
			// members to rank on. Sending an object without them is a 400 the
			// caller has never seen, so the text form is used and the structure
			// is dropped where the caller cannot have been relying on it.
			d.Fields = nil
		}
		out.Documents[i] = d
	}
	return out, nil
}

// MarshalResponse encodes a neutral rerank response as client-facing bytes.
func MarshalResponse(r *canonical.RerankResponse) ([]byte, error) {
	w, err := EncodeResponse(r)
	if err != nil {
		return nil, err
	}
	return wirejson.Marshal(w)
}

// EncodeResponse converts a neutral response to the wire shape.
func EncodeResponse(r *canonical.RerankResponse) (*Response, error) {
	if r == nil {
		return nil, errors.New("rerank: nil response")
	}
	out := &Response{ID: r.ID, Model: r.Model}
	out.Results = make([]Result, 0, len(r.Results))
	for i := range r.Results {
		res := &r.Results[i]
		wr := Result{Index: res.Index, RelevanceScore: res.RelevanceScore}
		if res.Document != nil {
			d := Document(*res.Document)
			wr.Document = &d
		}
		out.Results = append(out.Results, wr)
	}
	if r.Usage != nil && r.Usage.InputTokens > 0 {
		out.Usage = &Usage{TotalTokens: r.Usage.InputTokens, PromptTokens: r.Usage.InputTokens}
	}
	if r.SearchUnits > 0 {
		out.Meta = &Meta{BilledUnits: &BilledUnits{SearchUnits: r.SearchUnits}}
	}
	return out, nil
}
