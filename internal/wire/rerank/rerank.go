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

// AppendJSON implements [wirejson.Appender]. It is [Request.MarshalJSON]
// writing into the caller's buffer; the two are held byte-identical by
// FuzzAppendAgreesWithMarshal.
func (r Request) AppendJSON(dst []byte) ([]byte, error) {
	type alias Request
	return wirejson.AppendWithExtra(dst, alias(r), r.Extra, requestKnown)
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

// AppendJSON implements [wirejson.Appender].
//
// The structured form goes through json.RawMessage rather than being appended
// raw, because that is what MarshalJSON's caller does to it: encoding/json
// COMPACTS a Marshaler's result, so a document that arrived with whitespace in
// it goes out folded flat, and an appender that spliced the bytes verbatim
// would be a second serializer rather than the same one.
func (d Document) AppendJSON(dst []byte) ([]byte, error) {
	if len(d.Fields) > 0 {
		return wirejson.Append(dst, json.RawMessage(d.Fields))
	}
	return wirejson.AppendString(dst, d.Text), nil
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

	Extra map[string]json.RawMessage `json:"-"`
}

var responseKnown = wirejson.KnownKeys("id", "model", "results", "usage", "meta")

// MarshalJSON implements [encoding/json.Marshaler].
func (r Response) MarshalJSON() ([]byte, error) {
	type alias Response
	return wirejson.MarshalWithExtra(alias(r), r.Extra, responseKnown)
}

// AppendJSON implements [wirejson.Appender]. It is [Response.MarshalJSON]
// writing into the caller's buffer; the two are held byte-identical by
// FuzzAppendAgreesWithMarshal.
func (r Response) AppendJSON(dst []byte) ([]byte, error) {
	type alias Response
	return wirejson.AppendWithExtra(dst, alias(r), r.Extra, responseKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler]. Response-only, so
// json.Unmarshal rather than the strict filter: nothing is authorized against
// an answer.
func (r *Response) UnmarshalJSON(b []byte) error {
	type alias Response
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := wirejson.SplitExtraFold(b, responseKnown)
	if err != nil {
		return err
	}
	*r = Response(a)
	r.Extra = extra
	return nil
}

// Usage is the token-billing block.
//
// Four spellings reach this one block, because "rerank" is served by three
// vendor dialects and by every OpenAI-compatible engine that bolted the route
// on. prompt_tokens and total_tokens are the two this type always read;
// input_tokens and output_tokens are the pair [BilledUnits] names as arriving
// here unmodelled, and a backend that used them metered as ZERO tokens — priced
// at nothing, and invisible to §11.6's token guard, which triggers on a positive
// total.
//
// The pointers carry absence: dorang's own encoder emits prompt_tokens and
// total_tokens, so omitempty keeps the two read-only spellings off every answer
// it writes, and a nil is what says the backend used the other pair.
type Usage struct {
	TotalTokens  int `json:"total_tokens"`
	PromptTokens int `json:"prompt_tokens,omitempty"`

	InputTokens  *int `json:"input_tokens,omitempty"`
	OutputTokens *int `json:"output_tokens,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var usageKnown = wirejson.KnownKeys("total_tokens", "prompt_tokens",
	"input_tokens", "output_tokens")

// MarshalJSON implements [encoding/json.Marshaler].
func (u Usage) MarshalJSON() ([]byte, error) {
	type alias Usage
	return wirejson.MarshalWithExtra(alias(u), u.Extra, usageKnown)
}

// AppendJSON implements [wirejson.Appender]. It is [Usage.MarshalJSON]
// writing into the caller's buffer; the two are held byte-identical by
// FuzzAppendAgreesWithMarshal.
func (u Usage) AppendJSON(dst []byte) ([]byte, error) {
	type alias Usage
	return wirejson.AppendWithExtra(dst, alias(u), u.Extra, usageKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
func (u *Usage) UnmarshalJSON(b []byte) error {
	type alias Usage
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := wirejson.SplitExtraFold(b, usageKnown)
	if err != nil {
		return err
	}
	*u = Usage(a)
	u.Extra = extra
	return nil
}

// Meta is the search-unit billing block.
//
// api_version, warnings and tokens all live here on the vendor surface and none
// of them is modelled, so Extra is the difference between relaying a
// deprecation warning to the client and swallowing it.
type Meta struct {
	BilledUnits *BilledUnits `json:"billed_units,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var metaKnown = wirejson.KnownKeys("billed_units")

// MarshalJSON implements [encoding/json.Marshaler].
func (m Meta) MarshalJSON() ([]byte, error) {
	type alias Meta
	return wirejson.MarshalWithExtra(alias(m), m.Extra, metaKnown)
}

// AppendJSON implements [wirejson.Appender]. It is [Meta.MarshalJSON]
// writing into the caller's buffer; the two are held byte-identical by
// FuzzAppendAgreesWithMarshal.
func (m Meta) AppendJSON(dst []byte) ([]byte, error) {
	type alias Meta
	return wirejson.AppendWithExtra(dst, alias(m), m.Extra, metaKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
func (m *Meta) UnmarshalJSON(b []byte) error {
	type alias Meta
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := wirejson.SplitExtraFold(b, metaKnown)
	if err != nil {
		return err
	}
	*m = Meta(a)
	m.Extra = extra
	return nil
}

// BilledUnits is what the vendor charged for.
//
// search_units is the only member dorang prices from, and it is the only one
// modelled. The rest — input_tokens, output_tokens, classifications, whatever
// the vendor adds — are equally real line items and ride through Extra rather
// than being deleted for not being interesting to the router.
//
// That is deliberate and it is NOT the same decision as [Usage], which does read
// input_tokens. A vendor that fills this block bills in search units, and dorang
// prices the search units; reading a token count out of the same block and
// pricing that too would charge one call twice, in two units. The counts here
// are carried for the client to reconcile against, not for the router to bill
// from. TestRerankAnswerDropsNothing asserts they arrive.
type BilledUnits struct {
	SearchUnits int `json:"search_units"`

	Extra map[string]json.RawMessage `json:"-"`
}

var billedUnitsKnown = wirejson.KnownKeys("search_units")

// MarshalJSON implements [encoding/json.Marshaler].
func (b BilledUnits) MarshalJSON() ([]byte, error) {
	type alias BilledUnits
	return wirejson.MarshalWithExtra(alias(b), b.Extra, billedUnitsKnown)
}

// AppendJSON implements [wirejson.Appender]. It is [BilledUnits.MarshalJSON]
// writing into the caller's buffer; the two are held byte-identical by
// FuzzAppendAgreesWithMarshal.
func (b BilledUnits) AppendJSON(dst []byte) ([]byte, error) {
	type alias BilledUnits
	return wirejson.AppendWithExtra(dst, alias(b), b.Extra, billedUnitsKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
func (u *BilledUnits) UnmarshalJSON(b []byte) error {
	type alias BilledUnits
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := wirejson.SplitExtraFold(b, billedUnitsKnown)
	if err != nil {
		return err
	}
	*u = BilledUnits(a)
	u.Extra = extra
	return nil
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

// ErrNotAResponse is a JSON object that parsed cleanly and is not a rerank
// answer.
//
// It is a distinct condition from a decode failure, and the same one the chat
// decoders name. A vendor that answers HTTP 200 with
// `{"code":500,"msg":"404 NOT_FOUND","success":false}` — a real answer from a
// real host to a request addressed at a route it does not serve — unmarshals
// into a zero-valued [Response] without error.
//
// What reached the client was not merely an empty ranking. [Response] carries an
// Extra map so a member dorang does not model survives the crossing, and
// [MarshalResponse] splices it back on the way out, so the vendor's own error
// members were re-serialized INTO the answer next to dorang's spliced `model`
// and an empty `results` array — the relay failure mode, reached through a
// converting path. Nothing downstream could tell it from a ranking of nothing:
// retries never fired, §7.6 fallback never engaged, health counted a success,
// and neither billing block was present so cost recorded zero.
var ErrNotAResponse = errors.New("rerank: the body is a JSON object but not a rerank response")

// IsResponse reports whether a decoded body is a rerank answer.
//
// None of the three dialects defines a discriminator — there is no `object` or
// `type` member on a rerank answer in the generic, Cohere or Jina spelling — so
// the "accept on either ground" rule the chat gates state reduces here to its
// second ground alone: a payload-bearing member must be PRESENT. Those are
// `results`, which is the ranking, and `usage`, which is what it cost. It is the
// same pair the embeddings relay requires, for the same reason.
//
// Presence, not value. A rerank over an empty document list answers
// `{"results":[]}`, and a check for a non-empty ranking would refuse it.
//
// `meta` is deliberately NOT a ground, even though the Cohere dialect sends it
// on every answer. It is a metadata sidecar — api_version, warnings — of exactly
// the kind a vendor's error envelope also carries, so accepting on it would
// admit the bodies this exists to refuse. Every real answer that has a `meta`
// has a `results` beside it.
func IsResponse(w *Response) bool {
	return w != nil && (w.Results != nil || w.Usage != nil)
}

// DecodeResponse parses a rerank answer into the neutral form.
//
// json.Unmarshal and not the strict filter: these bytes are a backend's, not a
// caller's, and nothing is authorized against a response.
//
// It returns [ErrNotAResponse] for a JSON object that is not a rerank answer.
// Parsing without error is not the same fact as "this is an answer"; see
// [ErrNotAResponse] and [IsResponse].
func DecodeResponse(b []byte, flavor Flavor, model string) (*canonical.RerankResponse, error) {
	var w Response
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	if !IsResponse(&w) {
		return nil, ErrNotAResponse
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
	out := &canonical.RerankResponse{ID: w.ID, Model: w.Model, Extra: w.Extra}
	if model != "" {
		out.Model = model
	}
	if w.Usage != nil {
		// Inclusive input accounting, like every other surface (DESIGN §10.7).
		// A rerank backend usually reports one number; it is the whole prompt, so
		// it is InputTokens and the total derives from it rather than the other
		// way round.
		//
		// The total is the FALLBACK and not the first choice, and it is taken net
		// of any stated output half. A host that reports 400 in, 7 out and 407
		// total was charging 400 at the prompt rate and 7 at a generation rate,
		// and reading the total as the prompt count bills all 407 as prompt —
		// the same class of error as folding a cache count into the number it is
		// a breakdown of.
		out.Usage = &canonical.Usage{Reported: canonical.UsageInput}
		if w.Usage.OutputTokens != nil {
			out.Usage.OutputTokens = *w.Usage.OutputTokens
			out.Usage.Report(canonical.UsageOutput)
		}
		switch {
		case w.Usage.PromptTokens != 0:
			out.Usage.InputTokens = w.Usage.PromptTokens
		case w.Usage.InputTokens != nil && *w.Usage.InputTokens != 0:
			out.Usage.InputTokens = *w.Usage.InputTokens
		default:
			out.Usage.InputTokens = w.Usage.TotalTokens - out.Usage.OutputTokens
			if out.Usage.InputTokens < 0 {
				// A total smaller than the output half it is supposed to contain
				// is a contradiction dorang cannot attribute. Zero, never a
				// negative: a negative count hands back budget and quota nobody
				// paid for (internal/pricing's clamp, same rule).
				out.Usage.InputTokens = 0
			}
		}
		out.UsageExtra = w.Usage.Extra
	}
	if w.Meta != nil {
		out.MetaExtra = w.Meta.Extra
		if w.Meta.BilledUnits != nil {
			out.SearchUnits = w.Meta.BilledUnits.SearchUnits
			out.SearchUnitsReported = true
			out.BilledUnitsExtra = w.Meta.BilledUnits.Extra
		}
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
	return wirejson.MarshalAppended(w)
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
	return wirejson.MarshalAppended(w)
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
	out.Extra = r.Extra
	// "Only when the backend actually reported it" is what the [Response] doc
	// comment always said and what an `> 0` test cannot express: it collapses a
	// backend that billed nothing into a backend that said nothing. The presence
	// flags carry the difference; a count dorang produced itself still reports
	// nothing and is still omitted.
	if r.Usage != nil && (r.Usage.InputTokens > 0 || r.Usage.Reports(canonical.UsageInput)) {
		out.Usage = &Usage{
			// ONE definition of "total tokens" in the binary: input plus output,
			// with every breakdown field a subset of one of them. The same
			// function as canonical.Usage.TotalTokens, deliberately — a rerank
			// answer that stated an output half and a total excluding it is a
			// total two readers would disagree about.
			TotalTokens:  r.Usage.TotalTokens(),
			PromptTokens: r.Usage.InputTokens,
			Extra:        r.UsageExtra,
		}
		if r.Usage.OutputTokens > 0 || r.Usage.Reports(canonical.UsageOutput) {
			// Only when the backend stated one. A reranker that scores rather
			// than generates reports no output half at all, and inventing a zero
			// would tell the client this vendor measured something it did not.
			out.Usage.OutputTokens = ptrInt(r.Usage.OutputTokens)
		}
	}
	if r.SearchUnits > 0 || r.SearchUnitsReported {
		out.Meta = &Meta{
			BilledUnits: &BilledUnits{SearchUnits: r.SearchUnits, Extra: r.BilledUnitsExtra},
			Extra:       r.MetaExtra,
		}
	} else if len(r.MetaExtra) > 0 {
		// meta carried something other than a billing block — an api_version, a
		// deprecation warning. Dropping it because there were no search units
		// loses the part the client was most likely to act on.
		out.Meta = &Meta{Extra: r.MetaExtra}
	}
	return out, nil
}

// ptrInt is the address of a count, used where absence and zero are different
// facts on the wire (see [Usage]).
func ptrInt(v int) *int { return &v }
