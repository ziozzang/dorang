package backend

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/wire/wirejson"
)

// geminiAdapter is Google's generative-language surface.
//
// This is the adapter that earns the package. The other three wire shapes are
// dialects of one another — a field renamed, a ceiling spelled differently — and
// this one is a different protocol wearing the same words:
//
//   - The MODEL is in the URL and the operation is a suffix after a colon, so
//     endpoint derivation depends on the routing decision rather than on
//     configuration alone.
//   - Messages are `contents`, the assistant's role is `model`, and the system
//     prompt is not a message at all but a sibling object.
//   - Every sampling parameter lives inside `generationConfig`, camelCased.
//   - Tool results are not a role; they are a `functionResponse` part inside a
//     user turn, addressed by tool NAME, because this protocol has no tool-call
//     id at all (see toolNames).
//   - The credential is a header of its own, not a bearer token.
//
// DESIGN §10.7's request table has three columns and a TODO noting that a
// fourth for this family is "noted, not planned". This is that fourth column,
// implemented rather than tabulated.
type geminiAdapter struct{}

// geminiDefaultVersion is the API version segment supplied when a base URL
// names a bare host. The catalogued default already carries it; a deployment
// that overrides base_url with a bare host gets the version the catalog's
// default names, not a guess.
const geminiDefaultVersion = "/v1beta"

func (geminiAdapter) endpoint(p *Provider, op Operation, model string, stream bool) (string, error) {
	if op != OpChat {
		return "", noOperation("gemini", op,
			"this adapter serves generateContent only; embeddings and rerank are separate surfaces with "+
				"different request shapes")
	}
	method := ":generateContent"
	if stream {
		// alt=sse is required. Without it the streaming route answers with a
		// JSON ARRAY delivered in chunks, which is not SSE and which no
		// event-stream reader can consume.
		method = ":streamGenerateContent?alt=sse"
	}
	base := trimBase(p.base)
	if !hasVersionSegment(base) {
		// The same rule [joinVersioned] applies, for the same reason: a base
		// carrying a path but no version segment used to get nothing appended
		// and addressed a route that does not exist.
		base += geminiDefaultVersion
	}
	// The model goes in the path, so it is escaped: a name is opaque (§2.1) and
	// may contain characters a path segment reserves.
	return base + "/models/" + url.PathEscape(model) + method, nil
}

// credential is a vendor header rather than a bearer token.
//
// The key can also travel as a ?key= query parameter, and it deliberately does
// not: a URL is the one part of a request that ends up in access logs, proxy
// logs, error messages and traces, and a credential there outlives every
// precaution this package takes.
func (geminiAdapter) credential(secret string, h http.Header) { h.Set("x-goog-api-key", secret) }

func (geminiAdapter) headers(http.Header) {}

func (geminiAdapter) encode(x *exchange) ([]byte, error) {
	req, err := encodeGemini(x.req)
	if err != nil {
		return nil, err
	}
	return wirejson.Marshal(req)
}

func (geminiAdapter) decode(body []byte, x *exchange) (*decoded, error) {
	var w geminiResponse
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, err
	}
	if !isGeminiResponse(&w) {
		return nil, errNotAResponse
	}
	return &decoded{resp: geminiToCanonical(&w, x.call.Model)}, nil
}

// isGeminiResponse reports whether a decoded body is a GenerateContentResponse.
//
// The same rule the wire decoders apply, on the family that needed it least
// obviously and had it just as badly: a vendor's 200-wrapped error envelope
// unmarshalled into a zero-valued [geminiResponse] and left through the chat
// encoder as `{"object":"chat.completion","choices":[],…}` with a synthesized
// `chatcmpl-` id — an answer to a question this upstream never saw, in a
// protocol it does not even speak.
//
// This family defines no discriminator either, so the test is the presence of a
// payload-bearing member. There are three, not two, and the third is the
// interesting one:
//
//   - `candidates` — the generated turns.
//   - `usageMetadata` — what they cost.
//   - `promptFeedback` — the reason there are NO candidates. A prompt this
//     family's safety filter blocked answers with feedback and nothing else,
//     and that is a real answer: the request was received, evaluated and
//     refused. Refusing it here would report a working safety filter as a
//     broken gateway, so it is accepted and rendered as the empty turn it is.
//
// Presence, not value: `{"candidates":[]}` is accepted for the same reason
// `{"choices":[]}` is on the chat surface.
func isGeminiResponse(w *geminiResponse) bool {
	return w != nil &&
		(w.Candidates != nil || w.UsageMetadata != nil || len(w.PromptFeedback) > 0)
}

