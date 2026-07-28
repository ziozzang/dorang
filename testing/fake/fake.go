package fake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// Shape selects which wire contract the upstream speaks.
type Shape uint8

const (
	// ShapeOpenAI serves POST /v1/chat/completions.
	ShapeOpenAI Shape = iota
	// ShapeAnthropic serves POST /v1/messages.
	ShapeAnthropic
)

func (s Shape) String() string {
	switch s {
	case ShapeAnthropic:
		return "anthropic"
	default:
		return "openai"
	}
}

// Path is the endpoint this shape serves.
func (s Shape) Path() string {
	switch s {
	case ShapeAnthropic:
		return "/v1/messages"
	default:
		return "/v1/chat/completions"
	}
}

// Usage is one response's token accounting, in the INCLUSIVE convention of
// internal/canonical: InputTokens is the full prompt count with cache reads and
// cache writes included.
//
// The Anthropic wire is exclusive, so [Upstream] subtracts on the way out
// (COMPATIBILITY 6.7). Storing the inclusive number here means a test states one
// set of figures and both shapes render it correctly, which is the only way the
// self-check can compare the two encoders on equal terms.
type Usage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	ReasoningTokens  int
}

// Total is inclusive input plus output.
func (u Usage) Total() int { return u.InputTokens + u.OutputTokens }

// ExclusiveInput is the Anthropic wire's input_tokens: the prompt count with
// cache reads and writes removed, clamped at zero (COMPATIBILITY 6.7).
func (u Usage) ExclusiveInput() int {
	n := u.InputTokens - u.CacheReadTokens - u.CacheWriteTokens
	if n < 0 {
		return 0
	}
	return n
}

// ToolCall is one tool call the script emits.
type ToolCall struct {
	ID   string
	Name string
	// Arguments is the complete argument JSON. It is split into ArgChunks
	// fragments on the wire, because a tool call that arrives in one frame does
	// not exercise the accumulator that a real one does.
	Arguments string
	// ArgChunks is how many fragments Arguments is cut into. Zero or one sends
	// it whole; a value larger than len(Arguments) is clamped.
	ArgChunks int
}

// Script is what the upstream produces for one request.
//
// It is content, not bytes: the same Script renders as OpenAI chunks or as
// Anthropic events depending on the upstream's shape.
type Script struct {
	// ID pins the stream identity (COMPATIBILITY 2.4). Empty derives one from
	// the request sequence, which keeps a test deterministic without the test
	// having to name it.
	ID string
	// Created pins the created field. Zero uses a fixed constant, never the
	// wall clock: a golden comparison against a clock is not a comparison.
	Created int64
	// Model is the name the upstream stamps into its response. Empty echoes the
	// model the request carried, which is what a real backend does and is what
	// makes the alias scenario (DESIGN §14.8) meaningful.
	Model string

	// Thinking is reasoning text. It renders as reasoning_content deltas on the
	// OpenAI shape and as a thinking block on the Anthropic one.
	Thinking string
	// Signature is the integrity material attached to a thinking block. It is
	// emitted only on the Anthropic shape, because no OpenAI field carries one.
	Signature string

	// Text is the assistant's visible output.
	Text string
	// TextChunks is how many frames Text is cut into. Zero or one sends it
	// whole.
	TextChunks int

	// ToolCalls are emitted after the text.
	ToolCalls []ToolCall

	// Finish is the backend-native terminal reason. Empty means the backend
	// never sent one, and the terminal frame is SYNTHESIZED — "stop", upgraded
	// to "tool_calls" when a tool call was seen (COMPATIBILITY 4.4). On the
	// Anthropic shape the synthesized values are "end_turn" and "tool_use".
	Finish string

	// Usage is the accounting. It reaches the OpenAI wire only when the request
	// carried stream_options.include_usage (COMPATIBILITY 3.1) and always
	// reaches the Anthropic wire, where the terminal message_delta carries it.
	Usage Usage
}

// DefaultScript is what an upstream with no Options.Script produces.
func DefaultScript(rec *Recorded) Script {
	return Script{
		Text:       "Hello from " + rec.Model,
		TextChunks: 2,
		Usage:      Usage{InputTokens: 12, OutputTokens: 5},
	}
}

