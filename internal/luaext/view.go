package luaext

// The views are what a hook is allowed to know.
//
// Every one of them is a flat struct of identifiers, names and numbers. There
// is deliberately no field for a header map, a request body, a response body, a
// credential, or an Authorization value — DESIGN §11.5's sandbox is enforced by
// the *shape* of the data a hook receives rather than by a filter over a richer
// object, because a filter is a list of things somebody remembered and a struct
// with no such field is a proof.
//
// Field access from a policy program never uses reflection (§15.5 prohibits it
// on the hot path). Each view carries a hand-written accessor keyed by an index
// the compiler resolved at load time, so an unknown identifier is a load error
// rather than a runtime nil.

// viewer is the compiled program's window onto one hook's inputs.
type viewer interface {
	field(i int) value
}

// fieldSpec describes one identifier a program may name.
type fieldSpec struct {
	name string
	kind kind
	idx  int
}

// fieldTable indexes a hook's fields by name for the compiler.
type fieldTable struct {
	byName map[string]fieldSpec
	names  []string
}

func newFieldTable(specs []fieldSpec) fieldTable {
	t := fieldTable{byName: make(map[string]fieldSpec, len(specs))}
	for _, s := range specs {
		t.byName[s.name] = s
		t.names = append(t.names, s.name)
	}
	return t
}

// ---------------------------------------------------------------------------
// on_request

// RequestView is what on_request sees.
type RequestView struct {
	RequestID string
	Method    string
	Path      string
	Route     string
	Model     string
	KeyID     string
	KeyName   string
	UserID    string
	TeamID    string
	Priority  string

	BodyBytes       int64
	InputTokens     int64
	MaxOutputTokens int64

	Stream bool
}

const (
	fRequestID = iota
	fMethod
	fPath
	fRoute
	fModel
	fKeyID
	fKeyName
	fUserID
	fTeamID
	fPriority
	fBodyBytes
	fInputTokens
	fMaxOutputTokens
	fStream
)

var requestFields = newFieldTable([]fieldSpec{
	{"request_id", kindString, fRequestID},
	{"method", kindString, fMethod},
	{"path", kindString, fPath},
	{"route", kindString, fRoute},
	{"model", kindString, fModel},
	{"key_id", kindString, fKeyID},
	{"key_name", kindString, fKeyName},
	{"user_id", kindString, fUserID},
	{"team_id", kindString, fTeamID},
	{"priority", kindString, fPriority},
	{"body_bytes", kindNumber, fBodyBytes},
	{"input_tokens", kindNumber, fInputTokens},
	{"max_output_tokens", kindNumber, fMaxOutputTokens},
	{"stream", kindBool, fStream},
})

func (v *RequestView) field(i int) value {
	switch i {
	case fRequestID:
		return strValue(v.RequestID)
	case fMethod:
		return strValue(v.Method)
	case fPath:
		return strValue(v.Path)
	case fRoute:
		return strValue(v.Route)
	case fModel:
		return strValue(v.Model)
	case fKeyID:
		return strValue(v.KeyID)
	case fKeyName:
		return strValue(v.KeyName)
	case fUserID:
		return strValue(v.UserID)
	case fTeamID:
		return strValue(v.TeamID)
	case fPriority:
		return strValue(v.Priority)
	case fBodyBytes:
		return numValue(v.BodyBytes)
	case fInputTokens:
		return numValue(v.InputTokens)
	case fMaxOutputTokens:
		return numValue(v.MaxOutputTokens)
	case fStream:
		return boolValue(v.Stream)
	}
	return value{}
}

// RequestDecision is on_request's answer. The zero value permits the request,
// which is what every failure path returns.
type RequestDecision struct {
	tagset
	// Denied refuses the request. It is the one hook outcome that is honoured
	// on failure paths' behalf: it can only be set by a hook that completed
	// within its ceilings (DESIGN §11.5).
	Denied bool
	// Reason is shown to the caller. It must not name anything internal.
	Reason string
	// Code is the machine-readable refusal code, defaulted by the caller when
	// the program did not set one.
	Code string
}