func (geminiAdapter) source(r io.Reader, x *exchange) (eventSource, error) {
	return &geminiSource{br: bufio.NewReaderSize(r, 8<<10), model: x.call.Model}, nil
}

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

type geminiRequest struct {
	Contents          []geminiContent   `json:"contents"`
	SystemInstruction *geminiContent    `json:"systemInstruction,omitempty"`
	GenerationConfig  *geminiGenConfig  `json:"generationConfig,omitempty"`
	Tools             []geminiTool      `json:"tools,omitempty"`
	ToolConfig        *geminiToolConfig `json:"toolConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	InlineData       *geminiBlob             `json:"inlineData,omitempty"`
	FileData         *geminiFileData         `json:"fileData,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
	// Thought marks a reasoning part. It is read and never written: a thought
	// this gateway did not receive from this model is one it must not claim.
	Thought bool `json:"thought,omitempty"`
}

type geminiBlob struct {
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data"`
}

type geminiFileData struct {
	MimeType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri"`
}

type geminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type geminiFunctionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type geminiGenConfig struct {
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"topP,omitempty"`
	TopK             *int            `json:"topK,omitempty"`
	MaxOutputTokens  *int            `json:"maxOutputTokens,omitempty"`
	CandidateCount   *int            `json:"candidateCount,omitempty"`
	StopSequences    []string        `json:"stopSequences,omitempty"`
	Seed             *int64          `json:"seed,omitempty"`
	ResponseMimeType string          `json:"responseMimeType,omitempty"`
	ResponseSchema   json.RawMessage `json:"responseSchema,omitempty"`
	ThinkingConfig   *geminiThinking `json:"thinkingConfig,omitempty"`
}

type geminiThinking struct {
	ThinkingBudget  *int  `json:"thinkingBudget,omitempty"`
	IncludeThoughts *bool `json:"includeThoughts,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDecl `json:"functionDeclarations,omitempty"`
}

type geminiFunctionDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type geminiToolConfig struct {
	FunctionCallingConfig geminiCallingConfig `json:"functionCallingConfig"`
}

type geminiCallingConfig struct {
	Mode                 string   `json:"mode,omitempty"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

type geminiResponse struct {
	Candidates    []geminiCandidate `json:"candidates"`
	UsageMetadata *geminiUsage      `json:"usageMetadata,omitempty"`
	ModelVersion  string            `json:"modelVersion,omitempty"`
	ResponseID    string            `json:"responseId,omitempty"`
	// PromptFeedback is why a request produced no candidates at all — the
	// family's safety filter refusing the PROMPT rather than the completion.
	// It is kept raw and never converted: nothing in the neutral form expresses
	// it. It is modelled solely so [isGeminiResponse] can see that a body with
	// no candidates is nonetheless an answer, which is the difference between
	// reporting a working filter as an empty turn and reporting it as a 502.
	PromptFeedback json.RawMessage `json:"promptFeedback,omitempty"`
}

type geminiCandidate struct {
	Content      *geminiContent `json:"content,omitempty"`
	FinishReason string         `json:"finishReason,omitempty"`
	Index        int            `json:"index"`
}

// geminiUsage is this family's usage object.
//
// The two breakdowns are POINTERS and the two totals are not, because this
// vendor OMITS a breakdown it has nothing to say about: a turn with no cache hit
// carries no cachedContentTokenCount at all, and a non-thinking model carries no
// thoughtsTokenCount. Absence and a measured zero are different facts and a
// billing integration prices them differently (see [canonical.UsageField]), so
// the difference has to survive the decode — an int would collapse it, and then
// [geminiUsageToCanonical] would have to choose between losing every measured
// zero and inventing one on every deployment that has no cache.
type geminiUsage struct {
	PromptTokenCount        int  `json:"promptTokenCount"`
	CandidatesTokenCount    int  `json:"candidatesTokenCount"`
	CachedContentTokenCount *int `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      *int `json:"thoughtsTokenCount"`
	TotalTokenCount         int  `json:"totalTokenCount"`
}

// ---------------------------------------------------------------------------
// Request
// ---------------------------------------------------------------------------