// Behaviour is what the upstream does to the response other than its content.
//
// Every field is off at its zero value, so a test names only the failure it is
// exercising.
type Behaviour struct {
	// Latency delays the response headers. It is the "the backend is slow to
	// answer at all" knob.
	Latency time.Duration
	// TTFT delays the FIRST frame after the headers are already out. This is
	// the boundary DESIGN §7.6 refuses to cross with a fallback: once a byte is
	// written the status is 200 and cannot change.
	TTFT time.Duration
	// InterFrame delays every frame after the first — slow generation.
	InterFrame time.Duration

	// Status, when non-zero and not 200, answers with an error envelope and no
	// body content. The response never becomes a stream.
	Status int
	// ErrorType, ErrorMessage and ErrorCode fill the envelope. Empty fields are
	// derived from Status.
	ErrorType    string
	ErrorMessage string
	ErrorCode    string

	// RetryAfter sets the retry-after header, in whole seconds. It is attached
	// whatever the status, because DESIGN §10.4 makes a header the client acts
	// on unconditional.
	RetryAfter time.Duration

	// QuotaExhausted answers 429 with the provider's quota-exhaustion shape:
	// code "insufficient_quota" and the reset headers a caller reads to decide
	// whether to wait. It implies Status 429.
	QuotaExhausted bool
	// QuotaResetAt is the instant the window recovers, rendered into
	// x-ratelimit-reset-requests and anthropic-ratelimit-requests-reset.
	QuotaResetAt time.Time

	// FailAfter delivers a mid-stream failure after N content frames
	// (COMPATIBILITY 1.3): an in-band error frame, then [DONE] on the OpenAI
	// shape and nothing at all after the error event on the Anthropic one.
	// Zero is off; a value of 1 fails after the first content frame.
	FailAfter int
	// TruncateAfter stops writing after N content frames and closes, with no
	// terminal frame, no usage and no [DONE]. This is silent truncation: the
	// client sees a well-formed prefix and has to notice the absence of an
	// ending.
	TruncateAfter int

	// EmptyStream writes an empty body with no [DONE] (COMPATIBILITY 1.4).
	EmptyStream bool
}

func (b Behaviour) failing() bool { return b.QuotaExhausted || (b.Status != 0 && b.Status != 200) }

func (b Behaviour) status() int {
	if b.QuotaExhausted {
		return http.StatusTooManyRequests
	}
	return b.Status
}

// Recorded is one request the upstream received.
//
// It carries the raw bytes as well as the parsed fields, because several
// scenarios assert on what reached the wire rather than on what dorang meant.
type Recorded struct {
	// Seq counts from one.
	Seq int
	// Method and Path are the request line.
	Method string
	Path   string
	// Header is a clone. Nothing here is shared with the live request.
	Header http.Header
	// Body is the verbatim request body.
	Body []byte
	// At is when the request arrived.
	At time.Time

	// Model is the body's model field, an opaque string — nothing splits it
	// (DESIGN §2.1).
	Model string
	// Stream is the body's stream field.
	Stream bool
	// IncludeUsage is stream_options.include_usage EXACTLY true. A truthy value
	// is not enough (COMPATIBILITY 3.1), so this is false for 1, "true" and any
	// other shape.
	IncludeUsage bool
	// MaxTokens is whichever output ceiling the body carried.
	MaxTokens int
	// Priority is the body's priority field, present on the self-hosted engines.
	// PriorityPresent distinguishes "absent" from "zero", which on an ascending
	// engine is the most urgent value there is.
	Priority        int
	PriorityPresent bool
	// ServiceTier is the body's service_tier, when one was sent.
	ServiceTier string
}