// ---------------------------------------------------------------------------
// on_route

// RouteView is what on_route sees: the routing question and the answer the
// router gave, never the credential that answer resolves to.
type RouteView struct {
	Model         string
	KeyID         string
	UserID        string
	TeamID        string
	Provider      string
	Deployment    string
	Kind          string
	UpstreamModel string
	Priority      string

	Attempt     int64
	InputTokens int64

	Stream bool
}

const (
	rModel = iota
	rKeyID
	rUserID
	rTeamID
	rProvider
	rDeployment
	rKind
	rUpstreamModel
	rPriority
	rAttempt
	rInputTokens
	rStream
)

var routeFields = newFieldTable([]fieldSpec{
	{"model", kindString, rModel},
	{"key_id", kindString, rKeyID},
	{"user_id", kindString, rUserID},
	{"team_id", kindString, rTeamID},
	{"provider", kindString, rProvider},
	{"deployment", kindString, rDeployment},
	{"kind", kindString, rKind},
	{"upstream_model", kindString, rUpstreamModel},
	{"priority", kindString, rPriority},
	{"attempt", kindNumber, rAttempt},
	{"input_tokens", kindNumber, rInputTokens},
	{"stream", kindBool, rStream},
})

func (v *RouteView) field(i int) value {
	switch i {
	case rModel:
		return strValue(v.Model)
	case rKeyID:
		return strValue(v.KeyID)
	case rUserID:
		return strValue(v.UserID)
	case rTeamID:
		return strValue(v.TeamID)
	case rProvider:
		return strValue(v.Provider)
	case rDeployment:
		return strValue(v.Deployment)
	case rKind:
		return strValue(v.Kind)
	case rUpstreamModel:
		return strValue(v.UpstreamModel)
	case rPriority:
		return strValue(v.Priority)
	case rAttempt:
		return numValue(v.Attempt)
	case rInputTokens:
		return numValue(v.InputTokens)
	case rStream:
		return boolValue(v.Stream)
	}
	return value{}
}

// RouteDecision is on_route's answer.
type RouteDecision struct {
	tagset
	// Denied refuses the routing decision, and with it the request.
	//
	// It refuses rather than re-routing, and that is a real limit rather than a
	// preference: asking the router for a different deployment means entering a
	// fail-back session, and internal/router's fail-back is keyed on a Cause
	// drawn from a fixed set whose chains are configured per model. There is no
	// cause that means "an extension objected", and inventing one by borrowing
	// rate_limit would mark a healthy deployment unavailable. Refusing is the
	// safe direction for a policy: a data-residency rule that cannot be applied
	// should stop the request, not route around itself.
	Denied bool
	// Reason explains the refusal to the caller.
	Reason string
	// Code is the machine-readable refusal code.
	Code string
}

// ---------------------------------------------------------------------------
// on_response

// ResponseView is what on_response sees.
type ResponseView struct {
	RequestID     string
	Model         string
	KeyID         string
	UserID        string
	TeamID        string
	Provider      string
	Deployment    string
	UpstreamModel string
	ErrorCode     string

	Status       int64
	InputTokens  int64
	OutputTokens int64
	CostNanoUSD  int64
	TTFTMillis   int64
	TotalMillis  int64
	Attempts     int64

	Stream bool
}

const (
	pRequestID = iota
	pModel
	pKeyID
	pUserID
	pTeamID
	pProvider
	pDeployment
	pUpstreamModel
	pErrorCode
	pStatus
	pInputTokens
	pOutputTokens
	pCostNanoUSD
	pTTFT
	pTotal
	pAttempts
	pStream
)

