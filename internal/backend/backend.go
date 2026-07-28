package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/wire/anthropic"
	"github.com/ziozzang/dorang/internal/wire/openai"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// Observer receives one record per FAILED upstream attempt.
//
// It is the metering seam (DESIGN §12.4), and it is deliberately not called on
// success: passing a struct through an interface boxes it, boxing allocates,
// and §15.5 prohibits allocation on the hot path. A failed request is not the
// hot path, and the shape of an upstream's error envelope is precisely the
// thing SGLANG.md §6.2 and DESIGN §4.3 record as undiscoverable without a
// packet capture.
//
// A Failure carries identifiers and classifications only. There is no field
// here that can hold key material.
type Observer interface {
	UpstreamFailure(Failure)
}

// Failure is one unsuccessful attempt, in the terms an operator debugs in.
type Failure struct {
	Provider      string
	Credential    string
	UpstreamModel string
	Op            Operation
	// Status is the upstream HTTP status, zero when no response arrived.
	Status int
	// Attempts counts the attempts made, including this one.
	Attempts int
	// Shape is the upstream envelope [server.Normalize] recognized.
	Shape server.Shape
	// NativeErrorType is the upstream's own type when it was outside the
	// canonical vocabulary (COMPATIBILITY §11.3).
	NativeErrorType string
	// Code is dorang's canonical code for the condition.
	Code  string
	Total time.Duration
}

