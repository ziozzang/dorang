// Package tokenest is dorang's prompt-size estimate: the number the router's
// context-window filter reads, and the input half of the budget hold, decided
// before any upstream has been asked to count anything.
//
// # Why the request body's length is the wrong number
//
// The obvious estimate is bytes ÷ constant over the raw request body. It is
// wrong by two orders of magnitude the moment the body carries an image. A
// 2 MB photo base64-encodes to ~2.7 MB of body, which bytes/3 reports as
// ~900,000 tokens; the same image costs a vision model on the order of 1,600.
// The estimate then excludes every deployment whose window it exceeds, so
// ordinary multimodal traffic is refused with a "needs N tokens" number that is
// wrong by several hundred times. DESIGN §10.5a's justification for erring high
// — "an over-estimate costs an unnecessary route to a larger model" — does not
// survive that case: at 900,000 tokens there is no larger model, so the
// over-estimate produces exactly the hard failure it was written to avoid.
//
// So the estimate is structural. Text is counted by script class, an image is
// counted by the rule that governs images rather than by the length of its
// encoding, and only a request dorang never decoded into a typed form falls
// back to counting bytes.
//
// # The direction of the bias
//
// §10.5a requires the estimate to err pessimistic, because an under-estimate is
// invisible: dorang believes the request fits, dispatches it, and context-window
// fallback never fires. bytes/4 was rejected there for under-counting CJK. bytes/3
// is no better in the case that matters: internal/server/json.go fixes dorang's
// serializer as raw UTF-8 with no \uXXXX escaping, and the mainstream clients
// that send raw UTF-8 put Korean on the wire at three bytes per syllable against
// roughly one token per syllable — so bytes/3 lands on the answer with no margin
// at all, which for a rule whose whole purpose is margin is an under-count. A
// client that does escape (Python's json.dumps defaults to ensure_ascii) sends
// six bytes per syllable and only makes the byte rule more conservative, never
// less, so raw UTF-8 is the direction that has to be safe.
//
// Every count below is therefore deliberately above the real tokenizer ratio,
// and [Estimate.Exact] is false on every result this package produces: nothing
// here tokenizes, and DESIGN §15.5 keeps a tokenizer off this path.
//
// # Cost
//
// This runs on the request path, once per request. Every function here is
// integer arithmetic over memory the caller already holds: no allocation, no
// regular expressions, no reflection.
package tokenest

import "github.com/ziozzang/dorang/internal/canonical"

// The methods an [Estimate] can be produced by. They are surfaced to the client
// on a refusal, because a caller told "this needs 900,000 tokens" has no way to
// tell a measurement from a guess otherwise.
const (
	// MethodStructural counted a decoded request field by field.
	MethodStructural = "structural"
	// MethodBytes counted raw body bytes by UTF-8 class. It is the fallback for
	// the surfaces dorang relays without decoding.
	MethodBytes = "bytes"
	// MethodNone is the zero estimate: there was nothing to count.
	MethodNone = "none"
)

// Estimate is a prompt size and where the number came from.
type Estimate struct {
	// Tokens is the estimated prompt size, biased high.
	Tokens int64
	// Method names the rule that produced Tokens, one of the Method constants.
	Method string
	// Exact reports that Tokens is a measurement rather than an estimate. It is
	// false on everything this package returns and exists so that a count taken
	// from an upstream tokenizer can say so through the same type. Even a real
	// tokenizer would leave it false unless it also modelled the backend's own
	// chat framing, which is private to the backend.
	Exact bool
}

// Estimated reports whether e is a guess. It is the predicate an error message
// branches on, so that "estimated" is stated rather than assumed.
func (e Estimate) Estimated() bool { return !e.Exact }

// The per-class byte budgets for text. They are below every mainstream
// tokenizer's real ratio on purpose: three bytes per token for a word run is
// about a third above the ~4 bytes/token that cl100k and o200k achieve on ASCII
// prose, and punctuation is charged at two because a JSON schema or a code block
// is punctuation-dense and merges less than prose does.
const (
	asciiWordBytesPerToken  = 3
	asciiPunctBytesPerToken = 2
	// Whitespace mostly merges into the token beside it. It is still charged,
	// because deep indentation in a pasted file does not merge away entirely.
	asciiSpaceBytesPerToken = 4
	// opaqueBytesPerToken prices material that is not language at all: a
	// thinking signature, a tool-call id, a base64 blob. Four bytes per token is
	// what a byte-pair tokenizer averages on random base64.
	opaqueBytesPerToken = 4
)

// The per-image budgets.
//
// An image's token cost is a function of its pixel dimensions, which are not
// available here — reading them means decoding the payload, which is neither
// arithmetic nor allocation-free. So each image is charged the documented
// ceiling instead: 1600 is the top of Anthropic's (width × height) ÷ 750 rule,
// and comfortably above OpenAI's high-detail 85 + 170 × tiles for the largest
// image that surface accepts. The result is bounded, which is the property the
// encoded length never had.
const (
	imageTokens = 1600
	// imageTokensLowDetail is OpenAI's fixed cost for detail: "low". It is a
	// promise about the request, not a guess: the image is downsampled to one
	// tile whatever it was.
	imageTokensLowDetail = 85
)

