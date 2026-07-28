package app

import (
	"net/http"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/wire/openai"
	"github.com/ziozzang/dorang/internal/wire/rerank"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// The frontend half of the T1 inference surface: completions, responses,
// moderations, rerank, audio and images (COMPATIBILITY §0, DESIGN §2.1).
//
// Every one of them decodes to a type in internal/canonical, and internal/backend
// encodes from it. There is deliberately no "same family, copy the bytes"
// shortcut on any of these paths, for the reason §10.1 gives and one more: a
// shortcut is invisible until the day a deployment pairs an OpenAI frontend with
// a non-OpenAI backend, and then it is not a missing feature but a wrong answer.

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
		// The relayed OpenAI shape, and the one vendor whose /v1/embeddings is
		// close enough to relay into — same route, same members, with `input`
		// normalized to an array by that adapter.
		ok = api == catalog.APIOpenAIChat || api == catalog.APIJina
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