// Options configures a [Backend].
type Options struct {
	// Client is the HTTP client. Nil means [NewClient].
	Client *http.Client
	// Credentials resolves credential ids. Nil means every deployment is
	// unauthenticated, which is only correct for a test.
	Credentials Credentials
	// Observer receives failed attempts. Nil disables it.
	Observer Observer
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Backend is L5: it turns a routing decision into an upstream exchange.
//
// It is immutable after construction and safe for concurrent use. A hot reload
// rebuilds providers, not this.
type Backend struct {
	client *http.Client
	creds  Credentials
	obs    Observer
	now    func() time.Time
}

// New builds a Backend.
func New(o Options) *Backend {
	b := &Backend{client: o.Client, creds: o.Credentials, obs: o.Observer, now: o.Now}
	if b.client == nil {
		b.client = NewClient()
	}
	if b.now == nil {
		b.now = time.Now
	}
	return b
}

// Target is one routing decision, reduced to what the backend needs.
//
// It is a value rather than a *router.Decision so that L5 does not import L3.
// Priority arrives ALREADY direction-normalized: internal/router computed the
// canonical class value and negated it for a descending engine (§7.5), and this
// package must not repeat that arithmetic. Two implementations of one negation
// is exactly how vLLM and SGLang end up agreeing about a number they must
// disagree about.
type Target struct {
	Provider   *Provider
	Credential string
	// UpstreamModel is the REAL model id. It goes upstream; the client's
	// requested name goes back in the response body (§7.2).
	UpstreamModel string
	// PriorityField is the wire field the number goes in, empty when this
	// engine takes none. Priority is the value for THIS engine's direction.
	PriorityField string
	Priority      int
	// PriorityTier is the non-numeric fold (OpenAI's service_tier), empty when
	// this engine has none. It is refused for a self-hosted engine regardless
	// of configuration; see selfhosted.go.
	PriorityTier string
}

// Call is one client request, decoded once and reusable across fail-back hops.
type Call struct {
	Op Operation
	// ClientAPI is the protocol the CALLER speaks, which decides how the answer
	// is rendered. It is unrelated to the provider's wire shape.
	ClientAPI catalog.API
	// Model is the client-facing name, restored into every answer (§7.2).
	Model string
	// Body is the caller's original bytes, used by operations that are relayed
	// rather than converted.
	Body []byte
	// Request is the neutral form of a chat call. Nil for every other
	// operation.
	Request *canonical.Request
	// Rerank is the neutral form of a rerank call (§10.1's neutral
	// representation, in internal/canonical). Nil for every other operation.
	Rerank *canonical.RerankRequest

	// The T1 surface's neutral forms. Exactly one is set, selected by Op. They
	// are separate fields rather than an `any` because every consumer switches
	// on Op already, and a type assertion would add a second, independent way
	// for the two to disagree.
	Moderation    *canonical.ModerationRequest
	Speech        *canonical.SpeechRequest
	Transcription *canonical.TranscriptionRequest
	Image         *canonical.ImageRequest

	// ResponseID is dorang's own id for a stored Responses exchange, and
	// ResponseEcho the request fields that surface echoes back on its answer.
	// dorang owns the state (DESIGN §9.2 [R1-C7]), so it owns the id; the echo
	// belongs to the request, so regenerating it from dorang's defaults would
	// tell the client dorang changed settings it never touched.
	ResponseID   string
	ResponseEcho *openai.ResponsesOptions

	Stream bool
	// AllowLossy is the parsed x-dorang-allow-lossy opt-in (§10.1).
	AllowLossy canonical.Capability
	// DefaultMaxTokens is the target model's catalogued output ceiling, used
	// where the wire shape requires the field and the caller named none.
	DefaultMaxTokens int
	// IncludeUsage mirrors the caller's stream_options.include_usage.
	IncludeUsage bool

	// Transform is the response half of a §10.5b transform filter, nil when the
	// model carries none — which is the ordinary case and costs one nil check.
	//
	// It exists as an interface rather than as a mask because the mask table
	// belongs to the caller and must not travel: §10.5b rule 1 is that the table
	// appears in no log line, no metric label, no trace and no ledger row, and
	// the narrowest way to keep that true is for this package never to hold one.
	// What crosses the seam is neutral events and neutral responses, in both
	// directions.
	Transform Transform

	// Accepted, when set, is called exactly once, after the upstream answered a
	// status this package will serve and BEFORE any byte of the answer is
	// written.
	//
	// It is the only moment a caller can still put something on the response
	// headers: DESIGN §10.4 stamps the extension headers on the first write, and
	// a stream's first write happens inside [Backend.Do]. A caller that filled
	// them before the call would be describing an attempt that had not happened
	// yet; one that filled them afterwards would be writing into a header block
	// the client can no longer see.
	Accepted func()
}

// Transform is the response side of DESIGN §10.5b, held by the caller.
//
// The request side is not here: a filter rewrites the neutral request before
// routing, which is long before this package sees anything. What is left is the
// mirror image — restoring the caller's own text in whatever comes back — and it
// has to happen on the neutral form, once, rather than once per encoder.
type Transform interface {
	// Response restores a complete answer in place.
	Response(*canonical.Response)
	// Stream opens the per-stream rewriter. It is called once per relay, and the
	// value it returns holds state across frames: a placeholder can straddle a
	// frame boundary and neither half means anything alone.
	Stream() StreamTransform
}

// StreamTransform rewrites one stream's events.
type StreamTransform interface {
	// Rewrite applies the transform to one event in place and may return a
	// synthesized event that must be written BEFORE it — the held tail, when the
	// stream is about to move on to something with no text to attach it to.
	Rewrite(*canonical.StreamEvent) *canonical.StreamEvent
	// Flush is whatever is still held at the end of the stream, or nil. It is
	// the caller's own text: it only looked like the beginning of a placeholder.
	Flush() *canonical.StreamEvent
}

// Result is one exchange's outcome, in the terms the dispatcher needs to
// report a router outcome, price the request and answer the client.
type Result struct {
	// Status is the upstream HTTP status; zero when no response arrived.
	Status int
	// Body is the complete answer in the CALLER's protocol. It is nil for a
	// stream, which has already been written.
	Body []byte
	// ErrorBody is the upstream's own error envelope, verbatim apart from the
	// credential scrubbing of §10.6 rule 4. It is set only alongside a 4xx or
	// 5xx and is nil otherwise.
	//
	// It exists for one consumer: a batch row's error file, which has to carry
	// the upstream's own envelope rather than dorang's normalization of it —
	// that file is what the caller diffs against the vendor's documentation.
	// Every other path reads Err, which is the normalized form.
	ErrorBody []byte
	// ContentType labels Body. It is not always application/json: speech
	// answers audio and a transcription asked for in srt or vtt answers text,
	// and mislabelling either gives the client something it cannot play or
	// parse. Empty means application/json.
	ContentType string
	Usage       canonical.Usage

	TTFT  time.Duration
	Total time.Duration
	// RetryAfter is a provider-signalled cooldown, zero when none was sent.
	RetryAfter time.Duration
	// FirstByteSent reports that the client has already seen output, which
	// closes fail-back for good (§7.6).
	FirstByteSent bool
	// Attempts counts the in-provider attempts made.
	Attempts int

	// Err is nil on success.
	Err *server.Error
	// Transport reports that no HTTP response arrived at all.
	Transport bool
	// Timeout distinguishes a deadline from a refusal.
	Timeout bool
	// Retryable reports that ANOTHER deployment may be tried. It never means
	// "call this provider again": that decision was already made and spent.
	Retryable bool
}

// Do runs one exchange against one deployment.
//
// w is written only when the call streams, and only after the upstream has
// answered 2xx — so a failure before that point still has an HTTP status to
// spend. A non-streaming answer comes back on Result.Body rather than being
// written here, because pricing and token counts have to land on the request
// before the headers are stamped (§10.4).
func (b *Backend) Do(ctx context.Context, t Target, c *Call, w http.ResponseWriter) Result {
	var res Result
	start := b.now()

	if t.Provider == nil {
		res.Err = server.NewError(http.StatusBadGateway, server.TypeAPIError,
			"no upstream is configured for this deployment").WithCode("no_upstream")
		return res
	}
	p := t.Provider

	endpoint, err := p.Endpoint(c.Op, t.UpstreamModel, c.Stream)
	if err != nil {
		res.Err = operationError(err)
		return res
	}

	x := &exchange{call: c, target: &t, prov: p, attempt: 1}
	if c.Op.ChatShaped() {
		x.req = prepareRequest(x)
	}

	payload, err := p.ad.encode(x)
	if err != nil {
		res.Err = encodeError(err)
		return res
	}

	// One attempt per iteration. The policy retries only a connection that was
	// never established (see [Policy]); everything else is the fallback chain's
	// call, which is why this loop is short.
	attempts := p.retry.attempts()
	for n := 1; ; n++ {
		res.Attempts = n
		x.attempt = n
		resp, aerr := b.send(ctx, x, endpoint, payload)
		if aerr != nil {
			res.Total = b.now().Sub(start)
			res.Err, res.Timeout = aerr.err, aerr.timeout
			res.Transport = aerr.transport
			res.Retryable = true
			if aerr.credential {
				// A credential dorang cannot use is not a transport failure and
				// not this deployment's fault to retry against.
				res.Transport, res.Retryable = false, true
			}
			if n < attempts && aerr.retriable && waitFor(ctx, p.retry.wait(n)) {
				continue
			}
			b.report(&res, t, c)
			return res
		}
		res.TTFT = b.now().Sub(start)
		return b.finish(ctx, x, resp, w, res, start)
	}
}

// attemptError is one attempt's failure, before any response body exists.
type attemptError struct {
	err *server.Error
	// retriable marks a failure the in-provider policy may repeat: the
	// connection was never established, so the model never saw the request.
	retriable  bool
	transport  bool
	timeout    bool
	credential bool
}

// send builds and issues one HTTP request.
func (b *Backend) send(ctx context.Context, x *exchange, endpoint string,
	payload []byte) (*http.Response, *attemptError) {

	p, t, c, contentType := x.prov, *x.target, x.call, x.ctype

	// providers[].timeout, applied per attempt. A stream's deadline covers the
	// whole exchange, which is the point: an engine that stops emitting
	// mid-generation is exactly the case a request timeout exists for.
	//
	// The no-timeout case gets a no-op cancel rather than a second context, so
	// the common configuration costs nothing.
	cancel := context.CancelFunc(noCancel)
	if p.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
	}

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		cancel()
		return nil, &attemptError{err: server.NewError(http.StatusBadGateway, server.TypeAPIError,
			"the upstream request could not be built").WithCode(CodeUpstreamRequest)}
	}
	if contentType == "" {
		contentType = jsonType
	}
	hreq.Header.Set("Content-Type", contentType)
	// Identity encoding, always: a compressed stream cannot be relayed frame by
	// frame and the terminal usage frame has to be readable (see openai.Scanner).
	hreq.Header.Set("Accept-Encoding", "identity")
	if c.Stream {
		hreq.Header.Set("Accept", "text/event-stream")
	}
	if cerr := b.applyCredential(p, t.Credential, hreq.Header); cerr != nil {
		cancel()
		return nil, &attemptError{err: credentialError(t.Credential), credential: true}
	}
	// What was actually put on the wire, read back from the headers rather than
	// from the credential table: an OAuth token is applied by code this package
	// does not own and never sees, and the header is the one place both spellings
	// are visible. It is what an upstream would be echoing if it echoed anything
	// (§10.6 rule 4, the response direction).
	x.secrets = collectSecrets(hreq.Header)

	resp, err := b.client.Do(hreq)
	if err != nil {
		cancel()
		e, timeout := transportError(err)
		return nil, &attemptError{
			err: e, transport: true, timeout: timeout,
			// Only a connection that was never established may be repeated: a
			// POST that reached the server may have been executed, and a second
			// one bills the caller twice for one answer.
			retriable: !timeout && dialFailure(err),
		}
	}
	// The deadline must outlive this function for a streaming body, so the
	// cancel travels with the response and fires when the body is closed.
	resp.Body = &cancelReader{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// noCancel is the cancel function of an attempt with no deadline of its own.
func noCancel() {}

// applyCredential resolves and applies the provider credential.
func (b *Backend) applyCredential(p *Provider, id string, h http.Header) error {
	var (
		secret string
		oauth  Applier
	)
	if b.creds != nil && id != "" {
		secret, oauth = b.creds.Credential(id)
	} else {
		p.ad.headers(h)
		return nil
	}
	return p.ApplyCredential(secret, oauth, h)
}

// finish handles a response that arrived: the status classes, then the body.
func (b *Backend) finish(ctx context.Context, x *exchange, resp *http.Response,
	w http.ResponseWriter, res Result, start time.Time) Result {

	defer resp.Body.Close()
	res.Status = resp.StatusCode
	res.RetryAfter = retryAfter(resp.Header)

	// A 30x never reaches the redirect-following code because the client is
	// built not to have any (§10.6). It arrives here as an ordinary response
	// and is refused by status, which is the only place a test can observe the
	// refusal.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		drain(resp.Body)
		res.Total = b.now().Sub(start)
		res.Err = redirectError(resp.StatusCode)
		res.Retryable = true
		b.report(&res, *x.target, x.call)
		return res
	}

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		res.Total = b.now().Sub(start)
		res.Err = upstreamError(resp.StatusCode, body, resp.Header, x.secrets)
		// The upstream's own bytes, scrubbed. Nothing on the interactive path
		// reads them; a batch row's error file does, and normalizing there would
		// hand the caller dorang's paraphrase of a vendor error they are trying
		// to look up.
		res.ErrorBody = scrubBytes(body, x.secrets)
		if res.Err.RetryAfterSeconds > 0 && res.RetryAfter == 0 {
			res.RetryAfter = time.Duration(res.Err.RetryAfterSeconds) * time.Second
		}
		res.Retryable = true
		b.report(&res, *x.target, x.call)
		return res
	}

	// The last moment anything can still be put on the response headers: they
	// are stamped on the first write, and for a stream the first write is three
	// lines below (DESIGN §10.4).
	if x.call.Accepted != nil {
		x.call.Accepted()
	}

	if x.call.Stream {
		usage, err := b.relay(x, resp, w)
		res.Total = b.now().Sub(start)
		res.Usage = usage
		res.FirstByteSent = true
		if err != nil {
			res.Err = streamError(err)
			b.report(&res, *x.target, x.call)
		}
		return res
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		res.Total = b.now().Sub(start)
		res.Err = server.NewError(http.StatusBadGateway, server.TypeAPIError,
			"could not read the upstream response").WithCode(CodeUpstreamBody)
		res.Retryable = true
		b.report(&res, *x.target, x.call)
		return res
	}

	// The upstream's own labelling is the only way to tell a JSON transcript
	// from an srt one: the two arrive on the same route with the same status.
	x.respType = resp.Header.Get("Content-Type")
	out, ctype, usage, cerr := b.convert(body, x)
	res.Total = b.now().Sub(start)
	if cerr != nil {
		res.Err = cerr
		b.report(&res, *x.target, x.call)
		return res
	}
	res.Usage = usage
	res.Body = out
	res.ContentType = ctype
	return res
}

