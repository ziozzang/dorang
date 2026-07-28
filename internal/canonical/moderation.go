package canonical

import "encoding/json"

// ModerationRequest is a protocol-neutral content-classification call.
type ModerationRequest struct {
	// Model is an opaque string, and may be empty: the moderation surface has a
	// server-side default in the vendor API. dorang still requires one, because
	// a gateway with several deployments cannot route a request that names no
	// model — the refusal happens in the handler, not here.
	Model string
	// Inputs are the items to classify, in the caller's order. Result i
	// describes input i.
	Inputs []ModerationInput
	// StringForm records that the wire carried a bare string rather than an
	// array, so a same-protocol crossing re-emits the form the caller sent.
	StringForm bool
	Extra      map[string]json.RawMessage
}

// ModerationInputKind discriminates a [ModerationInput].
type ModerationInputKind string

// The moderation input kinds.
const (
	ModerationText  ModerationInputKind = "text"
	ModerationImage ModerationInputKind = "image_url"
)

// ModerationInput is one item to classify.
//
// The bare-string request form and the typed-array form both exist on the wire.
// Which one arrived is preserved on the request (see StringForm) so a
// same-protocol crossing does not rewrite the caller's request.
type ModerationInput struct {
	Kind ModerationInputKind
	Text string
	// Source carries an image input.
	Source *Source
}

// ModerationResponse is a protocol-neutral classification answer.
type ModerationResponse struct {
	ID      string
	Model   string
	Results []ModerationResult
	Extra   map[string]json.RawMessage
}

// ModerationResult is the verdict for one input.
//
// Categories is an ordered slice rather than three parallel maps, because the
// wire form IS three parallel objects keyed by category name and rebuilding
// them from maps loses the backend's ordering. A category set is small and
// vendor-specific; enumerating it in dorang would date immediately.
type ModerationResult struct {
	Flagged    bool
	Categories []ModerationCategory
	Extra      map[string]json.RawMessage
}

// ModerationCategory is one classifier output.
type ModerationCategory struct {
	// Name is the vendor's own category name, verbatim.
	Name    string
	Flagged bool
	// Score is present when the backend reported one. Scored distinguishes an
	// absent score from a reported 0.0 — a category the backend did not score
	// and one it scored zero are different answers.
	Score   float64
	Scored  bool
	Applied []string
}
