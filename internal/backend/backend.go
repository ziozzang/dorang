package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/loadsignal"
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
	// MaxResponseBytes bounds a non-streaming upstream body. Zero means
	// [DefaultMaxResponseBytes].
	MaxResponseBytes int64
}

// Backend is L5: it turns a routing decision into an upstream exchange.
//
// It is immutable after construction and safe for concurrent use. A hot reload
// rebuilds providers, not this.
type Backend struct {
	client  *http.Client
	creds   Credentials
	obs     Observer
	now     func() time.Time
	maxBody int64
}

// New builds a Backend.
func New(o Options) *Backend {
	b := &Backend{client: o.Client, creds: o.Credentials, obs: o.Observer, now: o.Now,
		maxBody: o.MaxResponseBytes}
	if b.client == nil {
		b.client = NewClient()
	}
	if b.now == nil {
		b.now = time.Now
	}
	if b.maxBody <= 0 {
		b.maxBody = DefaultMaxResponseBytes
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
	// ModelKnown reports that pkg/catalog holds an entry for UpstreamModel
	// under this provider's kind, mirroring [router.Decision.ModelKnown].
	//
	// It arms the substitution reading: unset, this package still classifies
	// what the upstream answered — that costs nothing — but does not COPY the
	// name out, because a deployment whose model dorang has no figures for
	// cannot prove a substitution and would otherwise pay one string allocation
	// per request for an answer nobody can act on. An operator alias like
	// `local`, answered by whatever gguf the server loaded, is exactly that
	// case.
	ModelKnown bool
	// PriorityField is the wire field the number goes in, empty when this
	// engine takes none. Priority is the value for THIS engine's direction.
	PriorityField string
	Priority      int
	// PriorityTier is the non-numeric fold (OpenAI's service_tier), empty when
	// this engine has none. It is refused for a self-hosted engine regardless
	// of configuration; see selfhosted.go.
	PriorityTier string
	// Capabilities is what THIS deployment can express (§10.1). Zero means the
	// wire shape's own set, which is what [CapabilitiesForAPI] answers — and
	// internal/app builds the routing table by calling that same function, so
	// leaving it unset cannot put the two layers out of agreement.
	//
	// It is on the Target rather than the Call because it is a property of the
	// deployment, and a fail-back hop lands on a different one. It exists so a
	// deployment that expresses LESS than its family — a self-hosted engine
	// without stop sequences, a shape added later — is refused rather than
	// silently downgraded, and so that the set the router filtered on is the
	// set the encoder converts against.
	Capabilities canonical.Capability
	// MaxTokensField is the spelling this deployment's upstream takes for the
	// output ceiling: `max_tokens` or `max_completion_tokens`. Empty is
	// `max_tokens`, which is [openai.EncodeOptions]'s own default, so an
	// unconfigured deployment keeps today's bytes exactly.
	//
	// It is on the Target rather than the Call because it is a property of the
	// deployment and a fail-back hop lands on a different one — the same
	// argument Capabilities carries. It is only meaningful for the OpenAI wire
	// shapes; internal/app refuses to build a deployment that sets it on a
	// provider whose shape has no such field, rather than accepting it and
	// encoding something else.
	MaxTokensField string
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

	// UsageChunkChoices and AnthropicTotalTokens are COMPATIBILITY §3.3 and
	// §6.8, the two deliberate divergences an operator can select between.
	//
	// They are the field this struct did not have, and their absence is why
	// `compat.usage_chunk_choices: empty` and `compat.anthropic_total_tokens:
	// false` were REFUSED at load rather than served: internal/wire has
	// implemented both shapes all along, internal/config has accepted both
	// spellings all along, and there was no path between them. DESIGN §17.1
	// names a setting that loads and does nothing as this repository's dominant
	// defect; refusing was the honest interim, and this is the path.
	//
	// They are on the CALL rather than on [Options] because [Backend] is
	// immutable after construction and a hot reload rebuilds providers, not the
	// backend — so a setting that can change under reload has to arrive with the
	// request. The zero value of each is the served default in the wire package
	// it feeds, which means a caller that never sets them gets today's behaviour
	// with no branch.
	UsageChunkChoices    openai.UsageChunkChoices
	AnthropicTotalTokens anthropic.TotalTokensMode

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
	//
	// It receives the conversion's loss report for exactly that reason. [Result]
	// looks like the obvious carrier and is the wrong one: a stream's headers are
	// already on the wire by the time Do returns, so a loss delivered there would
	// reach x-dorang-downgraded on buffered answers and nowhere on streamed ones —
	// which is a report that is present or absent depending on a property of the
	// response the caller did not ask about.
	//
	// # WHEN it fires, and why that is not one line
	//
	// At the LAST MOMENT THIS KIND OF ANSWER still has a header block, which is
	// not the same instant for both kinds:
	//
	//   - a STREAM: immediately before [Backend.relay], whose third statement
	//     sets the SSE content type and whose first event write stamps the
	//     headers. There is nothing after it.
	//   - a BUFFERED answer: when [Backend.finish] returns, after the upstream
	//     body has been read, decoded and rendered into the caller's protocol.
	//     The bytes go out on [Result.Body] and are written by the caller, long
	//     after Do has returned, so the block is still open.
	//
	// It used to fire at the stream's moment for both, and that was the whole of
	// the response direction's silence. Every loss the RESPONSE encoders produce
	// — a rich stop reason collapsed onto this family's enumeration, a thinking
	// block that cannot cross, `n > 1` folded into one message — is produced
	// while rendering the answer, which happens strictly AFTER the upstream body
	// is read. At the stream's moment not one byte of that body has arrived, so
	// a buffered answer's report was handed over complete in one direction and
	// empty in the other, and the encoders wrote their half into a report nobody
	// held.
	//
	// The asymmetry that remains is in the ANSWER, not in the carrier, and it is
	// irreducible: a streamed answer's response-side losses are discovered event
	// by event, and its headers left with the first one. There is no later point
	// for a stream, so the report a stream delivers carries the request direction
	// only. See the note on [Backend.relay].
	//
	// The report is nil for an operation with no neutral request, and empty for a
	// conversion that lost nothing; both are ordinary, and every LossReport method
	// tolerates a nil receiver.
	Accepted func(*canonical.LossReport)
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
	// UsageExtra is what the upstream reported that no counter names, including
	// what SERVER-SIDE tools cost. It is carried rather than summarised because
	// pricing charges some of it: a web search is billed per search and an
	// image's tokens at a rate unrelated to chat tokens, and neither is
	// reachable from any field above.
	UsageExtra *canonical.UsageExtra

	TTFT  time.Duration
	Total time.Duration
	// RetryAfter is a provider-signalled cooldown, zero when none was sent.
	RetryAfter time.Duration
	// FirstByteSent reports that the client has already seen output, which
	// closes fail-back for good (§7.6).
	FirstByteSent bool
	// Attempts counts the in-provider attempts made.
	Attempts int
	// ServedModel is the model name the upstream put in its own response body,
	// and ModelAgreement is how it compares with the `upstream_model` dorang
	// sent. ServedModel is filled only when the comparison found something
	// worth naming, because materializing it otherwise would put an allocation
	// on every stream for a string nobody reads.
	//
	// [canonical.ModelSubstituted] is a 200 that answered as a DIFFERENT model:
	// the request is priced at the asked model's rate for another model's work,
	// the router's context-window fallback used the asked model's declared
	// window, and the client was told nothing. This is reported and never acted
	// on here — see [exchange.agree].
	ServedModel    string
	ModelAgreement canonical.ModelAgreement

	// LoadMetrics is the engine's own `endpoint-load-metrics` report, verbatim,
	// or empty when the response carried none (VLLM.md §3.2). It is the input to
	// utilization pricing and is deliberately unparsed here: an empty string
	// means "no report", never "an idle backend", and only the layer that prices
	// on it is in a position to record that distinction.
	LoadMetrics string

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
		// Allocated before the encoder runs, per exchange rather than per call: a
		// fail-back hop lands on a different deployment and loses different things,
		// and a report carried over from the previous one would describe a
		// conversion this response is not the result of.
		x.loss = &canonical.LossReport{}
		x.req = prepareRequest(x)
		if err := refuseMaterialLoss(x); err != nil {
			res.Err = err
			return res
		}
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
	// Ask a vLLM engine to report its own occupancy on this response (VLLM.md
	// §3.2). No server flag is needed and no other backend is asked: the header
	// is a vLLM extension, and sending a dorang-invented header to a hosted
	// vendor's API is at best ignored and at worst a 400.
	//
	// It is sent for streamed calls too even though the report only comes back
	// on a buffered one, because the condition dorang can test here is the
	// ENGINE and the condition that decides whether vLLM answers is its own.
	// Asking and getting nothing is a refusal this build records by name
	// (loadsignal.RefusalNoHeader); not asking would make the absence dorang's
	// doing and unreportable.
	if p.engine == EngineVLLM {
		hreq.Header.Set(loadsignal.RequestHeader, loadsignal.RequestFormat)
	}
	if cerr := b.applyCredential(p, t.Credential, hreq.Header); cerr != nil {
		cancel()
		return nil, &attemptError{err: credentialError(t.Credential, cerr), credential: true}
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
	// The engine's own occupancy report, read here because this is the last
	// place resp.Header exists — the response is closed on the way out of this
	// function and both the streamed and the buffered branch are below. The
	// value is carried raw and parsed by the pricing layer, so this package
	// keeps no opinion about what a load report means.
	res.LoadMetrics = resp.Header.Get(loadsignal.Header)

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

	// A streaming request the upstream answered with a JSON document instead of
	// an event stream.
	//
	// It is refused here, before a byte is written, because the byte relay cannot
	// refuse it later: that path forwards as it scans and only learns the body was
	// never SSE when it runs out of input, by which time the vendor's error
	// envelope has been written into the client's stream verbatim and
	// [Result.FirstByteSent] has closed §7.6 fallback over bytes that were never
	// an answer. The upstream's own Content-Type is the one signal available
	// before the first write, and it is decisive: no SSE body is ever labelled
	// application/json.
	//
	// Only that one label is refused. A missing Content-Type, or any of the
	// text/event-stream spellings, goes to the relay exactly as before — an
	// upstream that mislabels a real stream must not be turned away over a
	// header.
	if x.call.Stream && jsonLabelled(resp.Header.Get("Content-Type")) {
		drain(resp.Body)
		res.Total = b.now().Sub(start)
		res.Err = server.NewError(http.StatusBadGateway, server.TypeAPIError,
			"the upstream answered a streaming request with a JSON document rather than an "+
				"event stream").WithCode(CodeUpstreamShape)
		res.Retryable = true
		b.report(&res, *x.target, x.call)
		return res
	}

	if x.call.Stream {
		// The last moment a STREAM has a header block: they are stamped on the
		// first write, and the first write is inside relay, three lines below
		// (DESIGN §10.4). Nothing of the answer has been read yet, so the report
		// carries the request direction only — see [Call.Accepted].
		x.accept()
		usage, sent, err := b.relay(x, resp, w)
		res.Total = b.now().Sub(start)
		res.Usage = usage
		// From the bytes that actually reached the client, not from the fact that
		// this was a stream. The flag closes fail-back for good (§7.6), and it
		// used to be set unconditionally — so an upstream that answered 200 and
		// then wrote nothing at all committed a response that had no content,
		// could never be retried, and reported success.
		res.FirstByteSent = sent > 0
		res.ServedModel, res.ModelAgreement = x.served, x.agree
		if err != nil {
			res.Err = streamError(err)
			// Another DEPLOYMENT may be tried precisely while nothing has been
			// written. Once a byte is out, FirstByteSent forbids the hop on its
			// own; saying "retryable" as well would be describing a hop that
			// cannot happen.
			res.Retryable = sent == 0
			b.report(&res, *x.target, x.call)
		}
		return res
	}

	// A BUFFERED answer's header block is still open when this function returns —
	// the bytes leave on [Result.Body] and the caller writes them — so the
	// handover happens here, at the end, where the response encoders have already
	// written their half of the report. Deferred rather than called on the
	// success path, so that the failure returns below hand over the same report
	// they always did rather than acquiring a new way to skip it.
	defer x.accept()

	body, err := readUpstreamBody(resp.Body, b.maxBody)
	if err != nil {
		res.Total = b.now().Sub(start)
		if errors.Is(err, errUpstreamTooLarge) {
			// Not retryable: a fail-back hop would buffer another one, and the
			// deployment that answered this way will answer the next hop the
			// same way.
			res.Err = server.NewError(http.StatusBadGateway, server.TypeAPIError,
				"the upstream response exceeded the size this gateway will buffer").
				WithCode(CodeUpstreamTooLarge)
			b.report(&res, *x.target, x.call)
			return res
		}
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
		// An answer that is not a response of this family — a wrong route, a
		// wrong `api`, a vendor error envelope wearing a 200 — is offered to the
		// fallback chain. Nothing was generated, so there is no billed turn to
		// repeat, and a sibling deployment is exactly the right next hop: it is
		// what the operator declared the chain FOR. Every other conversion
		// failure stays terminal, because those describe an answer that did
		// arrive and that a second deployment would not improve.
		res.Retryable = cerr.Code == CodeUpstreamShape
		b.report(&res, *x.target, x.call)
		return res
	}
	res.Usage = usage
	// The counts nothing names, carried to whoever prices this. A web search is
	// billed per search and an image's tokens at a rate unrelated to chat
	// tokens, and neither is reachable from res.Usage.
	res.UsageExtra = x.usageExtra
	res.Body = out
	res.ContentType = ctype
	res.ServedModel, res.ModelAgreement = x.served, x.agree
	return res
}

// jsonLabelled reports whether a Content-Type names a JSON document.
//
// It reads only the media type, so `application/json; charset=utf-8` and
// `application/json` are the same answer, and it is deliberately narrow: a
// vendor-specific `+json` suffix, an empty header and every text/* spelling all
// report false, because the only thing this decides is whether to refuse a body
// before reading it.
func jsonLabelled(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.EqualFold(strings.TrimSpace(ct), jsonType)
}

// maxErrorBody bounds how much of an error body is read. An upstream that
// answers a failure with a hundred megabytes of HTML must not be able to make
// dorang read it.
const maxErrorBody = 1 << 20

// DefaultMaxResponseBytes bounds a non-streaming upstream body.
//
// The error path above has always been bounded at 1 MiB and the success path
// was an unbounded io.ReadAll, which is the same asymmetry the interactive
// dispatcher shipped: a hostile, compromised or merely broken backend — and
// DESIGN §4.4 makes a self-hosted vLLM/SGLang node a first-class provider —
// could OOM the gateway from the far side of the trust boundary at three to
// four times the body size, because the buffer is then decoded and re-encoded.
//
// It matches the request cap rather than the 1 MiB error cap because a
// legitimate answer can be large: an embeddings response for a big batch of
// inputs is megabytes of floats, and a ceiling below the request cap would
// refuse answers to requests dorang itself accepted.
const DefaultMaxResponseBytes int64 = 32 << 20

// errUpstreamTooLarge is the sentinel for a body that hit the ceiling.
var errUpstreamTooLarge = errors.New("upstream response exceeded the configured ceiling")

// readUpstreamBody reads a complete upstream answer under a hard ceiling.
//
// One byte past the limit is read so that hitting it is detected rather than
// silently truncating the answer — a truncated JSON body would fail to decode
// somewhere further along and be reported as a protocol error, which sends
// whoever debugs it to the wrong place entirely.
func readUpstreamBody(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = DefaultMaxResponseBytes
	}
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errUpstreamTooLarge
	}
	return b, nil
}