// encodeGemini converts the neutral request.
func encodeGemini(req *canonical.Request) (*geminiRequest, error) {
	if req == nil {
		return nil, errNilRequest
	}
	out := &geminiRequest{}

	// A tool result addresses the tool by NAME here, and the neutral form
	// addresses it by the id the model issued. The mapping is in the
	// conversation itself: every tool_use block that precedes the result names
	// both. Reading it from the request is exact, where deriving a name from an
	// id would only be a convention.
	names := toolNames(req.Messages)

	if sys := systemText(req); sys != "" {
		out.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: sys}}}
	}

	for i := range req.Messages {
		m := &req.Messages[i]
		switch m.Role {
		case canonical.RoleSystem, canonical.RoleDeveloper:
			// Folded into systemInstruction above.
			continue
		}
		parts := geminiParts(m, names)
		if len(parts) == 0 {
			continue
		}
		role := "user"
		if m.Role == canonical.RoleAssistant {
			role = "model"
		}
		// Consecutive turns of the same role are merged: this protocol expects
		// them to alternate, and a split assistant turn (text, then a tool call)
		// is ordinary in the neutral form.
		if n := len(out.Contents); n > 0 && out.Contents[n-1].Role == role {
			out.Contents[n-1].Parts = append(out.Contents[n-1].Parts, parts...)
			continue
		}
		out.Contents = append(out.Contents, geminiContent{Role: role, Parts: parts})
	}
	if out.Contents == nil {
		out.Contents = []geminiContent{}
	}

	out.GenerationConfig = geminiConfig(req)
	out.Tools, out.ToolConfig = geminiTools(req)
	return out, nil
}

// systemText flattens the neutral system prompt.
//
// The per-block attributes of a structured system prompt — cache breakpoints
// above all — have no expression here. That is a structural downgrade
// (CapStructuredSystem), and it is recorded by the capability filter during
// routing rather than invented here.
func systemText(req *canonical.Request) string {
	var b strings.Builder
	for i := range req.System {
		if req.System[i].Kind == canonical.KindText {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(req.System[i].Text)
		}
	}
	for i := range req.Messages {
		m := &req.Messages[i]
		if m.Role != canonical.RoleSystem && m.Role != canonical.RoleDeveloper {
			continue
		}
		if t := m.Content.Flatten(); t != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(t)
		}
	}
	return b.String()
}

// toolNames maps a tool-call id to the name it was issued under.
func toolNames(msgs []canonical.Message) map[string]string {
	var out map[string]string
	for i := range msgs {
		for j := range msgs[i].Content {
			b := &msgs[i].Content[j]
			if b.Kind != canonical.KindToolUse || b.ToolUse == nil {
				continue
			}
			if out == nil {
				out = make(map[string]string, 4)
			}
			out[b.ToolUse.ID] = b.ToolUse.Name
		}
	}
	return out
}

// geminiParts converts one message's blocks.
func geminiParts(m *canonical.Message, names map[string]string) []geminiPart {
	parts := make([]geminiPart, 0, len(m.Content))
	for i := range m.Content {
		b := &m.Content[i]
		switch b.Kind {
		case canonical.KindText:
			if b.Text != "" {
				parts = append(parts, geminiPart{Text: b.Text})
			}
		case canonical.KindThinking:
			// Not re-sent. This protocol's reasoning parts carry a signature
			// dorang cannot produce for text that came from somewhere else, and
			// §10.2 forbids fabricating one.
		case canonical.KindImage, canonical.KindDocument:
			if b.Source == nil {
				continue
			}
			switch b.Source.Kind {
			case canonical.SourceURL:
				parts = append(parts, geminiPart{FileData: &geminiFileData{
					MimeType: b.Source.MediaType, FileURI: b.Source.Data}})
			case canonical.SourceText:
				parts = append(parts, geminiPart{Text: b.Source.Data})
			default:
				parts = append(parts, geminiPart{InlineData: &geminiBlob{
					MimeType: b.Source.MediaType, Data: b.Source.Data}})
			}
		case canonical.KindToolUse:
			if b.ToolUse == nil {
				continue
			}
			parts = append(parts, geminiPart{FunctionCall: &geminiFunctionCall{
				Name: b.ToolUse.Name, Args: rawObject(b.ToolUse.Input)}})
		case canonical.KindToolResult:
			if b.ToolResult == nil {
				continue
			}
			name := names[b.ToolResult.ToolUseID]
			if name == "" {
				// A conversation that arrived without the call it answers. The
				// id is the only name there is, and sending it is better than
				// dropping the result the model is waiting for.
				name = b.ToolResult.ToolUseID
			}
			parts = append(parts, geminiPart{FunctionResponse: &geminiFunctionResponse{
				Name: name, Response: toolResultObject(b.ToolResult),
			}})
		}
	}
	return parts
}

