package backend

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/wire/systemone"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// systemoneAdapter is the decision-model surface, POST /v1/systemone.
//
// Every host that serves it speaks one contract, so the body is relayed with
// only the model replaced and the answer handed back with the client-facing
// model restored. What differs between hosts is WHETHER they serve it and the
// limits they impose, and that is this adapter's whole job:
//
//   - kind `typesafe` — TypeSafe's own endpoint. Its documented ceilings
//     (https://docs.typesafe.ai/api.md): 255 Choice options, 10 Score levels.
//   - kind `systemone` — any other host speaking the contract. Its limits are
//     its own and are not guessed at here; a refusal comes back as the
//     upstream's 422.
//   - kind `ollama` — a LOCAL Ollama, v0.35 or later
//     (https://docs.ollama.com/api/systemone.md): 64 questions, 2–26 Choice
//     options and Score levels, criteria written as strings, a 64 KiB body.
//   - kind `ollama-cloud` — refused by name. Ollama documents System One as
//     local-only, and the hosted endpoint answers 501 "not implemented"
//     (asked 2026-10-01); sending it there would spend a hop to learn that.
//
// It is selected by OPERATION, not by the provider's api, because the local
// Ollama kind's api is openai-chat: the same host serves chat at /v1/chat/…
// and decisions at /v1/systemone.
type systemoneAdapter struct{}

// TypeSafe's documented ceilings.
const (
	typesafeMaxScoreLevels = 10
	typesafeMaxOptions     = systemone.MaxChoiceOptions
)

// Ollama's documented ceilings.
const (
	ollamaMaxQuestions = 64
	ollamaMaxOptions   = 26
	ollamaMaxBody      = 64 << 10
)

func (systemoneAdapter) endpoint(p *Provider, op Operation, _ string, _ bool) (string, error) {
	if op != OpSystemOne {
		return "", noOperation("systemone", op, "a decision-model host serves /v1/systemone only")
	}
	switch p.kind {
	case "ollama-cloud":
		return "", noOperation("ollama-cloud", op,
			"Ollama serves System One on a local server only; the hosted endpoint answers 501. "+
				"Route this model to a local `ollama` provider, `typesafe`, or another `systemone` host")
	case "ollama", "typesafe", "systemone":
		// The local Ollama base carries /v1 already; TypeSafe's does not.
		return joinVersioned(p.base, "/v1", pathSystemOne), nil
	}
	if p.api == catalog.APISystemOne {
		return joinVersioned(p.base, "/v1", pathSystemOne), nil
	}
	return "", noOperation(p.kind, op,
		"this provider kind does not serve System One; use `typesafe`, a local `ollama`, or `systemone` with the host's base_url")
}

func (systemoneAdapter) credential(secret string, h http.Header) { bearer(secret, h) }

func (systemoneAdapter) headers(http.Header) {}

func (systemoneAdapter) encode(x *exchange) ([]byte, error) {
	if x.call.SystemOne == nil {
		return nil, errNilRequest
	}
	if err := checkSystemOneLimits(x.prov.kind, x.call.SystemOne, len(x.call.Body)); err != nil {
		return nil, err
	}
	return relayRequest(x.call.Body, x.target.UpstreamModel)
}

// checkSystemOneLimits applies the ceilings a host documents. A request the
// gateway accepted can still exceed them, because the gateway admits what ANY
// host takes; this is where the deployment it landed on says no, in its terms.
func checkSystemOneLimits(kind string, req *canonical.SystemOneRequest, bodyLen int) error {
	limit := func(param, format string, args ...any) error {
		return &systemone.Error{Param: param, Message: fmt.Sprintf(format, args...)}
	}
	switch kind {
	case "typesafe":
		for _, q := range req.Questions {
			switch {
			case q.Type == canonical.SystemOneScore && q.Options > typesafeMaxScoreLevels:
				return limit("questions."+q.ID+".criteria",
					"has %d levels; TypeSafe accepts at most %d", q.Options, typesafeMaxScoreLevels)
			case q.Type == canonical.SystemOneChoice && q.Options > typesafeMaxOptions:
				return limit("questions."+q.ID+".criteria",
					"has %d options; TypeSafe accepts at most %d", q.Options, typesafeMaxOptions)
			}
		}
	case "ollama":
		if bodyLen > ollamaMaxBody {
			return limit("body", "is %d bytes; Ollama accepts at most 64 KiB", bodyLen)
		}
		if len(req.Questions) > ollamaMaxQuestions {
			return limit("questions", "has %d questions; Ollama accepts at most %d", len(req.Questions), ollamaMaxQuestions)
		}
		for _, q := range req.Questions {
			if q.Options > ollamaMaxOptions {
				return limit("questions."+q.ID+".criteria",
					"has %d entries; Ollama accepts at most %d", q.Options, ollamaMaxOptions)
			}
		}
		if !req.StringCriteriaOnly {
			return limit("questions", "Ollama accepts criteria written as strings only (and null for a Choice option); "+
				"an object or array description needs a `typesafe` or `systemone` host")
		}
	}
	return nil
}

func (systemoneAdapter) decode(body []byte, x *exchange) (*decoded, error) {
	use, reported, err := systemone.CheckResponse(body)
	if err != nil {
		return nil, err
	}
	obj, err := jsonObject(body)
	if err != nil {
		return nil, err
	}
	// The answering model, before the client-facing name replaces it: TypeSafe
	// reports the versioned id behind an alias, which is what a caller pins a
	// confidence threshold to, so it is kept as the served model.
	var served string
	if raw, ok := obj["model"]; ok {
		_ = json.Unmarshal(raw, &served)
	}
	x.noteServedModel(served)
	out, err := marshalWithModel(obj, x.call.Model)
	if err != nil {
		return nil, err
	}
	var u canonical.Usage
	if reported {
		u.InputTokens, u.OutputTokens = use.InputTokens, use.OutputTokens
		u.Report(canonical.UsageInput)
		u.Report(canonical.UsageOutput)
	}
	return &decoded{raw: out, usage: u}, nil
}

func (systemoneAdapter) source(io.Reader, *exchange) (eventSource, error) {
	return nil, noOperation("systemone", OpSystemOne, "System One answers one JSON body and does not stream")
}
