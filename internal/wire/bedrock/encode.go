// Package bedrock speaks Amazon Bedrock's Converse API: the one request and
// response shape Bedrock serves every model family through, and its binary
// event stream.
//
// # Why Converse and not InvokeModel
//
// InvokeModel forwards each vendor's native body, so a gateway using it would
// need one encoder per model family behind one endpoint. Converse is Bedrock's
// own neutral form — messages, content blocks, tool config, inference config —
// and it is close enough to dorang's that the crossing is a translation rather
// than a rewrite. What Converse cannot express (logprobs, seeds, penalties,
// several choices, a JSON schema on the answer) is reported as lost by the
// same [canonical.LossReport] every other family uses; what it expresses
// through `additionalModelRequestFields` (a reasoning budget, on the Anthropic
// models) is written there.
package bedrock

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ziozzang/dorang/internal/canonical"
)

// DefaultCapabilities is what Converse can carry. Absent on purpose:
// CapJSONSchema (no response format on this surface), CapSeed, CapLogitBias,
// CapLogprobs, CapPenalties, CapMultipleChoices, CapUser, CapServiceTier,
// CapPriority — each a parameter Converse has no member for, and each reported
// when dropped. CapTopK is absent too: Converse takes it only through the
// model-specific additional fields, and a field one family accepts is a 400 on
// the next.
const DefaultCapabilities = canonical.CapMultiBlockContent | canonical.CapImageBlocks |
	canonical.CapDocumentBlocks | canonical.CapCacheBreakpoints | canonical.CapThinkingBlocks |
	canonical.CapMultiBlockToolResult | canonical.CapStructuredSystem | canonical.CapRichStopReasons |
	canonical.CapToolCalls | canonical.CapParallelToolCalls | canonical.CapReasoningControl |
	canonical.CapStopSequences | canonical.CapMetadata

// EncodeOptions configures [MarshalRequest].
type EncodeOptions struct {
	// Capabilities is the set the request is encoded against; zero takes
	// [DefaultCapabilities].
	Capabilities canonical.Capability
	// Loss receives every parameter and construct that could not cross.
	Loss *canonical.LossReport
	// DefaultMaxTokens fills inferenceConfig.maxTokens when the caller set
	// none. Converse does not require one; zero leaves it to the model.
	DefaultMaxTokens int
	// DefaultReasoningBudget is the thinking budget written when the caller
	// enabled reasoning without naming one. Zero takes 1024, the smallest
	// budget the Anthropic models accept.
	DefaultReasoningBudget int
}

func (o *EncodeOptions) caps() canonical.Capability {
	if o == nil || o.Capabilities == 0 {
		return DefaultCapabilities
	}
	return o.Capabilities
}

func (o *EncodeOptions) loss() *canonical.LossReport {
	if o == nil || o.Loss == nil {
		return &canonical.LossReport{}
	}
	return o.Loss
}

// Request is the Converse request body.
type Request struct {
	Messages                     []Message         `json:"messages"`
	System                       []SystemBlock     `json:"system,omitempty"`
	InferenceConfig              *InferenceConfig  `json:"inferenceConfig,omitempty"`
	ToolConfig                   *ToolConfig       `json:"toolConfig,omitempty"`
	AdditionalModelRequestFields map[string]any    `json:"additionalModelRequestFields,omitempty"`
	RequestMetadata              map[string]string `json:"requestMetadata,omitempty"`
}

// Message is one turn. Converse knows two roles.
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// SystemBlock is one member of `system`.
type SystemBlock struct {
	Text       string      `json:"text,omitempty"`
	CachePoint *CachePoint `json:"cachePoint,omitempty"`
}

// ContentBlock is a union: exactly one member is set.
type ContentBlock struct {
	Text             string            `json:"text,omitempty"`
	Image            *ImageBlock       `json:"image,omitempty"`
	Document         *DocumentBlock    `json:"document,omitempty"`
	ToolUse          *ToolUseBlock     `json:"toolUse,omitempty"`
	ToolResult       *ToolResultBlock  `json:"toolResult,omitempty"`
	ReasoningContent *ReasoningContent `json:"reasoningContent,omitempty"`
	CachePoint       *CachePoint       `json:"cachePoint,omitempty"`
	JSON             json.RawMessage   `json:"json,omitempty"`
}

type CachePoint struct {
	Type string `json:"type"`
}