// toolResultObject wraps a tool result, which this protocol requires to be a
// JSON OBJECT. A result that is already one is passed through; anything else —
// a string, a number, a list of blocks — is wrapped under "output", which is
// the member the vendor's own SDK uses for exactly this case.
func toolResultObject(r *canonical.ToolResult) json.RawMessage {
	text := canonical.Content(r.Content).Flatten()
	if t := strings.TrimSpace(text); len(t) > 0 && t[0] == '{' && json.Valid([]byte(t)) {
		return json.RawMessage(t)
	}
	key := "output"
	if r.IsError {
		key = "error"
	}
	enc, err := json.Marshal(text)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	out := make([]byte, 0, len(enc)+len(key)+6)
	out = append(out, '{', '"')
	out = append(out, key...)
	out = append(out, '"', ':')
	out = append(out, enc...)
	return append(out, '}')
}

// geminiConfig folds the sampling parameters into generationConfig.
func geminiConfig(req *canonical.Request) *geminiGenConfig {
	c := &geminiGenConfig{
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		TopK:          req.TopK,
		StopSequences: req.Stop,
		Seed:          req.Seed,
	}
	if req.MaxTokens != nil {
		c.MaxOutputTokens = req.MaxTokens
	}
	if req.N != nil && *req.N > 1 {
		c.CandidateCount = req.N
	}
	if f := req.ResponseFormat; f != nil {
		switch f.Kind {
		case canonical.FormatJSONObject:
			c.ResponseMimeType = "application/json"
		case canonical.FormatJSONSchema:
			c.ResponseMimeType = "application/json"
			c.ResponseSchema = f.Schema
		}
	}
	if r := req.Reasoning; r != nil {
		t := &geminiThinking{}
		if r.BudgetTokens > 0 {
			b := r.BudgetTokens
			t.ThinkingBudget = &b
		}
		if r.Enabled != nil && !*r.Enabled {
			// Zero is this protocol's "off"; there is no boolean.
			zero := 0
			t.ThinkingBudget = &zero
		}
		if r.Summary != "" && r.Summary != canonical.SummaryNone {
			yes := true
			t.IncludeThoughts = &yes
		}
		if t.ThinkingBudget != nil || t.IncludeThoughts != nil {
			c.ThinkingConfig = t
		}
	}
	if c.empty() {
		return nil
	}
	return c
}

// empty reports whether nothing was folded. It is written out rather than
// compared against a zero value because the struct holds slices, which are not
// comparable — and a == that does not compile is better than one that would
// have silently emitted an empty object.
func (c *geminiGenConfig) empty() bool {
	return c.Temperature == nil && c.TopP == nil && c.TopK == nil &&
		c.MaxOutputTokens == nil && c.CandidateCount == nil && len(c.StopSequences) == 0 &&
		c.Seed == nil && c.ResponseMimeType == "" && len(c.ResponseSchema) == 0 &&
		c.ThinkingConfig == nil
}

// geminiTools converts the tool declarations and the choice constraint.
func geminiTools(req *canonical.Request) ([]geminiTool, *geminiToolConfig) {
	if len(req.Tools) == 0 {
		return nil, nil
	}
	decls := make([]geminiFunctionDecl, 0, len(req.Tools))
	for i := range req.Tools {
		t := &req.Tools[i]
		decls = append(decls, geminiFunctionDecl{
			Name: t.Name, Description: t.Description, Parameters: t.Parameters,
		})
	}
	tools := []geminiTool{{FunctionDeclarations: decls}}

	tc := req.ToolChoice
	if tc == nil {
		return tools, nil
	}
	cfg := geminiCallingConfig{}
	switch tc.Mode {
	case canonical.ToolChoiceNone:
		cfg.Mode = "NONE"
	case canonical.ToolChoiceRequired:
		cfg.Mode = "ANY"
	case canonical.ToolChoiceTool:
		cfg.Mode = "ANY"
		cfg.AllowedFunctionNames = []string{tc.Name}
	default:
		cfg.Mode = "AUTO"
	}
	return tools, &geminiToolConfig{FunctionCallingConfig: cfg}
}