// peek pulls the fields a fake backend needs out of a body without modelling
// the whole request. Case-sensitive, because COMPATIBILITY 2.0 makes that a
// security property rather than a style choice: a hand-written scanner and
// every Python backend downstream match "model" and never "Model".
func (r *Recorded) peek() {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(r.Body, &raw); err != nil {
		return
	}
	if v, ok := raw["model"]; ok {
		_ = json.Unmarshal(v, &r.Model)
	}
	if v, ok := raw["stream"]; ok {
		_ = json.Unmarshal(v, &r.Stream)
	}
	if v, ok := raw["max_tokens"]; ok {
		_ = json.Unmarshal(v, &r.MaxTokens)
	}
	if v, ok := raw["max_completion_tokens"]; ok {
		_ = json.Unmarshal(v, &r.MaxTokens)
	}
	if v, ok := raw["service_tier"]; ok {
		_ = json.Unmarshal(v, &r.ServiceTier)
	}
	if v, ok := raw["priority"]; ok {
		if json.Unmarshal(v, &r.Priority) == nil {
			r.PriorityPresent = true
		}
	}
	if v, ok := raw["stream_options"]; ok {
		var so map[string]json.RawMessage
		if json.Unmarshal(v, &so) == nil {
			if iv, ok := so["include_usage"]; ok {
				// COMPATIBILITY 3.1: exactly true. json.Unmarshal into a bool
				// refuses 1 and "true", which is the point.
				var b bool
				if json.Unmarshal(iv, &b) == nil {
					r.IncludeUsage = b
				}
			}
		}
	}
}

// Options configures an [Upstream].
type Options struct {
	// Shape selects the wire contract.
	Shape Shape
	// Name identifies the upstream in failure messages and in the
	// x-fake-upstream response header.
	Name string
	// Script produces the content for one request. Nil uses [DefaultScript].
	Script func(*Recorded) Script
	// Behaviour produces the failure injection for one request. Nil means no
	// injection at all.
	Behaviour func(*Recorded) Behaviour
}

// Upstream is a running fake backend.
type Upstream struct {
	// URL is the base URL. Append [Shape.Path] to reach the endpoint.
	URL string
	// Shape is the wire contract this upstream speaks.
	Shape Shape
	// Name identifies it.
	Name string

	srv  *httptest.Server
	opts Options

	mu       sync.Mutex
	requests []*Recorded
}

// New starts an upstream. The caller closes it; every scenario does so through
// t.Cleanup.
func New(opts Options) *Upstream {
	if opts.Name == "" {
		opts.Name = "fake-" + opts.Shape.String()
	}
	u := &Upstream{Shape: opts.Shape, Name: opts.Name, opts: opts}
	u.srv = httptest.NewServer(http.HandlerFunc(u.serve))
	u.URL = u.srv.URL
	return u
}

// Close shuts the upstream down.
func (u *Upstream) Close() { u.srv.Close() }

// Endpoint is the full URL of this shape's inference endpoint.
func (u *Upstream) Endpoint() string { return u.URL + u.Shape.Path() }

// Requests returns everything received so far, oldest first. The slice is a
// copy; the records it points at are not mutated after the response is written.
func (u *Upstream) Requests() []*Recorded {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]*Recorded, len(u.requests))
	copy(out, u.requests)
	return out
}

// Count is how many requests have arrived.
func (u *Upstream) Count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

// Last returns the most recent request, or nil.
func (u *Upstream) Last() *Recorded {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.requests) == 0 {
		return nil
	}
	return u.requests[len(u.requests)-1]
}

// Reset forgets every recorded request.
func (u *Upstream) Reset() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.requests = nil
}

