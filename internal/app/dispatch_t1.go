package app

import (
	"net/http"
	"strconv"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/wire/openai"
	"github.com/ziozzang/dorang/internal/wire/rerank"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// The T1 inference surface: completions, responses, moderations, rerank, audio
// and images (COMPATIBILITY §0, DESIGN §2.1).
//
// Every one of them decodes to a type in internal/canonical and encodes from
// it. There is deliberately no "same family, copy the bytes" shortcut on any of
// these paths, for the reason §10.1 gives and one more: a shortcut is invisible
// until the day a deployment pairs an OpenAI frontend with a non-OpenAI backend,
// and then it is not a missing feature but a wrong answer.

// decodeT1 decodes the T1 families. It is the continuation of
// [dispatcher.decode]'s switch, split out so the T0 path stays readable.
func (d *dispatcher) decodeT1(st *dispatchState, rq *server.Request, c *call) error {
	// c.body, not rq.Body: the call already holds the capped bytes, and reading
	// them from one place keeps the decode independent of how the gate stored
	// them.
	body := c.body
	bad := func(err error) error {
		return server.NewError(http.StatusBadRequest, server.TypeInvalidRequest,
			err.Error()).WithCode("invalid_request")
	}

	switch rq.Route.Family {
	case server.FamilyOpenAICompletions:
		creq, err := openai.DecodeCompletionRequest(body)
		if err != nil {
			return bad(err)
		}
		c.kind, c.clientAPI, c.creq = callCompletions, catalog.APIOpenAIChat, creq
		c.allowUsg = creq.IncludeUsage()

	case server.FamilyOpenAIResponses:
		return d.decodeResponses(st, rq, c, body)

	case server.FamilyOpenAIModerations:
		mreq, err := openai.DecodeModerationRequest(body)
		if err != nil {
			return bad(err)
		}
		c.kind, c.clientAPI, c.modReq = callModerations, catalog.APIOpenAIChat, mreq

	case server.FamilyOpenAIRerank:
		rreq, err := rerank.DecodeRequest(body)
		if err != nil {
			return bad(err)
		}
		if len(rreq.Documents) == 0 {
			return server.NewError(http.StatusBadRequest, server.TypeInvalidRequest,
				"rerank requires at least one document").
				WithCode("invalid_parameter").WithParam("documents")
		}
		c.kind, c.clientAPI, c.rerankReq = callRerank, catalog.APICohere, rreq

	case server.FamilyOpenAISpeech:
		sreq, err := openai.DecodeSpeechRequest(body)
		if err != nil {
			return bad(err)
		}
		if sreq.StreamFormat == "sse" {
			// The SSE speech container is a different response protocol, not a
			// different parameter. Answering the non-streaming body to a client
			// that asked for frames is worse than refusing (DESIGN §0.2).
			return server.NewError(http.StatusNotImplemented, server.TypeNotImplemented,
				"streamed speech (stream_format: sse) is not implemented; omit stream_format for the complete audio body").
				WithCode("speech_stream_not_implemented").WithParam("stream_format")
		}
		c.kind, c.clientAPI, c.speechReq = callSpeech, catalog.APIOpenAIChat, sreq

	case server.FamilyOpenAITranscription, server.FamilyOpenAITranslation:
		translate := rq.Route.Family == server.FamilyOpenAITranslation
		treq, err := openai.DecodeTranscriptionForm(rq.Form, translate)
		if err != nil {
			return bad(err)
		}
		if treq.Stream {
			return server.NewError(http.StatusNotImplemented, server.TypeNotImplemented,
				"streamed transcription is not implemented; omit stream for the complete transcript").
				WithCode("transcription_stream_not_implemented").WithParam("stream")
		}
		c.kind, c.clientAPI, c.transReq = callTranscription, catalog.APIOpenAIChat, treq

	case server.FamilyOpenAIImageGeneration, server.FamilyOpenAIImageEdit,
		server.FamilyOpenAIImageVariation:
		ireq, err := d.decodeImage(rq, body)
		if err != nil {
			return err
		}
		if ireq.Stream {
			return server.NewError(http.StatusNotImplemented, server.TypeNotImplemented,
				"streamed image generation is not implemented; omit stream for the complete image").
				WithCode("image_stream_not_implemented").WithParam("stream")
		}
		c.kind, c.clientAPI, c.imageReq = callImages, catalog.APIOpenAIChat, ireq

	default:
		return server.NewError(http.StatusNotImplemented, server.TypeNotImplemented,
			"this route has no dispatcher in this build").WithCode("route_not_implemented")
	}
	return nil
}

func (d *dispatcher) decodeImage(rq *server.Request, body []byte) (*canonical.ImageRequest, error) {
	switch rq.Route.Family {
	case server.FamilyOpenAIImageEdit:
		return openai.DecodeImageForm(rq.Form, canonical.ImageEdit)
	case server.FamilyOpenAIImageVariation:
		return openai.DecodeImageForm(rq.Form, canonical.ImageVariation)
	default:
		return openai.DecodeImageRequest(body)
	}
}

// checkFamily refuses a crossing that has no meaning rather than producing one.
//
// A 501 with a named code is the contract for "declared but not served on this
// deployment" (DESIGN §0.2). The alternative — encoding a rerank request as
// chat completions because the deployment happens to speak that — would answer
// 200 with something that is not a ranking.
func checkFamily(c *call, api catalog.API) error {
	ok := true
	switch c.kind {
	case callEmbeddings:
		ok = api == catalog.APIOpenAIChat
	case callRerank:
		// The self-hosted engines serve the same shape at /rerank, so they are
		// accepted alongside the two vendors whose protocol it is.
		ok = api == catalog.APICohere || api == catalog.APIJina || api == catalog.APIOpenAIChat
	case callModerations, callSpeech, callTranscription, callImages:
		ok = api == catalog.APIOpenAIChat || api == catalog.APIOpenAIResponses
	case callCompletions:
		// A non-streaming legacy request crosses into any family: it is a
		// conversation in the neutral form and comes back through it. A
		// STREAMING one does not, because the relay that makes cross-family
		// streaming work emits chat.completion.chunk frames and a
		// /v1/completions client reads text_completion ones. Emitting the wrong
		// shape is worse than refusing.
		ok = !c.stream || api == catalog.APIOpenAIChat
	}
	if ok {
		return nil
	}
	return server.NewError(http.StatusNotImplemented, server.TypeNotImplemented,
		c.kind.String()+" cannot cross protocol families and this deployment speaks "+string(api)).
		WithCode(c.kind.code() + "_family_mismatch")
}

// encodeT1Upstream renders the T1 requests that are not chat-shaped.
func (d *dispatcher) encodeT1Upstream(c *call, dec *router.Decision, up *upstream) (upstreamRequest, error) {
	switch c.kind {
	case callRerank:
		flavor := rerankFlavor(up.api)
		body, err := rerank.MarshalRequest(c.rerankReq, &rerank.EncodeOptions{
			Model: dec.UpstreamModel, Flavor: flavor,
		})
		if err != nil {
			return upstreamRequest{}, encodeError(err)
		}
		endpoint := up.endpoint(pathRerank)
		if flavor == rerank.FlavorCohere {
			endpoint = up.endpointRaw(cohereRerankPath)
		}
		return jsonUpstream(body, endpoint), nil

	case callModerations:
		body, err := openai.MarshalModerationRequest(c.modReq, dec.UpstreamModel)
		if err != nil {
			return upstreamRequest{}, encodeError(err)
		}
		return jsonUpstream(body, up.endpoint(pathModerations)), nil

	case callSpeech:
		body, err := openai.MarshalSpeechRequest(c.speechReq, dec.UpstreamModel)
		if err != nil {
			return upstreamRequest{}, encodeError(err)
		}
		return jsonUpstream(body, up.endpoint(pathSpeech)), nil

	case callTranscription:
		boundary := d.boundary(dec)
		body, _, err := openai.EncodeTranscriptionForm(c.transReq, dec.UpstreamModel, boundary)
		if err != nil {
			return upstreamRequest{}, encodeError(err)
		}
		path := pathTranscriptions
		if c.transReq.Translate {
			path = pathTranslations
		}
		return upstreamRequest{
			body:        body,
			endpoint:    up.endpoint(path),
			contentType: "multipart/form-data; boundary=" + boundary,
		}, nil

	case callImages:
		if c.imageReq.Op == canonical.ImageGenerate {
			body, err := openai.MarshalImageRequest(c.imageReq, dec.UpstreamModel)
			if err != nil {
				return upstreamRequest{}, encodeError(err)
			}
			return jsonUpstream(body, up.endpoint(pathImageGenerate)), nil
		}
		boundary := d.boundary(dec)
		body, err := openai.EncodeImageForm(c.imageReq, dec.UpstreamModel, boundary)
		if err != nil {
			return upstreamRequest{}, encodeError(err)
		}
		path := pathImageEdit
		if c.imageReq.Op == canonical.ImageVariation {
			path = pathImageVariation
		}
		return upstreamRequest{
			body:        body,
			endpoint:    up.endpoint(path),
			contentType: "multipart/form-data; boundary=" + boundary,
		}, nil
	}
	return upstreamRequest{}, server.NewError(http.StatusNotImplemented,
		server.TypeNotImplemented, "this route has no upstream encoder in this build").
		WithCode("route_not_implemented")
}

// boundary picks a multipart boundary.
//
// It is derived from the attempt rather than randomly generated so that a
// golden test over an outgoing request compares bytes, not a fresh UUID. It
// only has to not appear in the payload, and this one cannot: it is
// hyphen-prefixed ASCII of a fixed shape.
func (d *dispatcher) boundary(dec *router.Decision) string {
	return "dorang" + strconv.FormatInt(d.now().UnixNano(), 36) +
		"x" + strconv.Itoa(dec.Attempt)
}

func rerankFlavor(api catalog.API) rerank.Flavor {
	switch api {
	case catalog.APICohere:
		return rerank.FlavorCohere
	case catalog.APIJina:
		return rerank.FlavorJina
	default:
		return rerank.FlavorGeneric
	}
}

// convertT1Response turns a T1 upstream answer into the client's protocol.
func (d *dispatcher) convertT1Response(c *call, up *upstream, body []byte,
	header http.Header) ([]byte, string, canonical.Usage, error) {

	fail := func(err error) ([]byte, string, canonical.Usage, error) {
		return nil, "", canonical.Usage{}, server.NewError(http.StatusBadGateway,
			server.TypeAPIError, "could not read the upstream response: "+err.Error()).
			WithCode("upstream_decode")
	}

	switch c.kind {
	case callRerank:
		cresp, err := rerank.DecodeResponse(body, rerankFlavor(up.api), c.model)
		if err != nil {
			return fail(err)
		}
		out, err := rerank.MarshalResponse(cresp)
		if err != nil {
			return fail(err)
		}
		return out, "application/json", usageOf(cresp.Usage), nil

	case callModerations:
		cresp, err := openai.DecodeModerationResponse(body, c.model)
		if err != nil {
			return fail(err)
		}
		out, err := openai.MarshalModerationResponse(cresp)
		if err != nil {
			return fail(err)
		}
		// Moderation is billed per request, not per token, on every backend
		// that serves it. Reporting a fabricated token count here would price
		// it twice over (DESIGN §8.3).
		return out, "application/json", canonical.Usage{}, nil

	case callSpeech:
		// The neutral form holds the bytes; the media type is dorang's own from
		// the requested container rather than the backend's label, because a
		// self-hosted engine routinely answers application/octet-stream and a
		// browser handed that plays nothing.
		mt := openai.SpeechMediaType(c.speechReq.Format)
		return body, mt, canonical.Usage{}, nil

	case callTranscription:
		cresp, err := openai.DecodeTranscriptionResponse(body, header.Get("Content-Type"))
		if err != nil {
			return fail(err)
		}
		out, mt, err := openai.MarshalTranscriptionResponse(cresp)
		if err != nil {
			return fail(err)
		}
		return out, mt, usageOf(cresp.Usage), nil

	case callImages:
		cresp, err := openai.DecodeImageResponse(body)
		if err != nil {
			return fail(err)
		}
		out, err := openai.MarshalImageResponse(cresp)
		if err != nil {
			return fail(err)
		}
		return out, "application/json", usageOf(cresp.Usage), nil
	}
	return nil, "", canonical.Usage{}, server.NewError(http.StatusNotImplemented,
		server.TypeNotImplemented, "this route has no response decoder in this build").
		WithCode("route_not_implemented")
}

func usageOf(u *canonical.Usage) canonical.Usage {
	if u == nil {
		return canonical.Usage{}
	}
	return *u
}
