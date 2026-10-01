package canonical

// SystemOneRequest is a System One decision request: one shared state and a
// set of named, typed questions about it (POST /v1/systemone).
//
// It is not a neutral form the way [Request] is. Every upstream that serves the
// operation speaks the same contract — TypeSafe defines it, local Ollama and
// the aggregators implement it — so the body is relayed with only the model
// replaced, and this is what the gateway learned while validating it: the
// shape each backend checks its own limits against, without parsing the body a
// second time.
type SystemOneRequest struct {
	// Model is the client-facing model name.
	Model string
	// Questions are the request's questions in id order.
	Questions []SystemOneQuestion
	// StringCriteriaOnly reports whether every criterion description is a plain
	// string (or null, for a Choice option). Ollama accepts nothing else there;
	// TypeSafe also accepts objects and arrays. Instructions are not covered:
	// both accept a string, an object or an array.
	StringCriteriaOnly bool
}

// SystemOneQuestion is one question's type and size.
type SystemOneQuestion struct {
	ID   string
	Type string
	// Options is the number of choices of a Choice, or the number of levels of
	// a Score. Zero for a Noul.
	Options int
}

// System One question types.
const (
	SystemOneNoul   = "noul"
	SystemOneChoice = "choice"
	SystemOneScore  = "score"
)
