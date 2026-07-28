package canonical

import "encoding/json"

// Role identifies who authored a message.
//
// The set is the union of the two protocols. "developer" exists only on the
// OpenAI side and "system" only survives as a top-level field on the Anthropic
// side; an encoder folds whichever it cannot carry.
type Role string

const (
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	// RoleTool carries tool results. On the OpenAI wire one tool message holds
	// exactly one result; on the Anthropic wire a single user message holds
	// several. Canonically a tool message holds one or more tool_result blocks
	// and the encoder splits or merges.
	RoleTool Role = "tool"
)

// BlockKind discriminates a [Block].
type BlockKind string

const (
	KindText       BlockKind = "text"
	KindImage      BlockKind = "image"
	KindDocument   BlockKind = "document"
	KindToolUse    BlockKind = "tool_use"
	KindToolResult BlockKind = "tool_result"
	KindThinking   BlockKind = "thinking"
)

// Block is one element of a message's ordered content.
//
// Exactly one of the kind-specific fields is meaningful, selected by Kind. The
// struct is a discriminated union rather than an interface so that encoders can
// switch on Kind without a type assertion and so that a []Block is one
// allocation rather than one per element.
type Block struct {
	Kind BlockKind

	// Text is the text of a KindText block and the visible reasoning of a
	// KindThinking block.
	Text string

	// Source is the payload of a KindImage or KindDocument block.
	Source *Source

	// ToolUse is set on a KindToolUse block.
	ToolUse *ToolUse

	// ToolResult is set on a KindToolResult block.
	ToolResult *ToolResult

	// Thinking carries the integrity material of a KindThinking block, when the
	// originating protocol attached any. dorang never fabricates or re-signs one
	// (DESIGN §10.2): it is stored and replayed byte-identically or not at all.
	Thinking *Thinking

	// CacheControl marks this block as a prompt-cache breakpoint. It exists in
	// one protocol and not the other, which is precisely why it lives on the
	// neutral type: an encoder that cannot express it must report a structural
	// downgrade rather than drop it silently.
	CacheControl *CacheControl

	// Extra carries protocol fields dorang does not model, so a same-protocol
	// round trip is not lossy.
	Extra map[string]json.RawMessage
}

// SourceKind says how a [Source] carries its bytes.
type SourceKind string

const (
	SourceBase64 SourceKind = "base64"
	SourceURL    SourceKind = "url"
	SourceFileID SourceKind = "file_id"
	// SourceText is a document supplied inline as plain text rather than as an
	// encoded file.
	SourceText SourceKind = "text"
)

// Source is the payload of an image or document block.
type Source struct {
	Kind SourceKind
	// MediaType is an IANA media type ("image/png", "application/pdf"). It may
	// be empty for SourceURL, where the fetcher determines it.
	MediaType string
	// Data is the base64 payload, the URL, the file id, or the inline text,
	// according to Kind.
	Data string
	// Name is a document title or filename, when the protocol carries one.
	Name string
	// Detail is the OpenAI image detail hint ("auto", "low", "high").
	Detail string
}

// ToolUse is a model-issued call to a tool.
type ToolUse struct {
	// ID is the call id. Cross-protocol ids are normalized consistently in both
	// directions (COMPATIBILITY 5.4).
	ID string
	// Name is the tool name as the *caller* declared it, at full length. Wire
	// encoders that impose a length limit shorten it reversibly and restore it
	// on the way back (COMPATIBILITY 5.3).
	Name string
	// Input is the argument object as JSON. It is kept raw because re-encoding
	// a decoded map reorders keys, and a model that emitted a specific ordering
	// on turn one must see the same bytes echoed on turn two.
	Input json.RawMessage
}

// ToolResult is the caller's answer to a [ToolUse].
//
// Content is a list of blocks, not a string, because the Anthropic form allows
// text plus images inside a single result. Flattening it to a string is a
// structural downgrade ([CapMultiBlockToolResult]), not a formatting detail.
type ToolResult struct {
	ToolUseID string
	Content   []Block
	IsError   bool
}

// Thinking carries a reasoning block's non-text material.
type Thinking struct {
	// Signature is integrity material that must be echoed verbatim on the next
	// turn of a tool-use exchange. dorang stores it and replays it; it never
	// synthesizes one for a backend that did not produce it.
	Signature string
	// Redacted marks a block whose text the provider withheld.
	Redacted bool
	// Handle is dorang's opaque key for a stored block, when the block was
	// retained rather than carried inline.
	Handle string
}

// CacheControl marks a prompt-cache breakpoint.
type CacheControl struct {
	// Type is the breakpoint kind. "ephemeral" is the only value in use.
	Type string
	// TTL is a provider-specific lifetime hint ("5m", "1h"). Empty means the
	// provider default.
	TTL string
}