// ---------------------------------------------------------------------------
// Response
// ---------------------------------------------------------------------------

// geminiToCanonical converts a complete answer.
func geminiToCanonical(w *geminiResponse, model string) *canonical.Response {
	out := &canonical.Response{ID: w.ResponseID, Model: model}
	if model == "" {
		out.Model = w.ModelVersion
	}
	out.Usage = geminiUsageToCanonical(w.UsageMetadata)
	out.Choices = make([]canonical.Choice, 0, len(w.Candidates))
	for i := range w.Candidates {
		c := &w.Candidates[i]
		choice := canonical.Choice{Index: c.Index}
		choice.Message.Role = canonical.RoleAssistant
		tools := 0
		if c.Content != nil {
			for j := range c.Content.Parts {
				b, isTool := geminiBlock(&c.Content.Parts[j], c.Index, j)
				if b.Kind == "" {
					continue
				}
				if isTool {
					tools++
				}
				choice.Message.Content = append(choice.Message.Content, b)
			}
		}
		choice.StopReason, choice.NativeStopReason = geminiStopReason(c.FinishReason, tools > 0)
		out.Choices = append(out.Choices, choice)
	}
	return out
}

// geminiBlock converts one part.
func geminiBlock(p *geminiPart, choice, index int) (canonical.Block, bool) {
	switch {
	case p.FunctionCall != nil:
		// This protocol issues no tool-call id, and every client that speaks
		// either of the other two families requires one to correlate a result.
		// The id is synthesized from the position, which is stable for one
		// response, and the NAME is what the request encoder addresses the
		// result by — so nothing depends on this string surviving a round trip.
		id := "call_" + strconv.Itoa(choice) + "_" + strconv.Itoa(index)
		return canonical.ToolUseBlock(id, p.FunctionCall.Name, rawObject(p.FunctionCall.Args)), true
	case p.Thought && p.Text != "":
		return canonical.ThinkingBlock(p.Text, ""), false
	case p.Text != "":
		return canonical.TextBlock(p.Text), false
	case p.InlineData != nil:
		return canonical.ImageBlock(p.InlineData.MimeType, p.InlineData.Data), false
	}
	return canonical.Block{}, false
}

// geminiStopReason maps the terminal reason.
//
// The mapping is not one-to-one and the second return value is why: a reason
// this enumeration cannot express is preserved verbatim (COMPATIBILITY 4.3)
// rather than collapsed onto the nearest neighbour and forgotten.
func geminiStopReason(native string, sawTool bool) (canonical.StopReason, string) {
	switch native {
	case "", "STOP":
		if sawTool {
			// The API reports STOP for a turn that ended in a function call,
			// and a client that branches on the stop reason to decide whether
			// to run a tool would never run one.
			return canonical.StopToolUse, ""
		}
		return canonical.StopEndTurn, ""
	case "MAX_TOKENS":
		return canonical.StopMaxTokens, ""
	case "SAFETY", "PROHIBITED_CONTENT", "BLOCKLIST", "SPII", "IMAGE_SAFETY":
		return canonical.StopSafety, native
	case "RECITATION":
		return canonical.StopRecitation, native
	case "MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL":
		return canonical.StopError, native
	default:
		return canonical.StopEndTurn, native
	}
}

// geminiUsageToCanonical normalizes the token counts.
//
// Two normalizations are load-bearing (DESIGN §10.7):
//
//   - promptTokenCount already INCLUDES the cached prefix, which is dorang's
//     inclusive convention, so it becomes InputTokens unchanged and the cached
//     count is reported beside it rather than subtracted from it.
//   - thoughtsTokenCount is NOT included in candidatesTokenCount, and reasoning
//     is billed as output. So OutputTokens is the sum and ReasoningTokens is
//     the part of it that was reasoning — which keeps the invariant that
//     OutputTokens >= ReasoningTokens and that cost never adds them twice.
//
// A third was missing: which counters the backend actually STATED. This decoder
// recorded none, alone among the adapters, and the cost of that is the defect
// closed for the chat family arriving by another road — internal/wire/openai's
// EncodeUsage emits prompt_tokens_details only for a cache count that is
// positive or reported, so a Gemini turn that served nothing from cache reached
// an OpenAI client with no cache row at all, which reads as "this deployment has
// no cache" rather than "the cache returned nothing this time".
func geminiUsageToCanonical(u *geminiUsage) *canonical.Usage {
	if u == nil {
		return nil
	}
	out := &canonical.Usage{
		InputTokens:  u.PromptTokenCount,
		OutputTokens: u.CandidatesTokenCount,
		Reported:     canonical.UsageInput | canonical.UsageOutput,
	}
	if u.CachedContentTokenCount != nil {
		out.CacheReadTokens = *u.CachedContentTokenCount
		out.Report(canonical.UsageCacheRead)
	}
	if u.ThoughtsTokenCount != nil {
		out.ReasoningTokens = *u.ThoughtsTokenCount
		out.OutputTokens += *u.ThoughtsTokenCount
		out.Report(canonical.UsageReasoning)
	}
	return out
}