// convert turns the upstream answer into the caller's protocol.
//
// A same-family exchange still goes through the neutral form rather than being
// copied: §7.2 requires the body to carry the name the client asked for, and
// the name is not the only place the two differ once a deployment's upstream
// model id is not the client's.
func (b *Backend) convert(body []byte, x *exchange) ([]byte, string, canonical.Usage, *server.Error) {
	dec, err := x.prov.ad.decode(body, x)
	if err != nil {
		// An adapter that already knows the status and the code — a host whose
		// answer to a buffered caller is an event stream, read back and found
		// wanting — says so in dorang's envelope, and the envelope is passed
		// through. Re-classifying it here would turn "the stream was cut off"
		// into "could not read the upstream response" and lose the code the
		// fallback decision below reads.
		var se *server.Error
		if errors.As(err, &se) {
			return nil, "", canonical.Usage{}, se
		}
		if errors.Is(err, errNotAnObject) {
			// A relayed answer that is not a JSON object at all. It is a
			// different condition from "dorang could not understand this
			// answer", and the operator's next step differs: one is a wrong
			// route, the other a wrong shape.
			return nil, "", canonical.Usage{}, server.NewError(http.StatusBadGateway,
				server.TypeAPIError, "the upstream answer was not a JSON object").
				WithCode(CodeUpstreamShape)
		}
		if isNotAResponse(err) {
			// A JSON object that parsed and is not a response of this family.
			// The status is 502 and NOT 200: the upstream's own 200 was the
			// defect, and passing it on is what made a misrouted deployment
			// indistinguishable from a working one. See [CodeUpstreamShape].
			return nil, "", canonical.Usage{}, server.NewError(http.StatusBadGateway,
				server.TypeAPIError,
				"the upstream answered 200 with a body that is not a response of the "+
					"protocol this deployment speaks").
				WithCode(CodeUpstreamShape)
		}
		return nil, "", canonical.Usage{}, server.NewError(http.StatusBadGateway, server.TypeAPIError,
			"could not read the upstream response: "+err.Error()).WithCode(CodeUpstreamDecode)
	}
	if dec.raw != nil {
		return dec.raw, dec.ctype, dec.usage, nil
	}
	if dec.resp == nil {
		// An adapter returned neither rendered bytes nor a neutral response.
		// That is dorang's own invariant, not the upstream's, and the reason it
		// is a named 502 rather than an assertion is that it used to be a nil
		// dereference one line below: a transcript of silence renders to zero
		// bytes, `raw` came back nil rather than empty, and the process took a
		// SIGSEGV on a request that was entirely valid. The decoder now returns
		// an empty slice for that case; this is the floor under every other one.
		return nil, "", canonical.Usage{}, server.NewError(http.StatusBadGateway,
			server.TypeAPIError, "the upstream answer produced nothing to send").
			WithCode(CodeUpstreamShape)
	}

	// The one point at which both names exist: dec.resp.ServedModel is what the
	// upstream called itself, and it is about to stop mattering to everything
	// downstream because §7.2 has already put the client's name in Model. Read
	// here, reported by [Backend.Do], acted on nowhere.
	x.noteServedModel(dec.resp.ServedModel)

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
	if out, done, eerr := encodeT1Client(dec.resp, x, b.now().Unix()); done {
		if eerr != nil {
			return nil, "", usage, server.NewError(http.StatusBadGateway, server.TypeAPIError,
				"could not render the response: "+eerr.Error()).WithCode(CodeResponseEncode)
		}
		return out, jsonType, usage, nil
	}
	out, eerr := encodeClient(dec.resp, x, b.now().Unix())
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
//
// # Identity, and why it is decided here rather than in an encoder
//
// D4 and D5, which were one defect: the non-streaming converter disagreed with
// the streaming one about both halves of a response's identity.
//
//   - `created`. The Messages family has no such member, so a Messages answer
//     rendered as a chat completion carried `"created":0` — a timestamp in 1970
//     on every converted turn. The STREAM writer for the identical conversion
//     stamps a real one (COMPATIBILITY 2.4), so the same request differed
//     depending only on whether the caller passed stream:true. The clock is the
//     backend's injectable one, which is why the value arrives as an argument
//     instead of being read inside an encoder.
//
//   - `id`. The rule is now stated by family, on both paths (DESIGN §10.7):
//     **the upstream's id crosses unchanged when the answer did not change
//     family, and is minted in the caller's family shape when it did.** An
//     Anthropic upstream rendered as a chat completion used to hand the client
//     `{"object":"chat.completion","id":"msg_2026…"}` — an id of the wrong
//     family under an object member naming this one — while the streaming path
//     for that same conversion minted a `chatcmpl-` id. Traceability is the
//     argument for crossing the id, and it is a good one; it just has to be the
//     same argument on both paths, and it does not extend to handing a client an
//     identifier its own SDK's shape does not describe.
//
// # The loss report, and the capability set it is computed against
//
// Both travel now, and neither did. The response encoders have recorded what a
// crossing costs since they were written — a rich stop reason collapsed onto
// this family's enumeration, a thinking block with no shape here, a redacted
// payload that cannot be reconstructed, `n > 1` folded into one message — and
// every one of those calls landed on a nil report, discarded at the point it was
// produced. The capability set was worse than absent: it was zero, so each
// encoder's `opt.caps()` fell back to its own DefaultCapabilities, which is the
// same value [exchange.clientCapabilities] computes. The response direction was
// therefore converted against the correct set BY COINCIDENCE, and reported
// against nothing at all.
func encodeClient(r *canonical.Response, x *exchange, now int64) ([]byte, error) {
	c := x.call
	switch c.ClientAPI {
	case catalog.APIAnthropicMessages:
		// TotalTokens is COMPATIBILITY §6.8. The zero value is the compat shape
		// the reference implementation emits, so an unset call is unchanged; the
		// strict vendor shape is reachable only because the value now travels.
		opt := &anthropic.ResponseOptions{
			Model:        c.Model,
			TotalTokens:  c.AnthropicTotalTokens,
			Capabilities: x.clientCapabilities(),
			Loss:         x.loss,
		}
		if !r.SameFamily(canonical.FamilyAnthropicMessages) {
			opt.ID = anthropic.NewMessageID()
		}
		return anthropic.MarshalResponse(r, opt)
	default:
		opt := &openai.ResponseOptions{
			Model:        c.Model,
			Created:      createdOr(r, now),
			Capabilities: x.clientCapabilities(),
			Loss:         x.loss,
		}
		if !r.SameFamily(canonical.FamilyOpenAIChat) {
			opt.ID = openai.NewStreamID()
		}
		return openai.MarshalResponse(r, opt)
	}
}

// createdOr resolves the creation timestamp: the upstream's own when it stated
// one, this gateway's clock when its family has no such member.
func createdOr(r *canonical.Response, now int64) int64 {
	if r != nil && r.Created != 0 {
		return r.Created
	}
	return now
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
