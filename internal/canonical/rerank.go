package canonical

import "encoding/json"

// RerankRequest is a protocol-neutral rerank call.
//
// Rerank is not an OpenAI surface. Three spellings of it are deployed — the
// widely-copied `/v1/rerank`, the vendor's own `/v2/rerank`, and the
// embedding-vendor variant — and they agree on the shape of the question
// (a query and a document list) while disagreeing about how the answer reports
// its billing. That disagreement is exactly why the neutral form exists: the
// frontend decodes to this, and each backend encoder folds it onto its own
// spelling (DESIGN §10.1).
type RerankRequest struct {
	// Model is an opaque string. Nothing splits it (DESIGN §2.1).
	Model string
	Query string
	// Documents is the corpus, in the caller's order. Result indexes refer to
	// positions in this slice, so it is never reordered or de-duplicated.
	Documents []RerankDocument

	// TopN caps the returned results. Nil means "all of them", which is not the
	// same as zero.
	TopN *int
	// ReturnDocuments asks for the document text to be echoed back beside each
	// result. Nil means the backend default, which differs per vendor.
	ReturnDocuments *bool
	// MaxTokensPerDoc truncates a document rather than refusing it.
	MaxTokensPerDoc *int

	// RankFields names the members of a structured document to rank on.
	RankFields []string

	// User is the end-user identifier, for abuse attribution.
	User string

	// Extra carries every field dorang does not model, keyed by its wire name.
	Extra map[string]json.RawMessage
}

// RerankDocument is one corpus entry.
//
// A document arrives as a bare string or as a JSON object with named fields;
// both forms are on the wire and which one arrived is preserved, because
// collapsing an object to its text silently changes what a rank_fields-aware
// backend is ranking.
type RerankDocument struct {
	// Text is the document as plain text, meaningful when Fields is nil.
	Text string
	// Fields is the structured form, kept raw so member order survives.
	Fields json.RawMessage
}

// IsStructured reports whether the object form is in use.
func (d RerankDocument) IsStructured() bool { return len(d.Fields) > 0 }

// RerankResponse is a protocol-neutral rerank answer.
type RerankResponse struct {
	ID    string
	Model string
	// Results are in descending relevance order, as the backend returned them.
	Results []RerankResult
	// Usage is normalized the same way every other surface's is: inclusive
	// input counts (DESIGN §10.7). A vendor that bills rerank in "search units"
	// rather than tokens reports them in SearchUnits, never folded into a token
	// count that would then be priced per token.
	Usage *Usage
	// SearchUnits is what the vendor charged. SearchUnitsReported distinguishes
	// a billed_units block that said zero from one that was never sent, for the
	// reason [UsageField] gives about token counters: a billing integration
	// reads those two differently.
	SearchUnits         int
	SearchUnitsReported bool

	// Extra carries members of the response object dorang does not model.
	//
	// Rerank needs no [Family] gate. There is one client-facing rerank shape —
	// the union of the two vendor billing blocks, which this package emits
	// whichever dialect answered — so an answer never leaves in a shape other
	// than the one this type describes.
	Extra map[string]json.RawMessage
	// UsageExtra, MetaExtra and BilledUnitsExtra are the unmodelled members of
	// the three billing sub-objects. They are separate maps for the same reason
	// [UsageExtra] has three: flattening them loses which object a member
	// belonged to.
	UsageExtra       map[string]json.RawMessage
	MetaExtra        map[string]json.RawMessage
	BilledUnitsExtra map[string]json.RawMessage
}

// RerankResult is one scored document.
type RerankResult struct {
	// Index is the position in the request's Documents slice.
	Index int
	// RelevanceScore is the backend's score. It is NOT normalized across
	// vendors: the scales differ and rescaling would invent precision.
	RelevanceScore float64
	// Document is echoed back only when the caller asked for it.
	Document *RerankDocument
}