// maxErrorBody bounds how much of an error body is read. An upstream that
// answers a failure with a hundred megabytes of HTML must not be able to make
// dorang read it.
const maxErrorBody = 1 << 20

// convert turns the upstream answer into the caller's protocol.
//
// A same-family exchange still goes through the neutral form rather than being
// copied: §7.2 requires the body to carry the name the client asked for, and
// the name is not the only place the two differ once a deployment's upstream
// model id is not the client's.
func (b *Backend) convert(body []byte, x *exchange) ([]byte, string, canonical.Usage, *server.Error) {
	dec, err := x.prov.ad.decode(body, x)
	if err != nil {
		if errors.Is(err, errNotAnObject) {
			// A relayed answer that is not a JSON object at all. It is a
			// different condition from "dorang could not understand this
			// answer", and the operator's next step differs: one is a wrong
			// route, the other a wrong shape.
			return nil, "", canonical.Usage{}, server.NewError(http.StatusBadGateway,
				server.TypeAPIError, "the upstream answer was not a JSON object").
				WithCode(CodeUpstreamShape)
		}
		return nil, "", canonical.Usage{}, server.NewError(http.StatusBadGateway, server.TypeAPIError,
			"could not read the upstream response: "+err.Error()).WithCode(CodeUpstreamDecode)
	}
	if dec.raw != nil {
		return dec.raw, dec.ctype, dec.usage, nil
	}

	adoptEngineReasoning(dec.resp, x.prov.engine)

	// The non-streaming half of the same rule the stream sinks enforce: a tool
	// call whose arguments are not a JSON document must not be handed to the
	// client as a finished call. Here the status has not been spent yet, so it is
	// an error the client can actually act on rather than an in-band frame.
	if name, bad := malformedToolCall(dec.resp); bad {
		return nil, "", canonical.Usage{}, server.NewError(http.StatusBadGateway,
			server.TypeAPIError,
			"the upstream returned a tool call whose arguments are not valid JSON: "+name).
			WithCode(CodeMalformedToolArguments)
	}

	var usage canonical.Usage
	if dec.resp.Usage != nil {
		usage = *dec.resp.Usage
	}
	// §10.5b: restore the caller's own text before it is rendered. It happens
	// here, on the neutral form, so it happens once for every protocol pairing
	// rather than once per encoder — and after the usage has been read, because a
	// placeholder and the text it stands for are not the same number of tokens
	// and the upstream's count is the one that was billed.
	if x.call.Transform != nil {
		x.call.Transform.Response(dec.resp)
	}
	// The T1 chat-shaped surfaces render differently from chat completions even
	// though their caller speaks the same family, so they are asked first.
	if out, done, eerr := encodeT1Client(dec.resp, x.call); done {
		if eerr != nil {
			return nil, "", usage, server.NewError(http.StatusBadGateway, server.TypeAPIError,
				"could not render the response: "+eerr.Error()).WithCode(CodeResponseEncode)
		}
		return out, jsonType, usage, nil
	}
	out, eerr := encodeClient(dec.resp, x.call)
	if eerr != nil {
		return nil, "", usage, server.NewError(http.StatusBadGateway, server.TypeAPIError,
			"could not render the response: "+eerr.Error()).WithCode(CodeResponseEncode)
	}
	return out, jsonType, usage, nil
}

