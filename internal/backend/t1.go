package backend

import (
	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/wire/openai"
)

// The T1 inference surface (COMPATIBILITY §0, DESIGN §2.1): the legacy
// completions route, the Responses API, moderations, and the audio and image
// families.
//
// They live in one file rather than one adapter each because exactly one wire
// shape serves them — everything OpenAI-compatible — and a per-operation
// adapter would be twelve types that all dispatch to the same host. The other
// families refuse them through their own `default` branch, which is where a
// refusal belongs: the shape decides what it serves.
//
// The Responses API appears here only in [encodeT1Client], the CLIENT-facing
// half. Its upstream half is the chat request every family already understands,
// so it is encoded by the ordinary chat path and the operation only decides how
// the answer is rendered — see [openaiAdapter.endpoint].
//
// Two of them are not JSON in one direction and that is the whole reason this
// file has any structure at all:
//
//   - transcription, translation, image edit and image variation send
//     multipart, so encode has to say what it produced. It reports through
//     [exchange.ctype], which [Backend.send] reads.
//   - speech answers audio and a transcription asked for in srt or vtt answers
//     text, so decode has to say what it received. It reports through
//     [decoded.ctype], which the dispatcher puts on the response.
//
// Everything still goes through internal/canonical in both directions. There is
// no "same family, copy the bytes" shortcut on any of these paths: a shortcut
// is invisible until the day a deployment pairs one family's frontend with
// another's backend, and then it is not a missing feature but a wrong answer.

// encodeT1 renders a T1 request body.
func encodeT1(x *exchange) ([]byte, error) {
	c := x.call
	model := x.target.UpstreamModel

	switch c.Op {
	case OpCompletions:
		return openai.MarshalCompletionRequest(x.req, &openai.EncodeOptions{Model: model})

	case OpModerations:
		return openai.MarshalModerationRequest(c.Moderation, model)

	case OpSpeech:
		return openai.MarshalSpeechRequest(c.Speech, model)

	case OpTranscription, OpTranslation:
		body, _, err := openai.EncodeTranscriptionForm(c.Transcription, model, x.boundary())
		if err != nil {
			return nil, err
		}
		x.ctype = multipartType(x.boundary())
		return body, nil

	case OpImageGenerate:
		return openai.MarshalImageRequest(c.Image, model)

	case OpImageEdit, OpImageVariation:
		body, err := openai.EncodeImageForm(c.Image, model, x.boundary())
		if err != nil {
			return nil, err
		}
		x.ctype = multipartType(x.boundary())
		return body, nil
	}
	return nil, noOperation("openai-chat", c.Op, "no encoder in this build")
}

// decodeT1 converts a T1 upstream answer.
func decodeT1(body []byte, x *exchange) (*decoded, error) {
	c := x.call

	switch c.Op {
	case OpCompletions:
		resp, err := openai.DecodeCompletionResponse(body, &openai.DecodeOptions{Model: c.Model})
		if err != nil {
			return nil, err
		}
		return &decoded{resp: resp}, nil

	case OpModerations:
		cresp, err := openai.DecodeModerationResponse(body, c.Model)
		if err != nil {
			return nil, err
		}
		out, err := openai.MarshalModerationResponse(cresp)
		if err != nil {
			return nil, err
		}
		// Moderation is billed per request rather than per token on every
		// backend that serves it, and no backend reports a token count for it.
		// Inventing one here would price it as generation (DESIGN §8.3).
		return &decoded{raw: out, ctype: jsonType}, nil

	case OpSpeech:
		// The media type is dorang's own, derived from the container the caller
		// asked for, rather than the backend's label: a self-hosted engine
		// routinely answers application/octet-stream and a browser handed that
		// plays nothing.
		format := ""
		if c.Speech != nil {
			format = c.Speech.Format
		}
		return &decoded{raw: body, ctype: openai.SpeechMediaType(format)}, nil

	case OpTranscription, OpTranslation:
		cresp, err := openai.DecodeTranscriptionResponse(body, x.respType)
		if err != nil {
			return nil, err
		}
		out, mt, err := openai.MarshalTranscriptionResponse(cresp)
		if err != nil {
			return nil, err
		}
		d := &decoded{raw: out, ctype: mt}
		if cresp.Usage != nil {
			d.usage = *cresp.Usage
		}
		return d, nil

	case OpImageGenerate, OpImageEdit, OpImageVariation:
		cresp, err := openai.DecodeImageResponse(body)
		if err != nil {
			return nil, err
		}
		out, err := openai.MarshalImageResponse(cresp)
		if err != nil {
			return nil, err
		}
		d := &decoded{raw: out, ctype: jsonType}
		if cresp.Usage != nil {
			d.usage = *cresp.Usage
		}
		return d, nil
	}
	return nil, noOperation("openai-chat", c.Op, "no response decoder in this build")
}

// encodeT1Client renders a chat-shaped T1 answer in the caller's protocol.
//
// It is separate from [encodeClient] because the caller's family is not enough
// to decide the shape here: a /v1/completions client and a /v1/chat/completions
// client both speak openai-chat and must get different objects.
func encodeT1Client(r *canonical.Response, c *Call, now int64) ([]byte, bool, error) {
	switch c.Op {
	case OpCompletions:
		// `created` for the same reason as in [encodeClient]: this shape has the
		// member, the family the answer may have come from does not, and a
		// timestamp of 0 is a timestamp in 1970.
		out, err := openai.MarshalCompletionResponse(r, &openai.ResponseOptions{
			Model: c.Model, Created: createdOr(r, now),
		})
		return out, true, err
	case OpResponses:
		if c.ResponseID != "" {
			// dorang's own id, not the upstream's: the upstream id is
			// meaningless to dorang's store and would change under a fail-back
			// hop, so a client's saved reference would resolve on one attempt
			// and 404 on the next.
			r.ID = c.ResponseID
		}
		out, err := openai.MarshalResponsesResponse(r, c.ResponseEcho)
		return out, true, err
	}
	return nil, false, nil
}

const jsonType = "application/json"

func multipartType(boundary string) string {
	return "multipart/form-data; boundary=" + boundary
}