var responseFields = newFieldTable([]fieldSpec{
	{"request_id", kindString, pRequestID},
	{"model", kindString, pModel},
	{"key_id", kindString, pKeyID},
	{"user_id", kindString, pUserID},
	{"team_id", kindString, pTeamID},
	{"provider", kindString, pProvider},
	{"deployment", kindString, pDeployment},
	{"upstream_model", kindString, pUpstreamModel},
	{"error_code", kindString, pErrorCode},
	{"status", kindNumber, pStatus},
	{"input_tokens", kindNumber, pInputTokens},
	{"output_tokens", kindNumber, pOutputTokens},
	{"cost_nano_usd", kindNumber, pCostNanoUSD},
	{"ttft_ms", kindNumber, pTTFT},
	{"total_ms", kindNumber, pTotal},
	{"attempts", kindNumber, pAttempts},
	{"stream", kindBool, pStream},
})

func (v *ResponseView) field(i int) value {
	switch i {
	case pRequestID:
		return strValue(v.RequestID)
	case pModel:
		return strValue(v.Model)
	case pKeyID:
		return strValue(v.KeyID)
	case pUserID:
		return strValue(v.UserID)
	case pTeamID:
		return strValue(v.TeamID)
	case pProvider:
		return strValue(v.Provider)
	case pDeployment:
		return strValue(v.Deployment)
	case pUpstreamModel:
		return strValue(v.UpstreamModel)
	case pErrorCode:
		return strValue(v.ErrorCode)
	case pStatus:
		return numValue(v.Status)
	case pInputTokens:
		return numValue(v.InputTokens)
	case pOutputTokens:
		return numValue(v.OutputTokens)
	case pCostNanoUSD:
		return numValue(v.CostNanoUSD)
	case pTTFT:
		return numValue(v.TTFTMillis)
	case pTotal:
		return numValue(v.TotalMillis)
	case pAttempts:
		return numValue(v.Attempts)
	case pStream:
		return boolValue(v.Stream)
	}
	return value{}
}

// ResponseDecision is on_response's answer. It carries annotations only: the
// response has already been written when this hook runs, so there is nothing
// left to refuse.
type ResponseDecision struct {
	tagset
}

// ---------------------------------------------------------------------------
// on_email

// EmailView is what on_email sees: a notification that has already been
// rendered and redacted by internal/notify. It never carries key material,
// because the message it describes never carries key material either.
type EmailView struct {
	Event       string
	SubjectKind string
	SubjectID   string
	Recipient   string
	Subject     string
	Body        string
	Driver      string
}

const (
	eEvent = iota
	eSubjectKind
	eSubjectID
	eRecipient
	eSubject
	eBody
	eDriver
)

var emailFields = newFieldTable([]fieldSpec{
	{"event", kindString, eEvent},
	{"subject_kind", kindString, eSubjectKind},
	{"subject_id", kindString, eSubjectID},
	{"recipient", kindString, eRecipient},
	{"subject", kindString, eSubject},
	{"body", kindString, eBody},
	{"driver", kindString, eDriver},
})

func (v *EmailView) field(i int) value {
	switch i {
	case eEvent:
		return strValue(v.Event)
	case eSubjectKind:
		return strValue(v.SubjectKind)
	case eSubjectID:
		return strValue(v.SubjectID)
	case eRecipient:
		return strValue(v.Recipient)
	case eSubject:
		return strValue(v.Subject)
	case eBody:
		return strValue(v.Body)
	case eDriver:
		return strValue(v.Driver)
	}
	return value{}
}

// EmailDecision is on_email's answer.
type EmailDecision struct {
	tagset
	// Denied suppresses the message. The suppression is counted by
	// internal/notify, never silent.
	Denied bool
	// Reason explains the suppression.
	Reason string
}

// fieldsFor returns the identifier table a hook's programs compile against.
func fieldsFor(h Hook) fieldTable {
	switch h {
	case HookRequest:
		return requestFields
	case HookRoute:
		return routeFields
	case HookResponse:
		return responseFields
	case HookEmail:
		return emailFields
	}
	return fieldTable{byName: map[string]fieldSpec{}}
}