// malformedToolCall reports a tool call in a complete answer whose arguments do
// not parse, naming it.
//
// It reads the NEUTRAL form, so it covers every backend family at once: the
// OpenAI shapes carry arguments as an opaque string and are the ones that can be
// truncated, but a hand-written or proxied answer in any family can arrive the
// same way.
func malformedToolCall(r *canonical.Response) (string, bool) {
	if r == nil {
		return "", false
	}
	for i := range r.Choices {
		blocks := r.Choices[i].Message.Content
		for j := range blocks {
			tu := blocks[j].ToolUse
			if tu == nil || len(bytes.TrimSpace(tu.Input)) == 0 {
				continue
			}
			if !json.Valid(tu.Input) {
				name := tu.Name
				if name == "" {
					name = tu.ID
				}
				return name, true
			}
		}
	}
	return "", false
}

// encodeClient renders a neutral answer in the caller's protocol.
func encodeClient(r *canonical.Response, c *Call) ([]byte, error) {
	switch c.ClientAPI {
	case catalog.APIAnthropicMessages:
		return anthropic.MarshalResponse(r, &anthropic.ResponseOptions{Model: c.Model})
	default:
		return openai.MarshalResponse(r, &openai.ResponseOptions{Model: c.Model})
	}
}

// operationError renders a shape that does not serve an operation.
func operationError(err error) *server.Error {
	var no *errNoOperation
	if errors.As(err, &no) {
		return unsupportedError(no.code(), no.Error())
	}
	var un unsupportedProvider
	if errors.As(err, &un) {
		return unsupportedError(un.code, un.Error())
	}
	return server.NewError(http.StatusBadGateway, server.TypeAPIError,
		"the endpoint for this deployment could not be derived").WithCode(CodeUpstreamRequest)
}