// rawObject returns a JSON object, substituting an empty one for nothing.
func rawObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}

// ---------------------------------------------------------------------------
// Streaming
// ---------------------------------------------------------------------------

// geminiSource reads a streamGenerateContent SSE stream.
//
// Each frame is a complete GenerateContentResponse holding the increment, not a
// chunk shape of its own, so the same converter serves both paths and there is
// no second mapping to keep in step with the first.
type geminiSource struct {
	br    *bufio.Reader
	data  strings.Builder
	model string
	done  bool
	start bool
	// tools counts function calls already emitted, which is the index a
	// streaming tool-call delta is required to carry (COMPATIBILITY 5.1).
	tools int
}

func (s *geminiSource) next() ([]canonical.StreamEvent, error) {
	for {
		if s.done {
			return nil, io.EOF
		}
		payload, err := nextSSEData(s.br, &s.data, maxStreamFrame)
		if err != nil {
			s.done = true
			return nil, err
		}
		var w geminiResponse
		if err := json.Unmarshal([]byte(payload), &w); err != nil {
			return nil, err
		}
		if ev := s.events(&w); len(ev) > 0 {
			return ev, nil
		}
	}
}

// terminated reports false: this family sends no terminator frame at all — no
// [DONE], no message_stop, the body simply ends. A candidate's finishReason is
// the only end-of-stream marker it defines, and the relay reads that as the stop
// event.
func (s *geminiSource) terminated() bool { return false }

func (s *geminiSource) events(w *geminiResponse) []canonical.StreamEvent {
	var out []canonical.StreamEvent
	if !s.start {
		s.start = true
		out = append(out, canonical.StreamEvent{
			Type: canonical.EventStart, ID: w.ResponseID, Model: s.model,
		})
	}
	for i := range w.Candidates {
		c := &w.Candidates[i]
		var delta canonical.Delta
		sawTool := false
		if c.Content != nil {
			for j := range c.Content.Parts {
				p := &c.Content.Parts[j]
				switch {
				case p.FunctionCall != nil:
					args := ""
					if len(p.FunctionCall.Args) > 0 {
						args = string(p.FunctionCall.Args)
					}
					delta.ToolCalls = append(delta.ToolCalls, canonical.ToolCallDelta{
						Index: s.tools,
						ID:    "call_" + strconv.Itoa(c.Index) + "_" + strconv.Itoa(s.tools),
						Name:  p.FunctionCall.Name,
						// A whole call arrives in one frame here, so the
						// "fragment" is the complete argument object.
						Arguments: args,
						Type:      "function",
					})
					s.tools++
					sawTool = true
				case p.Thought && p.Text != "":
					delta.Content = append(delta.Content, canonical.ThinkingBlock(p.Text, ""))
				case p.Text != "":
					delta.Content = append(delta.Content, canonical.TextBlock(p.Text))
				}
			}
		}
		if len(delta.Content) > 0 || len(delta.ToolCalls) > 0 {
			out = append(out, canonical.StreamEvent{
				Type: canonical.EventDelta, Choice: c.Index, Model: s.model, Delta: delta,
			})
		}
		if c.FinishReason != "" {
			reason, native := geminiStopReason(c.FinishReason, sawTool || s.tools > 0)
			out = append(out, canonical.StreamEvent{
				Type: canonical.EventStop, Choice: c.Index, Model: s.model,
				Delta: canonical.Delta{StopReason: reason, NativeStopReason: native},
			})
		}
	}
	if u := geminiUsageToCanonical(w.UsageMetadata); u != nil && !u.Empty() {
		out = append(out, canonical.StreamEvent{Type: canonical.EventUsage, Model: s.model, Usage: u})
	}
	return out
}
