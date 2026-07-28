package openai

import (
	"encoding/json"

	"github.com/ziozzang/dorang/internal/canonical"
)

// The legacy text-completions surface: POST /v1/completions.
//
// It is a separate wire shape, not an alias of chat completions, and it goes
// through the neutral representation like everything else (DESIGN §10.1). That
// is what lets a `/v1/completions` request reach an Anthropic-shaped deployment
// at all: the prompt becomes a single user turn, and the answer comes back
// through canonical.Response into the text_completion shape the caller expects.
//
// The prompt field is the reason this file is longer than it looks. It is four
// things on the wire — a string, an array of strings, an array of token ids,
// and an array of arrays of token ids — and normalizing them to one form
// changes what the backend generates from. All four survive; see
// [canonical.Prompt].

// ObjectTextCompletion is the object value of this surface.
const ObjectTextCompletion = "text_completion"

// CompletionRequest is a legacy text-completions request.
type CompletionRequest struct {
	Model  string `json:"model"`
	Prompt Prompt `json:"prompt,omitempty"`

	MaxTokens   *int     `json:"max_tokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	TopK        *int     `json:"top_k,omitempty"`
	N           *int     `json:"n,omitempty"`

	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`

	// Logprobs is an INTEGER here and a boolean on the chat surface. The two
	// spellings share a name and nothing else; treating them as one field
	// silently turns "give me 5 alternatives" into "true".
	Logprobs *int `json:"logprobs,omitempty"`

	Stop             StopSequences      `json:"stop,omitempty"`
	PresencePenalty  *float64           `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64           `json:"frequency_penalty,omitempty"`
	LogitBias        map[string]float64 `json:"logit_bias,omitempty"`
	Seed             *int64             `json:"seed,omitempty"`
	User             string             `json:"user,omitempty"`
	ServiceTier      string             `json:"service_tier,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// suffix, echo and best_of are deliberately ABSENT from this list and from the
// struct above. They are legacy-only knobs with no neutral equivalent and no
// meaning to dorang, so they ride through Extra untouched: a same-protocol
// crossing forwards them verbatim, and a crossing into a family that has never
// heard of them drops them the same way every other unmodelled member is
// dropped. Modelling them would buy a field dorang never reads and a second
// place for the encoder and the decoder to disagree.
var completionRequestKnown = knownKeys("model", "prompt", "max_tokens",
	"temperature", "top_p", "top_k", "n", "stream", "stream_options", "logprobs",
	"stop", "presence_penalty", "frequency_penalty",
	"logit_bias", "seed", "user", "service_tier")

// MarshalJSON implements [encoding/json.Marshaler].
func (r CompletionRequest) MarshalJSON() ([]byte, error) {
	type alias CompletionRequest
	return marshalWithExtra(alias(r), r.Extra, completionRequestKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler] with case-SENSITIVE
// field matching (COMPATIBILITY 2.0).
func (r *CompletionRequest) UnmarshalJSON(b []byte) error {
	type alias CompletionRequest
	var a alias
	b = strictBytes(b, &a)
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtra(b, completionRequestKnown)
	if err != nil {
		return err
	}
	*r = CompletionRequest(a)
	r.Extra = extra
	return nil
}

// Prompt is the wire form of the prompt field.
type Prompt canonical.Prompt

// MarshalJSON implements [encoding/json.Marshaler]. It re-emits the form that
// arrived: a bare string stays a bare string, an array stays an array.
func (p Prompt) MarshalJSON() ([]byte, error) {
	switch {
	case len(p.Tokens) > 0:
		if !p.Array && len(p.Tokens) == 1 {
			return json.Marshal(p.Tokens[0])
		}
		return json.Marshal(p.Tokens)
	case p.Array:
		if p.Texts == nil {
			return []byte(`[]`), nil
		}
		return Marshal(p.Texts)
	case len(p.Texts) == 0:
		return []byte(`""`), nil
	default:
		return Marshal(p.Texts[0])
	}
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
func (p *Prompt) UnmarshalJSON(b []byte) error {
	b = trimSpace(b)
	*p = Prompt{}
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	switch b[0] {
	case '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		p.Texts = []string{s}
		return nil
	case '[':
		// An empty array is neither text nor tokens; record the array form and
		// nothing else so it round-trips as [].
		var probe []json.RawMessage
		if err := json.Unmarshal(b, &probe); err != nil {
			return err
		}
		p.Array = true
		if len(probe) == 0 {
			p.Texts = []string{}
			return nil
		}
		switch trimSpace(probe[0])[0] {
		case '"':
			var texts []string
			if err := json.Unmarshal(b, &texts); err != nil {
				return err
			}
			p.Texts = texts
		case '[':
			var tokens [][]int
			if err := json.Unmarshal(b, &tokens); err != nil {
				return err
			}
			p.Tokens = tokens
		default:
			var tokens []int
			if err := json.Unmarshal(b, &tokens); err != nil {
				return err
			}
			p.Tokens = [][]int{tokens}
			p.Array = false
		}
		return nil
	default:
		return errorString("openai: prompt must be a string or an array")
	}
}

// CompletionResponse is a complete legacy text completion.
//
// This shape is NOT chat completions and its unmodelled members do not
// interchange with that one — a choice here has `text` where a choice there has
// `message`. It carries its own [canonical.FamilyOpenAICompletions] tag for
// exactly that reason, so a chat backend's extras never surface here and this
// surface's never surface there.
type CompletionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []CompletionChoice `json:"choices"`
	Usage   *Usage             `json:"usage,omitempty"`

	SystemFingerprint *string `json:"system_fingerprint,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var completionResponseKnown = knownKeys("id", "object", "created", "model",
	"choices", "usage", "system_fingerprint")

// MarshalJSON implements [encoding/json.Marshaler].
func (r CompletionResponse) MarshalJSON() ([]byte, error) {
	type alias CompletionResponse
	return marshalWithExtra(alias(r), r.Extra, completionResponseKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler]. Response-only type, so
// json.Unmarshal rather than the strict filter — see [strictUnmarshal].
func (r *CompletionResponse) UnmarshalJSON(b []byte) error {
	type alias CompletionResponse
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtraFold(b, completionResponseKnown)
	if err != nil {
		return err
	}
	*r = CompletionResponse(a)
	r.Extra = extra
	return nil
}

// CompletionChoice is one completed choice.
type CompletionChoice struct {
	Index int    `json:"index"`
	Text  string `json:"text"`
	// Logprobs is carried raw; per-backend fidelity is explicitly unverified
	// (COMPATIBILITY 10) and it must never appear as "logprobs":null.
	Logprobs               json.RawMessage            `json:"logprobs,omitempty"`
	FinishReason           *string                    `json:"finish_reason,omitempty"`
	ProviderSpecificFields map[string]json.RawMessage `json:"provider_specific_fields,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var completionChoiceKnown = knownKeys("index", "text", "logprobs",
	"finish_reason", "provider_specific_fields")

// MarshalJSON implements [encoding/json.Marshaler].
func (c CompletionChoice) MarshalJSON() ([]byte, error) {
	type alias CompletionChoice
	return marshalWithExtra(alias(c), c.Extra, completionChoiceKnown)
}

// UnmarshalJSON implements [encoding/json.Unmarshaler].
func (c *CompletionChoice) UnmarshalJSON(b []byte) error {
	type alias CompletionChoice
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	extra, err := splitExtraFold(b, completionChoiceKnown)
	if err != nil {
		return err
	}
	*c = CompletionChoice(a)
	c.Extra = extra
	return nil
}

// ---------------------------------------------------------------------------
// Decode
// ---------------------------------------------------------------------------

// DecodeCompletionRequest parses legacy completion bytes into the neutral form.
//
// The decode is case-SENSITIVE (COMPATIBILITY 2.0), for the same reason the
// chat decode is: the authorization gate scanned these bytes with an exact key
// comparison, and an adapter that resolves a model the gate did not see is an
// allow-list bypass.
func DecodeCompletionRequest(b []byte) (*canonical.Request, error) {
	var w CompletionRequest
	if err := strictUnmarshal(b, &w); err != nil {
		return nil, err
	}
	return CompletionRequestToCanonical(&w)
}

// CompletionRequestToCanonical converts a decoded legacy request.
func CompletionRequestToCanonical(w *CompletionRequest) (*canonical.Request, error) {
	if w == nil {
		return nil, errNilRequest
	}
	p := canonical.Prompt(w.Prompt)
	out := &canonical.Request{
		Model:            w.Model,
		MaxTokens:        w.MaxTokens,
		Temperature:      w.Temperature,
		TopP:             w.TopP,
		TopK:             w.TopK,
		N:                w.N,
		Stream:           w.Stream,
		FrequencyPenalty: w.FrequencyPenalty,
		PresencePenalty:  w.PresencePenalty,
		LogitBias:        w.LogitBias,
		Seed:             w.Seed,
		User:             w.User,
		ServiceTier:      w.ServiceTier,
		Prompt:           &p,
		Extra:            w.Extra,
	}
	if len(w.Stop) > 0 {
		out.Stop = []string(w.Stop)
	}
	if w.StreamOptions != nil {
		out.StreamOptions = &canonical.StreamOptions{IncludeUsage: w.StreamOptions.IncludeUsage}
	}
	if w.Logprobs != nil {
		// The two surfaces spell this differently and mean different things. The
		// neutral form carries the chat spelling, so the count moves to
		// TopLogprobs and the flag is set from its presence.
		out.Logprobs = ptr(true)
		out.TopLogprobs = w.Logprobs
	}
	// The prompt is ALSO rendered as a user turn. That is what makes a legacy
	// request routable to a deployment with no completions surface: the neutral
	// request is complete on its own, and Prompt is the extra fidelity a
	// completions-speaking backend gets on top.
	if text, _ := p.Text(); text != "" {
		out.Messages = []canonical.Message{canonical.TextMessage(canonical.RoleUser, text)}
	}
	return out, nil
}

// DecodeCompletionResponse parses a legacy completion into the neutral form.
//
// json.Unmarshal and not strictUnmarshal, for the reason [DecodeResponse]
// states: these bytes are a backend's, not a caller's.
func DecodeCompletionResponse(b []byte, opt *DecodeOptions) (*canonical.Response, error) {
	var w CompletionResponse
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	return CompletionResponseToCanonical(&w, opt)
}

// CompletionResponseToCanonical converts a decoded legacy response.
func CompletionResponseToCanonical(w *CompletionResponse, opt *DecodeOptions) (*canonical.Response, error) {
	if w == nil {
		return nil, errorString("openai: nil completion response")
	}
	out := &canonical.Response{
		ID: w.ID, Model: w.Model, Created: w.Created,
		Extra:       w.Extra,
		ExtraFamily: canonical.FamilyOpenAICompletions,
	}
	if opt != nil && opt.Model != "" {
		out.Model = opt.Model
	}
	if w.SystemFingerprint != nil {
		out.SystemFingerprint = *w.SystemFingerprint
	}
	if w.Usage != nil {
		out.Usage = usageToCanonical(w.Usage)
		out.UsageExtra = usageExtraOf(w.Usage)
	}
	out.Choices = make([]canonical.Choice, 0, len(w.Choices))
	for i := range w.Choices {
		c := &w.Choices[i]
		cc := canonical.Choice{
			Index: c.Index,
			Message: canonical.Message{
				Role:    canonical.RoleAssistant,
				Content: canonical.Content{canonical.TextBlock(c.Text)},
			},
			Logprobs:       c.Logprobs,
			Extra:          c.Extra,
			ProviderFields: c.ProviderSpecificFields,
		}
		cc.StopReason, cc.NativeStopReason = stopReasonOfChoice(c.FinishReason, c.ProviderSpecificFields, opt.warn())
		out.Choices = append(out.Choices, cc)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Encode
// ---------------------------------------------------------------------------

// MarshalCompletionRequest encodes a neutral request as legacy completion bytes.
func MarshalCompletionRequest(req *canonical.Request, opt *EncodeOptions) ([]byte, error) {
	w, err := EncodeCompletionRequest(req, opt)
	if err != nil {
		return nil, err
	}
	return Marshal(w)
}

// EncodeCompletionRequest converts a neutral request to the legacy wire shape.
//
// A request that never carried a prompt — one that arrived as chat and is being
// sent to a completions endpoint — is rendered from its messages, and the
// flattening is reported as a structural downgrade rather than performed
// silently: roles, tool calls and images have nowhere to go on this surface.
func EncodeCompletionRequest(req *canonical.Request, opt *EncodeOptions) (*CompletionRequest, error) {
	if req == nil {
		return nil, errNilRequest
	}
	loss := opt.loss()
	out := &CompletionRequest{
		Model:            req.Model,
		MaxTokens:        req.MaxTokens,
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		TopK:             req.TopK,
		N:                req.N,
		Stream:           req.Stream,
		FrequencyPenalty: req.FrequencyPenalty,
		PresencePenalty:  req.PresencePenalty,
		LogitBias:        req.LogitBias,
		Seed:             req.Seed,
		User:             req.User,
		ServiceTier:      req.ServiceTier,
		Extra:            req.Extra,
	}
	if opt != nil && opt.Model != "" {
		out.Model = opt.Model
	}
	if len(req.Stop) > 0 {
		out.Stop = StopSequences(req.Stop)
	}
	if req.StreamOptions != nil {
		out.StreamOptions = &StreamOptions{IncludeUsage: req.StreamOptions.IncludeUsage}
	}
	if req.TopLogprobs != nil {
		out.Logprobs = req.TopLogprobs
	}
	switch {
	case req.Prompt != nil:
		out.Prompt = Prompt(*req.Prompt)
	default:
		text, exact := flattenForPrompt(req)
		if !exact {
			loss.Downgrade(canonical.ConstructMultiBlockContent, "messages")
		}
		out.Prompt = Prompt{Texts: []string{text}}
	}
	if len(req.Tools) > 0 {
		loss.Downgrade(canonical.ConstructToolCalls, "tools")
	}
	return out, nil
}

// flattenForPrompt renders a chat conversation as one prompt string.
//
// It is deliberately plain — role labels and newlines, no chat template. A
// gateway that invents a template picks one model's and is wrong for every
// other, and the caller cannot tell that it happened. Reporting the loss is the
// honest half; the rendering only has to be predictable.
func flattenForPrompt(req *canonical.Request) (string, bool) {
	exact := len(req.System) == 0 && len(req.Messages) == 1 &&
		req.Messages[0].Role == canonical.RoleUser
	var out []byte
	if s := req.System.Flatten(); s != "" {
		out = append(out, s...)
		out = append(out, '\n', '\n')
	}
	for i := range req.Messages {
		m := &req.Messages[i]
		text := m.Content.Flatten()
		if len(req.Messages) == 1 && exact {
			return text, len(m.Content) <= 1
		}
		if text == "" {
			continue
		}
		if len(out) > 0 {
			out = append(out, '\n')
		}
		out = append(out, m.Role...)
		out = append(out, ':', ' ')
		out = append(out, text...)
	}
	return string(out), false
}

// MarshalCompletionResponse encodes a neutral response as legacy bytes.
func MarshalCompletionResponse(r *canonical.Response, opt *ResponseOptions) ([]byte, error) {
	w, err := EncodeCompletionResponse(r, opt)
	if err != nil {
		return nil, err
	}
	return Marshal(w)
}

// EncodeCompletionResponse converts a neutral response to the legacy shape.
func EncodeCompletionResponse(r *canonical.Response, opt *ResponseOptions) (*CompletionResponse, error) {
	if r == nil {
		return nil, errorString("openai: nil response")
	}
	out := &CompletionResponse{
		ID:      r.ID,
		Object:  ObjectTextCompletion,
		Created: r.Created,
		Model:   r.Model,
	}
	if opt != nil {
		if opt.ID != "" {
			out.ID = opt.ID
		}
		if opt.Created != 0 {
			out.Created = opt.Created
		}
		if opt.Model != "" {
			out.Model = opt.Model
		}
	}
	if r.SystemFingerprint != "" {
		out.SystemFingerprint = ptr(r.SystemFingerprint)
	}
	// Same-family only; see [canonical.Family].
	same := r.SameFamily(canonical.FamilyOpenAICompletions)
	if same {
		out.Extra = r.Extra
	}
	if r.Usage != nil {
		out.Usage = EncodeUsage(*r.Usage)
		if same {
			attachUsageExtra(out.Usage, r.UsageExtra)
		}
	}
	loss := opt.loss()
	out.Choices = make([]CompletionChoice, 0, len(r.Choices))
	for i := range r.Choices {
		c := &r.Choices[i]
		wc := CompletionChoice{Index: c.Index, Logprobs: c.Logprobs}
		if same {
			wc.Extra = c.Extra
			wc.ProviderSpecificFields = c.ProviderFields
		}
		wc.Text = c.Message.Content.Flatten()
		for j := range c.Message.Content {
			if c.Message.Content[j].Kind != canonical.KindText {
				// A tool call or an image reached a surface with one string
				// field. It is dropped, and saying so is the whole point of
				// §10.1's structural-downgrade rule.
				loss.Downgrade(canonical.ConstructMultiBlockContent, "choices.text")
				break
			}
		}
		if c.StopReason != canonical.StopUnspecified {
			wc.FinishReason = ptr(FinishReasonOf(c.StopReason))
			native := c.NativeStopReason
			if native == "" && !c.StopReason.Expressible() {
				native = string(c.StopReason)
			}
			if native != "" {
				wc.ProviderSpecificFields = withNativeFinish(wc.ProviderSpecificFields, native)
			}
		}
		out.Choices = append(out.Choices, wc)
	}
	return out, nil
}