// The per-document budgets.
//
// Unlike an image, a document's token cost really does scale with its content,
// and nothing here can tell a 40-page text PDF from a 40-page scan without
// parsing it. The two calibrations disagree by an order of magnitude — a text
// page is ~800 tokens in ~5 KB, a scanned page ~3,000 tokens in ~200 KB — so the
// text calibration is used, because it is the pessimistic one and §10.5a says
// which direction to fail in. A document that is refused for being too large can
// be split by the caller; one that is silently believed to fit cannot be
// recovered from.
const (
	documentBytesPerToken = 6
	// documentFloorTokens is one page. A document referenced by URL or file id
	// is not in the body at all and still costs the model a document.
	documentFloorTokens = 1500
)

// audioBytesPerToken prices an uploaded audio file for the transcription
// surfaces. Speech runs about 150 words a minute whatever the codec, so the
// token count tracks duration and the byte count tracks bitrate; the ratio here
// is taken from the most compressed bitrate in ordinary use, which makes it an
// over-estimate for every less compressed one.
const audioBytesPerToken = 1000

// The framing budgets. Every protocol wraps a turn in role markers and
// separators the caller never sent and the tokenizer still counts, and DESIGN
// §10.5a's fit check has to leave room for them.
const (
	requestFraming = 8
	messageFraming = 8
	blockFraming   = 4
	toolFraming    = 8
)

// The safety margin, applied once to a finished structural count: +10%, in
// integer arithmetic. It is what turns "about right" into "biased high" for the
// scripts where the per-class budgets land close to the real ratio.
const (
	marginNumerator   = 11
	marginDenominator = 10
)

// maxBlockDepth bounds the tool-result recursion. A tool result carries blocks,
// and a block can be another tool result, so the shape is a caller-supplied tree
// on the request path. The bound is a refusal to recurse, not a refusal to
// count: material below it is simply not added, which only ever lowers the
// estimate for a request no real client sends.
const maxBlockDepth = 4

// Request estimates a decoded inference request.
//
// Every field that reaches the model is counted, including the ones a caller
// does not think of as prompt: tool declarations, a response-format schema, and
// the protocol fields dorang did not model and is carrying through verbatim.
func Request(r *canonical.Request) Estimate {
	if r == nil {
		return Estimate{Method: MethodNone}
	}
	n := int64(requestFraming)
	n += content(r.System, 0)

	for i := range r.Messages {
		m := &r.Messages[i]
		n += messageFraming
		n += text(m.Role)
		n += text(m.Name)
		n += text(m.Refusal)
		n += content(m.Content, 0)
		n += extra(m.Extra)
	}

	for i := range r.Tools {
		t := &r.Tools[i]
		n += toolFraming
		n += text(t.Type) + text(t.Name) + text(t.Description)
		n += text(t.Parameters)
		n += extra(t.Extra)
	}
	if r.ToolChoice != nil {
		n += toolFraming + text(r.ToolChoice.Name)
	}
	if f := r.ResponseFormat; f != nil {
		n += toolFraming + text(f.Kind) + text(f.Name) + text(f.Description) + text(f.Schema)
	}
	for _, s := range r.Stop {
		n += text(s)
	}
	if p := r.Prompt; p != nil {
		for _, s := range p.Texts {
			n += text(s)
		}
		for _, ids := range p.Tokens {
			// A token prompt is already tokenized. It is the one input on this
			// path whose count is exact — and it does not make the whole estimate
			// exact, because everything around it is still a guess.
			n += int64(len(ids))
		}
	}
	n += extra(r.Extra)

	return Estimate{Tokens: withMargin(n), Method: MethodStructural}
}

// Rerank estimates a rerank call: a query against a corpus, all of it prompt.
func Rerank(r *canonical.RerankRequest) Estimate {
	if r == nil {
		return Estimate{Method: MethodNone}
	}
	n := int64(requestFraming) + text(r.Query)
	for i := range r.Documents {
		d := &r.Documents[i]
		n += messageFraming + text(d.Text) + text(d.Fields)
	}
	for _, f := range r.RankFields {
		n += text(f)
	}
	n += extra(r.Extra)
	return Estimate{Tokens: withMargin(n), Method: MethodStructural}
}

// Moderation estimates a classification call.
func Moderation(r *canonical.ModerationRequest) Estimate {
	if r == nil {
		return Estimate{Method: MethodNone}
	}
	n := int64(requestFraming)
	for i := range r.Inputs {
		in := &r.Inputs[i]
		n += blockFraming
		if in.Kind == canonical.ModerationImage {
			n += imageSource(in.Source)
			continue
		}
		n += text(in.Text)
	}
	n += extra(r.Extra)
	return Estimate{Tokens: withMargin(n), Method: MethodStructural}
}