// Content is a message's ordered block list.
type Content []Block

// Plain returns the content as a single string, and whether that is lossless.
//
// It is lossless only when the content is exactly one text block carrying no
// cache breakpoint. Every other shape needs the array form, so an encoder that
// can only emit a string must record a structural downgrade.
func (c Content) Plain() (string, bool) {
	if len(c) == 1 && c[0].Kind == KindText && c[0].CacheControl == nil && len(c[0].Extra) == 0 {
		return c[0].Text, true
	}
	return c.Flatten(), false
}

// Flatten concatenates every text block, ignoring everything else. It is the
// lossy fallback and callers must record the loss.
func (c Content) Flatten() string {
	switch len(c) {
	case 0:
		return ""
	case 1:
		if c[0].Kind == KindText {
			return c[0].Text
		}
	}
	n := 0
	for i := range c {
		if c[i].Kind == KindText {
			n += len(c[i].Text)
		}
	}
	if n == 0 {
		return ""
	}
	out := make([]byte, 0, n)
	for i := range c {
		if c[i].Kind == KindText {
			out = append(out, c[i].Text...)
		}
	}
	return string(out)
}

// Message is one turn of the conversation.
type Message struct {
	Role Role
	// Name is the OpenAI participant name. It has no Anthropic equivalent and is
	// a droppable parameter, not a structural construct.
	Name    string
	Content Content
	// Refusal is the model's refusal text, which both protocols carry but in
	// different places — a sibling of content in one, a stop reason in the
	// other. Keeping it out of Content means flattening to a string never turns
	// a refusal into ordinary assistant text.
	Refusal string
	Extra   map[string]json.RawMessage
}

// TextBlock builds a plain text block.
func TextBlock(s string) Block { return Block{Kind: KindText, Text: s} }

// ImageBlock builds an image block from a base64 payload.
func ImageBlock(mediaType, data string) Block {
	return Block{Kind: KindImage, Source: &Source{Kind: SourceBase64, MediaType: mediaType, Data: data}}
}

// ImageURLBlock builds an image block referencing a URL. A data: URL is
// recognized and split into its media type and payload so that a protocol
// without URL sources can still carry it.
func ImageURLBlock(url string) Block {
	if mt, data, ok := splitDataURL(url); ok {
		return ImageBlock(mt, data)
	}
	return Block{Kind: KindImage, Source: &Source{Kind: SourceURL, Data: url}}
}

// DocumentBlock builds a document block from a base64 payload.
func DocumentBlock(mediaType, data, name string) Block {
	return Block{Kind: KindDocument, Source: &Source{Kind: SourceBase64, MediaType: mediaType, Data: data, Name: name}}
}

// ToolUseBlock builds a tool call block.
func ToolUseBlock(id, name string, input json.RawMessage) Block {
	return Block{Kind: KindToolUse, ToolUse: &ToolUse{ID: id, Name: name, Input: input}}
}

// ToolResultBlock builds a tool result block.
func ToolResultBlock(id string, content ...Block) Block {
	return Block{Kind: KindToolResult, ToolResult: &ToolResult{ToolUseID: id, Content: content}}
}

// ThinkingBlock builds a reasoning block.
func ThinkingBlock(text, signature string) Block {
	b := Block{Kind: KindThinking, Text: text}
	if signature != "" {
		b.Thinking = &Thinking{Signature: signature}
	}
	return b
}

// TextMessage builds a message whose content is one text block.
func TextMessage(role Role, text string) Message {
	return Message{Role: role, Content: Content{TextBlock(text)}}
}

// splitDataURL recognizes "data:<media-type>;base64,<payload>".
//
// It scans rather than using a regular expression, which the hot path forbids
// (DESIGN §15.5).
func splitDataURL(s string) (mediaType, data string, ok bool) {
	const prefix = "data:"
	const marker = ";base64,"
	if len(s) < len(prefix) || s[:len(prefix)] != prefix {
		return "", "", false
	}
	rest := s[len(prefix):]
	for i := 0; i+len(marker) <= len(rest); i++ {
		if rest[i:i+len(marker)] == marker {
			return rest[:i], rest[i+len(marker):], true
		}
	}
	return "", "", false
}

// DataURL is the inverse of splitDataURL.
func (s *Source) DataURL() string {
	if s == nil {
		return ""
	}
	if s.Kind == SourceURL {
		return s.Data
	}
	if s.MediaType == "" {
		return s.Data
	}
	return "data:" + s.MediaType + ";base64," + s.Data
}