type ImageBlock struct {
	Format string      `json:"format"`
	Source BytesSource `json:"source"`
}

type DocumentBlock struct {
	Format string      `json:"format"`
	Name   string      `json:"name"`
	Source BytesSource `json:"source"`
}

// BytesSource carries base64 bytes, which is how the JSON binding of Converse
// spells binary content.
type BytesSource struct {
	Bytes string `json:"bytes"`
}

type ToolUseBlock struct {
	ToolUseID string          `json:"toolUseId"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

type ToolResultBlock struct {
	ToolUseID string         `json:"toolUseId"`
	Content   []ContentBlock `json:"content"`
	Status    string         `json:"status,omitempty"`
}

// ReasoningContent is a union of reasoningText and redactedContent.
type ReasoningContent struct {
	ReasoningText   *ReasoningText `json:"reasoningText,omitempty"`
	RedactedContent string         `json:"redactedContent,omitempty"`
}

type ReasoningText struct {
	Text      string `json:"text"`
	Signature string `json:"signature,omitempty"`
}

type InferenceConfig struct {
	MaxTokens     *int     `json:"maxTokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"topP,omitempty"`
	StopSequences []string `json:"stopSequences,omitempty"`
}

type ToolConfig struct {
	Tools      []Tool      `json:"tools"`
	ToolChoice *ToolChoice `json:"toolChoice,omitempty"`
}

type Tool struct {
	ToolSpec *ToolSpec `json:"toolSpec,omitempty"`
}

type ToolSpec struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema InputSchema `json:"inputSchema"`
}

type InputSchema struct {
	JSON json.RawMessage `json:"json"`
}

// ToolChoice is a union of auto, any and tool.
type ToolChoice struct {
	Auto *struct{}        `json:"auto,omitempty"`
	Any  *struct{}        `json:"any,omitempty"`
	Tool *ToolChoiceNamed `json:"tool,omitempty"`
}

type ToolChoiceNamed struct {
	Name string `json:"name"`
}

// ErrNoMessages is a request with nothing to send.
var ErrNoMessages = errors.New("bedrock: the request carries no messages")