// Speech estimates a text-to-speech call. The input text is the whole prompt;
// the audio is the output and costs nothing here.
func Speech(r *canonical.SpeechRequest) Estimate {
	if r == nil {
		return Estimate{Method: MethodNone}
	}
	n := int64(requestFraming) + text(r.Input) + text(r.Instructions) + extra(r.Extra)
	return Estimate{Tokens: withMargin(n), Method: MethodStructural}
}

// Transcription estimates a speech-to-text call.
//
// The uploaded file is audio, not base64 text, and counting its bytes as prompt
// is the same defect as counting an image's: a 20 MB upload is minutes of speech
// and a few thousand tokens, not seven million.
func Transcription(r *canonical.TranscriptionRequest) Estimate {
	if r == nil {
		return Estimate{Method: MethodNone}
	}
	n := int64(requestFraming) + text(r.Prompt)
	n += int64(len(r.File.Data)) / audioBytesPerToken
	return Estimate{Tokens: withMargin(n), Method: MethodStructural}
}

// Image estimates an image generation, edit or variation call.
func Image(r *canonical.ImageRequest) Estimate {
	if r == nil {
		return Estimate{Method: MethodNone}
	}
	n := int64(requestFraming) + text(r.Prompt)
	// An input image is an image whichever direction it is travelling.
	n += int64(len(r.Images)) * imageTokens
	if r.Mask != nil {
		n += imageTokens
	}
	n += extra(r.Extra)
	return Estimate{Tokens: withMargin(n), Method: MethodStructural}
}

// Bytes is the fallback for a body dorang never decoded — the passthrough and
// embeddings paths, where there is no typed request to walk.
//
// It classifies by UTF-8 lead byte rather than dividing the length, so a
// multi-byte script is charged per character instead of per byte: two-byte
// scripts cost more than bytes/3 rather than less, three-byte scripts cost the
// same, and a four-byte sequence — the emoji and astral planes — costs two.
// That is the correction §10.5a asks for, in the direction it asks for.
func Bytes(b []byte) Estimate {
	if len(b) == 0 {
		return Estimate{Method: MethodNone}
	}
	return Estimate{Tokens: withMargin(text(b) + requestFraming), Method: MethodBytes}
}

// withMargin applies the safety margin with ceiling arithmetic, saturating
// rather than overflowing. A body large enough to overflow an int64 token count
// cannot exist, but the saturation costs one comparison and removes the question.
func withMargin(n int64) int64 {
	if n <= 0 {
		return 0
	}
	if n > (1<<62)/marginNumerator {
		return 1 << 62
	}
	return (n*marginNumerator + marginDenominator - 1) / marginDenominator
}

// content sums a block list.
func content(c canonical.Content, depth int) int64 {
	var n int64
	for i := range c {
		n += block(&c[i], depth)
	}
	return n
}

// block sums one content block by its kind.
func block(b *canonical.Block, depth int) int64 {
	n := int64(blockFraming)
	switch b.Kind {
	case canonical.KindText:
		n += text(b.Text)
	case canonical.KindThinking:
		n += text(b.Text)
		if b.Thinking != nil {
			// The signature is echoed verbatim on the next turn and is not
			// language; it is priced as the opaque material it is.
			n += opaque(len(b.Thinking.Signature))
		}
	case canonical.KindImage:
		n += imageSource(b.Source)
	case canonical.KindDocument:
		n += documentSource(b.Source)
	case canonical.KindToolUse:
		if b.ToolUse != nil {
			n += text(b.ToolUse.Name) + text(b.ToolUse.Input) + opaque(len(b.ToolUse.ID))
		}
	case canonical.KindToolResult:
		if b.ToolResult != nil {
			n += opaque(len(b.ToolResult.ToolUseID))
			if depth < maxBlockDepth {
				n += content(b.ToolResult.Content, depth+1)
			}
		}
	}
	n += extra(b.Extra)
	return n
}

// imageSource is what one image costs.
func imageSource(s *canonical.Source) int64 {
	if s != nil && s.Detail == "low" {
		return imageTokensLowDetail
	}
	return imageTokens
}

// documentSource is what one document costs.
func documentSource(s *canonical.Source) int64 {
	if s == nil {
		return documentFloorTokens
	}
	switch s.Kind {
	case canonical.SourceText:
		// An inline text document is text. It is counted as such rather than by
		// the page rule, which would be a wild over-estimate for a short one.
		return text(s.Data)
	case canonical.SourceBase64:
		n := int64(len(s.Data)) / 4 * 3 / documentBytesPerToken
		if n < documentFloorTokens {
			return documentFloorTokens
		}
		return n
	default:
		// A URL or a file id: the bytes are not here, the document still is.
		return documentFloorTokens
	}
}

// opaque prices non-language material by length.
func opaque(n int) int64 {
	if n <= 0 {
		return 0
	}
	return int64(n+opaqueBytesPerToken-1) / opaqueBytesPerToken
}

// extra sums the unmodelled JSON dorang is carrying through. It is counted as
// text, because that is what it usually is and because under-counting is the
// direction §10.5a forbids.
func extra[V ~string | ~[]byte](m map[string]V) int64 {
	var n int64
	for k, v := range m {
		n += text(k) + text(v)
	}
	return n
}