func (u *Upstream) serve(w http.ResponseWriter, r *http.Request) {
	body := readAll(r)
	rec := &Recorded{
		Method: r.Method,
		Path:   r.URL.Path,
		Header: r.Header.Clone(),
		Body:   body,
		At:     time.Now(),
	}
	rec.peek()

	u.mu.Lock()
	u.requests = append(u.requests, rec)
	rec.Seq = len(u.requests)
	u.mu.Unlock()

	script := DefaultScript(rec)
	if u.opts.Script != nil {
		script = u.opts.Script(rec)
	}
	if script.Model == "" {
		script.Model = rec.Model
	}
	if script.ID == "" {
		script.ID = u.defaultID(rec.Seq)
	}
	if script.Created == 0 {
		script.Created = fixedCreated
	}

	var bh Behaviour
	if u.opts.Behaviour != nil {
		bh = u.opts.Behaviour(rec)
	}

	if bh.Latency > 0 {
		time.Sleep(bh.Latency)
	}

	h := w.Header()
	h.Set("x-fake-upstream", u.Name)
	if bh.RetryAfter > 0 {
		h.Set("retry-after", fmt.Sprintf("%d", int(bh.RetryAfter.Round(time.Second).Seconds())))
	}
	if bh.QuotaExhausted {
		u.quotaHeaders(h, bh)
	}

	if bh.failing() {
		u.writeError(w, bh)
		return
	}

	if rec.Stream {
		u.writeStream(w, rec, script, bh)
		return
	}
	u.writeJSON(w, rec, script)
}

// fixedCreated is the pinned created value. A constant rather than time.Now,
// because COMPATIBILITY 2.4 makes created a stream identity clients compare and
// a golden test cannot compare against a clock.
const fixedCreated int64 = 1753660800 // 2025-07-28T00:00:00Z

func (u *Upstream) defaultID(seq int) string {
	if u.Shape == ShapeAnthropic {
		return fmt.Sprintf("msg_fake%08d", seq)
	}
	return fmt.Sprintf("chatcmpl-fake%08d", seq)
}

func (u *Upstream) quotaHeaders(h http.Header, bh Behaviour) {
	reset := bh.QuotaResetAt
	if reset.IsZero() {
		reset = time.Now().Add(time.Hour)
	}
	h.Set("x-ratelimit-limit-requests", "1000")
	h.Set("x-ratelimit-remaining-requests", "0")
	h.Set("x-ratelimit-reset-requests", reset.UTC().Format(time.RFC3339))
	if u.Shape == ShapeAnthropic {
		h.Set("anthropic-ratelimit-requests-limit", "1000")
		h.Set("anthropic-ratelimit-requests-remaining", "0")
		h.Set("anthropic-ratelimit-requests-reset", reset.UTC().Format(time.RFC3339))
	}
	if h.Get("retry-after") == "" {
		d := time.Until(reset)
		if d < time.Second {
			d = time.Second
		}
		h.Set("retry-after", fmt.Sprintf("%d", int(d.Round(time.Second).Seconds())))
	}
}

func readAll(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r.Body)
	_ = r.Body.Close()
	return buf.Bytes()
}

// marshal encodes with the serializer COMPATIBILITY 2.1a makes normative:
// compact separators, raw UTF-8, and Go's HTML escaping OFF. Left on, Go turns
// "a && b" into "a && b", which no other OpenAI-compatible server
// does.
func marshal(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic("fake: marshal: " + err.Error())
	}
	// Encode appends a newline; the frame supplies its own.
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

func ptrOf[T any](v T) *T { return &v }

// splitInto cuts s into at most n pieces on byte boundaries. It never returns an
// empty piece, and n <= 1 returns s whole.
func splitInto(s string, n int) []string {
	if s == "" {
		return nil
	}
	if n <= 1 || n >= len(s) {
		if n >= len(s) && len(s) > 1 {
			out := make([]string, 0, len(s))
			for i := range len(s) {
				out = append(out, s[i:i+1])
			}
			return out
		}
		return []string{s}
	}
	out := make([]string, 0, n)
	size := len(s) / n
	for i := range n {
		start := i * size
		end := start + size
		if i == n-1 {
			end = len(s)
		}
		out = append(out, s[start:end])
	}
	return out
}

// flusher writes a frame and pushes it out, so a test observing a stream sees
// frames as they are produced rather than at close.
type flusher struct {
	w http.ResponseWriter
	f http.Flusher
}

func newFlusher(w http.ResponseWriter) *flusher {
	fl, _ := w.(http.Flusher)
	return &flusher{w: w, f: fl}
}

func (f *flusher) write(b []byte) bool {
	if _, err := f.w.Write(b); err != nil {
		return false
	}
	if f.f != nil {
		f.f.Flush()
	}
	return true
}