// MarshalRequest renders the neutral request as a Converse body.
func MarshalRequest(req *canonical.Request, opt *EncodeOptions) ([]byte, error) {
	out, err := EncodeRequest(req, opt)
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

// EncodeRequest converts without serialising.
func EncodeRequest(req *canonical.Request, opt *EncodeOptions) (*Request, error) {
	if req == nil {
		return nil, ErrNoMessages
	}
	caps, loss := opt.caps(), opt.loss()
	out := &Request{}

	for _, b := range req.System {
		switch b.Kind {
		case canonical.KindText:
			out.System = append(out.System, SystemBlock{Text: b.Text})
			if b.CacheControl != nil && caps.Has(canonical.CapCacheBreakpoints) {
				out.System = append(out.System, SystemBlock{CachePoint: &CachePoint{Type: "default"}})
			}
		default:
			loss.Downgrade("system."+string(b.Kind), "Converse takes text in system; the block was dropped")
		}
	}

	for _, m := range req.Messages {
		role := "user"
		if m.Role == canonical.RoleAssistant {
			role = "assistant"
		}
		blocks, err := encodeBlocks(m.Content, caps, loss)
		if err != nil {
			return nil, err
		}
		if len(blocks) == 0 {
			continue
		}
		// Converse requires alternating roles and refuses two of the same in a
		// row; consecutive same-role turns are merged, which changes nothing
		// about what the model is told.
		if n := len(out.Messages); n > 0 && out.Messages[n-1].Role == role {
			out.Messages[n-1].Content = append(out.Messages[n-1].Content, blocks...)
			continue
		}
		out.Messages = append(out.Messages, Message{Role: role, Content: blocks})
	}
	if len(out.Messages) == 0 {
		return nil, ErrNoMessages
	}

	ic := &InferenceConfig{Temperature: req.Temperature, TopP: req.TopP}
	switch {
	case req.MaxTokens != nil:
		ic.MaxTokens = req.MaxTokens
	case opt != nil && opt.DefaultMaxTokens > 0:
		v := opt.DefaultMaxTokens
		ic.MaxTokens = &v
	}
	if len(req.Stop) > 0 {
		if caps.Has(canonical.CapStopSequences) {
			ic.StopSequences = req.Stop
		} else {
			loss.DropParam("stop")
		}
	}
	if ic.MaxTokens != nil || ic.Temperature != nil || ic.TopP != nil || len(ic.StopSequences) > 0 {
		out.InferenceConfig = ic
	}

	if len(req.Tools) > 0 && caps.Has(canonical.CapToolCalls) {
		tc := &ToolConfig{}
		for _, t := range req.Tools {
			if t.Type != "" && t.Type != "function" {
				loss.Downgrade("tools."+t.Type, "Converse has no provider-native tool of this type; the tool was dropped")
				continue
			}
			schema := t.Parameters
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tc.Tools = append(tc.Tools, Tool{ToolSpec: &ToolSpec{
				Name: t.Name, Description: t.Description, InputSchema: InputSchema{JSON: schema},
			}})
		}
		if req.ToolChoice != nil {
			switch req.ToolChoice.Mode {
			case canonical.ToolChoiceAuto, "":
				tc.ToolChoice = &ToolChoice{Auto: &struct{}{}}
			case canonical.ToolChoiceRequired:
				tc.ToolChoice = &ToolChoice{Any: &struct{}{}}
			case canonical.ToolChoiceTool:
				tc.ToolChoice = &ToolChoice{Tool: &ToolChoiceNamed{Name: req.ToolChoice.Name}}
			case canonical.ToolChoiceNone:
				// No spelling for "declared but do not call". The tools are
				// withheld, which is what "none" asks for on the turn.
				loss.Downgrade("tool_choice.none", "Converse has no tool_choice none; the tools were withheld for this turn")
				tc = nil
			}
		}
		if tc != nil && len(tc.Tools) > 0 {
			out.ToolConfig = tc
		}
	} else if len(req.Tools) > 0 {
		loss.DropCapability(canonical.CapToolCalls)
	}

	if req.Reasoning != nil && (req.Reasoning.Enabled == nil || *req.Reasoning.Enabled) {
		if caps.Has(canonical.CapReasoningControl) {
			budget := req.Reasoning.BudgetTokens
			if budget <= 0 {
				budget = 1024
				if opt != nil && opt.DefaultReasoningBudget > 0 {
					budget = opt.DefaultReasoningBudget
				}
			}
			out.AdditionalModelRequestFields = map[string]any{
				"thinking": map[string]any{"type": "enabled", "budget_tokens": budget},
			}
		} else {
			loss.DropParam("reasoning")
		}
	}

	if len(req.Metadata) > 0 {
		if caps.Has(canonical.CapMetadata) {
			out.RequestMetadata = req.Metadata
		} else {
			loss.DropParam("metadata")
		}
	}

	// Everything Converse has no member for, by name.
	if req.TopK != nil {
		loss.DropParam("top_k")
	}
	if req.Seed != nil {
		loss.DropParam("seed")
	}
	if len(req.LogitBias) > 0 {
		loss.DropParam("logit_bias")
	}
	if req.Logprobs != nil {
		loss.DropParam("logprobs")
	}
	if req.TopLogprobs != nil {
		loss.DropParam("top_logprobs")
	}
	if req.FrequencyPenalty != nil {
		loss.DropParam("frequency_penalty")
	}
	if req.PresencePenalty != nil {
		loss.DropParam("presence_penalty")
	}
	if req.N != nil && *req.N > 1 {
		loss.DropParam("n")
	}
	if req.User != "" {
		loss.DropParam("user")
	}
	if req.ServiceTier != "" {
		loss.DropParam("service_tier")
	}
	if req.ResponseFormat != nil && req.ResponseFormat.Kind != "" {
		loss.Downgrade("response_format", "Converse has no response format; the model was asked for free text")
	}
	return out, nil
}

func encodeBlocks(content canonical.Content, caps canonical.Capability, loss *canonical.LossReport) ([]ContentBlock, error) {
	var out []ContentBlock
	for _, b := range content {
		switch b.Kind {
		case canonical.KindText:
			if b.Text != "" {
				out = append(out, ContentBlock{Text: b.Text})
			}
		case canonical.KindImage:
			blk, ok := imageBlock(b, loss)
			if ok {
				out = append(out, blk)
			}
		case canonical.KindDocument:
			blk, ok := documentBlock(b, loss)
			if ok {
				out = append(out, blk)
			}
		case canonical.KindToolUse:
			if b.ToolUse == nil {
				continue
			}
			input := b.ToolUse.Input
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			out = append(out, ContentBlock{ToolUse: &ToolUseBlock{ToolUseID: b.ToolUse.ID, Name: b.ToolUse.Name, Input: input}})
		case canonical.KindToolResult:
			if b.ToolResult == nil {
				continue
			}
			tr := &ToolResultBlock{ToolUseID: b.ToolResult.ToolUseID}
			if b.ToolResult.IsError {
				tr.Status = "error"
			}
			inner, err := encodeBlocks(b.ToolResult.Content, caps, loss)
			if err != nil {
				return nil, err
			}
			if len(inner) == 0 {
				// A tool that returned nothing. `{"text":""}` would serialise
				// to `{}` under omitempty — no union member selected, which
				// Converse rejects — so an empty JSON string is used: a valid
				// `json` member carrying nothing.
				inner = []ContentBlock{{JSON: json.RawMessage(`""`)}}
			}
			tr.Content = inner
			out = append(out, ContentBlock{ToolResult: tr})
		case canonical.KindThinking:
			if !caps.Has(canonical.CapThinkingBlocks) {
				loss.Downgrade("thinking", "the deployment does not carry thinking blocks; the block was dropped")
				continue
			}
			rc := &ReasoningContent{}
			if b.Thinking != nil && b.Thinking.Redacted {
				rc.RedactedContent = b.Text
			} else {
				rt := &ReasoningText{Text: b.Text}
				if b.Thinking != nil {
					rt.Signature = b.Thinking.Signature
				}
				rc.ReasoningText = rt
			}
			out = append(out, ContentBlock{ReasoningContent: rc})
		default:
			loss.Downgrade(string(b.Kind), "Converse has no block of this kind; it was dropped")
			continue
		}
		if b.CacheControl != nil && caps.Has(canonical.CapCacheBreakpoints) {
			out = append(out, ContentBlock{CachePoint: &CachePoint{Type: "default"}})
		}
	}
	return out, nil
}

func imageBlock(b canonical.Block, loss *canonical.LossReport) (ContentBlock, bool) {
	if b.Source == nil || b.Source.Kind != canonical.SourceBase64 {
		loss.Downgrade("image.url", "Converse takes image bytes, not a URL; the image was dropped")
		return ContentBlock{}, false
	}
	format := strings.TrimPrefix(strings.ToLower(b.Source.MediaType), "image/")
	if format == "jpg" {
		format = "jpeg"
	}
	switch format {
	case "png", "jpeg", "gif", "webp":
	default:
		loss.Downgrade("image."+format, "Converse takes png, jpeg, gif and webp; the image was dropped")
		return ContentBlock{}, false
	}
	return ContentBlock{Image: &ImageBlock{Format: format, Source: BytesSource{Bytes: b.Source.Data}}}, true
}

func documentBlock(b canonical.Block, loss *canonical.LossReport) (ContentBlock, bool) {
	if b.Source == nil || b.Source.Kind != canonical.SourceBase64 {
		loss.Downgrade("document.url", "Converse takes document bytes, not a URL; the document was dropped")
		return ContentBlock{}, false
	}
	format := documentFormat(b.Source.MediaType)
	if format == "" {
		loss.Downgrade("document."+b.Source.MediaType, "Converse has no document format for this media type; the document was dropped")
		return ContentBlock{}, false
	}
	name := "document"
	if v, ok := b.Extra["name"]; ok {
		var s string
		if json.Unmarshal(v, &s) == nil && s != "" {
			name = s
		}
	}
	return ContentBlock{Document: &DocumentBlock{Format: format, Name: name, Source: BytesSource{Bytes: b.Source.Data}}}, true
}

func documentFormat(mediaType string) string {
	switch strings.ToLower(mediaType) {
	case "application/pdf":
		return "pdf"
	case "text/plain":
		return "txt"
	case "text/markdown":
		return "md"
	case "text/html":
		return "html"
	case "text/csv":
		return "csv"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return "docx"
	case "application/msword":
		return "doc"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return "xlsx"
	case "application/vnd.ms-excel":
		return "xls"
	}
	return ""
}

// String renders a request for logs with no content.
func (r *Request) String() string {
	return fmt.Sprintf("bedrock.Request{%d messages}", len(r.Messages))
}