// report hands a failed attempt to the observer. It runs off the success path
// only, so the interface boxing it costs is never on the hot path.
func (b *Backend) report(res *Result, t Target, c *Call) {
	if b.obs == nil || res.Err == nil {
		return
	}
	b.obs.UpstreamFailure(Failure{
		Provider:        t.Provider.name,
		Credential:      t.Credential,
		UpstreamModel:   t.UpstreamModel,
		Op:              c.Op,
		Status:          res.Status,
		Attempts:        res.Attempts,
		Shape:           res.Err.Shape,
		NativeErrorType: res.Err.NativeType,
		Code:            res.Err.Code,
		Total:           res.Total,
	})
}

// waitFor sleeps between attempts, and reports whether the context outlived it.
func waitFor(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// dialFailure reports whether an error happened while establishing the
// connection, which is the one failure a non-idempotent POST may repeat.
func dialFailure(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) {
		return op.Op == "dial"
	}
	return false
}

// drain reads and discards a bounded amount of a body so the connection can be
// reused rather than torn down.
func drain(r io.Reader) { _, _ = io.Copy(io.Discard, io.LimitReader(r, 4<<10)) }

// cancelReader ties a per-attempt context deadline to the response body's
// lifetime, so a streaming response is not cut off when send returns.
type cancelReader struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelReader) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}
