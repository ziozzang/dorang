package server

import (
	"net/http"
	"strconv"
)

// baseRoutes is the T0 surface of COMPATIBILITY §0 plus the metrics scrape.
//
// Eleven paths, fourteen operations. The audit behind §0 found that an attached
// client breaks immediately without exactly these and that they are 2.2% of a
// live 505-path proxy deployment — which is why they are built first and why
// the other 97.8% answers 501 with a reason instead of being stubbed.
//
// The pairs are not aliases to collapse. A client configured with a base URL
// ending in /v1 and one configured without it both exist in the wild, and both
// must work; that is why /chat/completions is registered next to
// /v1/chat/completions rather than redirected to it.
func (s *Server) baseRoutes(access MetricsAccess) []*Route {
	inference := func(pattern, name string, family Family) *Route {
		return &Route{
			Pattern:   pattern,
			Methods:   MethodPOST,
			Name:      name,
			Family:    family,
			NeedsBody: true,
			// The gate scans the model out of the body and enforces the
			// allow-list before this handler runs.
			ModelAuth: ModelAuthGate,
			Handler:   s.handleInference,
		}
	}
	models := func(pattern string) *Route {
		return &Route{
			Pattern: pattern,
			Methods: MethodGET | MethodHEAD,
			Name:    "models",
			Family:  FamilyModels,
			// The listing is FILTERED by the allow-list rather than gated on
			// it: a key sees the models it may use and nothing else, which is
			// COMPATIBILITY §7.4 and is not an authorization decision about a
			// model call, because no model is called.
			ModelAuth: ModelAuthNone,
			Handler:   s.handleModels,
		}
	}
	modelRetrieve := func(pattern string) *Route {
		return &Route{
			Pattern: pattern,
			Methods: MethodGET | MethodHEAD,
			Name:    "models_retrieve",
			Family:  FamilyModels,
			// Same rule as the listing, applied to one record:
			// handleModelRetrieve answers 404 for a model the key may not use,
			// so the allow-list is a FILTER here and not a gate. No model is
			// called, so there is nothing for the gate to authorize.
			ModelAuth: ModelAuthNone,
			Handler:   s.handleModelRetrieve,
		}
	}
	health := func(pattern, name string, kind healthKind) *Route {
		return &Route{
			Pattern:   pattern,
			Methods:   MethodGET | MethodHEAD | MethodOPTIONS,
			Name:      name,
			Family:    FamilyHealth,
			Public:    true,
			ModelAuth: ModelAuthNone,
			Handler:   s.healthHandler(kind),
		}
	}

	// multipart is the same route with the body declared as form data. The
	// server parses it in the gate so the allow-list check sees the model
	// (COMPATIBILITY 2.0 applied to a non-JSON body).
	multipart := func(pattern, name string, family Family) *Route {
		rt := inference(pattern, name, family)
		rt.Multipart = true
		return rt
	}
	// deployment is one of the model-in-the-path aliases. The pattern's {model}
	// segment carries the deployment name, and the server resolves it before
	// authorization.
	deployment := func(pattern, name string, family Family) *Route {
		rt := inference(pattern, name, family)
		rt.ModelParam = "model"
		return rt
	}
	deploymentMultipart := func(pattern, name string, family Family) *Route {
		rt := deployment(pattern, name, family)
		rt.Multipart = true
		return rt
	}

	routes := []*Route{
		inference("/v1/chat/completions", "chat_completions", FamilyOpenAIChat),
		inference("/chat/completions", "chat_completions", FamilyOpenAIChat),
		inference("/v1/embeddings", "embeddings", FamilyOpenAIEmbeddings),
		inference("/embeddings", "embeddings", FamilyOpenAIEmbeddings),
		inference("/v1/messages", "messages", FamilyAnthropicMessages),
		inference("/v1/messages/count_tokens", "messages_count_tokens", FamilyAnthropicCountTokens),

		// T1 — the legacy text-completions surface. Two spellings, for the same
		// reason chat completions has two: a client configured with a base URL
		// ending in /v1 and one configured without it both exist.
		inference("/v1/completions", "completions", FamilyOpenAICompletions),
		inference("/completions", "completions", FamilyOpenAICompletions),

		// T1 — rerank. Three spellings are deployed and all three are served;
		// /v2/rerank is not a different protocol, it is the same one under the
		// path the vendor's own SDK uses.
		inference("/v1/rerank", "rerank", FamilyOpenAIRerank),
		inference("/rerank", "rerank", FamilyOpenAIRerank),
		inference("/v2/rerank", "rerank", FamilyOpenAIRerank),

		// T1 — moderations.
		inference("/v1/moderations", "moderations", FamilyOpenAIModerations),
		inference("/moderations", "moderations", FamilyOpenAIModerations),

		// T1 — audio. Speech takes JSON and answers bytes; the other two take
		// multipart.
		inference("/v1/audio/speech", "audio_speech", FamilyOpenAISpeech),
		multipart("/v1/audio/transcriptions", "audio_transcriptions", FamilyOpenAITranscription),
		multipart("/v1/audio/translations", "audio_translations", FamilyOpenAITranslation),

		// T1 — images. Generation takes JSON; edits and variations take
		// multipart because they carry an image and possibly a mask.
		inference("/v1/images/generations", "images_generations", FamilyOpenAIImageGeneration),
		multipart("/v1/images/edits", "images_edits", FamilyOpenAIImageEdit),
		multipart("/v1/images/variations", "images_variations", FamilyOpenAIImageVariation),

		// T1 — the Responses API. The sub-resources are stateful and are
		// mounted by internal/app, which owns responses_store; this is the
		// inference half.
		inference("/v1/responses", "responses", FamilyOpenAIResponses),

		models("/v1/models"),
		models("/models"),
		modelRetrieve("/v1/models/{id}"),
		modelRetrieve("/models/{id}"),

		// "liveliness" is not a typo and not a synonym dorang chose. The
		// widely-deployed proxy this surface has to interoperate with spells it
		// that way, container manifests in the field probe that exact path, and
		// the correctly-spelled "liveness" exists alongside it. Serving only one
		// breaks half the deployments.
		health("/health/liveliness", "health_liveness", healthLive),
		health("/health/liveness", "health_liveness", healthLive),
		health("/health/readiness", "health_readiness", healthReady),
		health("/health", "health", healthOverall),
	}

	// The scrape. observability.prometheus removes it entirely rather than
	// serving an empty page: a route this build does not serve answers 501 with
	// a reason like every other one (COMPATIBILITY §9), and a 200 with no
	// families would tell a scraper the gateway is healthy and idle.
	//
	// It authenticates unless the file says otherwise. It used to be Public
	// alongside the container probes, which put per-key spend, per-credential
	// quota state and the whole configured model list on an unauthenticated
	// port.
	if access != MetricsOff {
		routes = append(routes, &Route{
			Pattern: "/metrics",
			Methods: MethodGET | MethodHEAD,
			Name:    "metrics",
			Family:  FamilyMetrics,
			Public:  access == MetricsPublic,
			Admin:   access == MetricsAdmin,
			// A scrape names no model. ModelAuthNone is the written answer,
			// not a default: newRouteTable refuses ModelAuthUnset.
			ModelAuth: ModelAuthNone,
			Handler:   s.handleMetrics,
		})
	}

	// The deployment-in-the-path aliases.
	//
	// COMPATIBILITY §7.5 is why these can be registered at all: the table is
	// specificity-ordered, so /openai/deployments/{model}/chat/completions
	// matches before a configured /openai/{rest...} passthrough prefix. A naive
	// prefix router swallows every one of them into the catch-all and the
	// symptom is an Azure-shaped client silently reaching a vendor passthrough.
	for _, base := range []string{"/engines/{model}", "/openai/deployments/{model}"} {
		routes = append(routes,
			deployment(base+"/chat/completions", "chat_completions", FamilyOpenAIChat),
			deployment(base+"/completions", "completions", FamilyOpenAICompletions),
			deployment(base+"/embeddings", "embeddings", FamilyOpenAIEmbeddings),
		)
	}
	// Azure serves the audio and image surfaces under the same deployment
	// prefix; /engines does not, and inventing routes there would answer 200 on
	// a path no client sends.
	const azure = "/openai/deployments/{model}"
	routes = append(routes,
		deployment(azure+"/audio/speech", "audio_speech", FamilyOpenAISpeech),
		deploymentMultipart(azure+"/audio/transcriptions", "audio_transcriptions", FamilyOpenAITranscription),
		deploymentMultipart(azure+"/audio/translations", "audio_translations", FamilyOpenAITranslation),
		deployment(azure+"/images/generations", "images_generations", FamilyOpenAIImageGeneration),
		deploymentMultipart(azure+"/images/edits", "images_edits", FamilyOpenAIImageEdit),
		deployment(azure+"/responses", "responses", FamilyOpenAIResponses),
	)
	return routes
}

// handleInference is every route that ends in an upstream model call. The
// server does the gate; the dispatcher does everything past it.
func (s *Server) handleInference(w http.ResponseWriter, rq *Request) error {
	cfg := rq.srv.snap.Load()
	if cfg.dispatcher == nil {
		return NewError(http.StatusNotImplemented, TypeNotImplemented,
			"no dispatcher is configured for this deployment").
			WithCode("dispatcher_not_configured")
	}
	if rq.Model == "" {
		return NewError(http.StatusBadRequest, TypeInvalidRequest,
			"the request did not name a model").
			WithCode("missing_model").WithParam("model")
	}
	// The allow-list is NOT consulted here any more. It used to be, and that is
	// precisely how it came to be enforced on one route and no other: this
	// handler is reached by the six inference patterns and by nothing else, so
	// a check living here was a check that batch create and passthrough could
	// not reach. It now runs at the gate for every ModelAuthGate route
	// (Server.serve), which is the same set plus anything added later — and it
	// answers 403 permission_error there, per COMPATIBILITY §11.2, which is the
	// answer the shared authorization gate has always given for the same
	// refusal.

	err := cfg.dispatcher.Dispatch(rq.Context(), rq, w)
	if err == nil {
		// A dispatcher that placed the frame itself has already set the flag;
		// this covers the rest.
		rq.EmitUsageEvent(w)
	}
	return err
}

// healthKind distinguishes the three probes.
type healthKind uint8

const (
	healthLive healthKind = iota
	healthReady
	healthOverall
)

// healthHandler answers a container probe.
//
// Liveness is about the process: it is up, so it answers 200 until it is not
// there to answer at all. Readiness is about whether new work should arrive,
// and goes 503 the instant a drain starts — which is the entire mechanism by
// which a rolling deploy does not drop requests.
//
// OPTIONS is answered because COMPATIBILITY §0 lists it alongside GET: probes
// behind a service mesh preflight, and a 501 on the preflight fails the pod.
func (s *Server) healthHandler(kind healthKind) Handler {
	return func(w http.ResponseWriter, rq *Request) error {
		h := w.Header()
		if rq.Method == http.MethodOptions {
			h.Set("Allow", rq.Route.allow)
			h.Set("Content-Length", "0")
			w.WriteHeader(http.StatusNoContent)
			return nil
		}
		ready := s.Ready()
		status := http.StatusOK
		var state string
		switch kind {
		case healthReady:
			if !ready {
				status, state = http.StatusServiceUnavailable, "draining"
			} else {
				state = "ready"
			}
		case healthOverall:
			if !ready {
				status, state = http.StatusServiceUnavailable, "draining"
			} else {
				state = "healthy"
			}
		default:
			state = "alive"
		}

		buf := getBuf()
		defer putBuf(buf)
		b := append(*buf, `{"status":`...)
		b = appendJSONString(b, state)
		// The shadow gate reports here as well as in metrics. A daily cost
		// ceiling that has stopped shadowing is not a serving failure and must
		// not take the pod out of rotation — so the status stays what it was
		// and the fact appears beside it (DESIGN §14.1).
		if kind != healthLive {
			snap := rq.srv.snap.Load()
			if o := snap.observer; o != nil {
				b = appendHealthObject(b, "shadow", o.Health)
			}
			// Everything else that has something to say about its own health,
			// metering first among them: DESIGN §12.1 promises a drop is never
			// silent, and this is where an operator looks.
			for _, r := range snap.reporters {
				b = appendHealthObject(b, r.HealthName(), r.Health)
			}
		}
		b = append(b, '}')
		*buf = b

		h.Set("Content-Type", "application/json")
		h.Set("Content-Length", strconv.Itoa(len(b)))
		h.Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		if rq.Method != http.MethodHead {
			_, _ = w.Write(b)
		}
		return nil
	}
}

// appendHealthObject writes `,"name":<json>` when the reporter produced
// something, and leaves dst untouched when it did not.
//
// "Produced something" is measured as growth past the key it was offered,
// because a reporter that appends nothing must not leave a dangling key behind
// — a health body that fails to parse is worse than one that omits a subsystem.
func appendHealthObject(dst []byte, name string, fill func([]byte) []byte) []byte {
	n := len(dst)
	dst = append(dst, ',', '"')
	dst = append(dst, name...)
	dst = append(dst, '"', ':')
	grown := fill(dst)
	if len(grown) <= len(dst) {
		return dst[:n]
	}
	return grown
}

// handleModels serves GET /v1/models.
//
// COMPATIBILITY §7.4: items are {"id","object":"model","created":<constant>,
// "owned_by"} and the list is filtered by the calling key's allow-list. The
// filter is not cosmetic — a client that reads the list and then picks a model
// from it must not be handed a name that will be refused at 401.
func (s *Server) handleModels(w http.ResponseWriter, rq *Request) error {
	cfg := rq.srv.snap.Load()
	if cfg.models == nil {
		return NewError(http.StatusNotImplemented, TypeNotImplemented,
			"no model catalog is configured for this deployment").
			WithCode("catalog_not_configured")
	}
	buf := getBuf()
	defer putBuf(buf)
	*buf = appendModelList(*buf, cfg.models.Models(), rq.Principal)

	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(*buf)))
	w.WriteHeader(http.StatusOK)
	if rq.Method != http.MethodHead {
		_, _ = w.Write(*buf)
	}
	return nil
}

// handleModelRetrieve serves GET /v1/models/{id}.
//
// It is filtered by the calling key's allow-list exactly as the list endpoint
// is (COMPATIBILITY §7.4), and the filter is the whole point: a client that
// cannot see a model in the list must not be able to confirm it exists by
// asking for it directly. A disallowed model is therefore answered 404
// `model_not_found` rather than 403 — the two would otherwise differ, and the
// difference is an enumeration oracle over another key's catalogue.
func (s *Server) handleModelRetrieve(w http.ResponseWriter, rq *Request) error {
	cfg := rq.srv.snap.Load()
	if cfg.models == nil {
		return NewError(http.StatusNotImplemented, TypeNotImplemented,
			"no model catalog is configured for this deployment").
			WithCode("catalog_not_configured")
	}
	// The id is one path segment and is compared whole: a model name is opaque
	// and nothing splits it on ':' or '/' (DESIGN §2.1).
	id := rq.Param("id")
	models := cfg.models.Models()
	var found *Model
	for i := range models {
		if models[i].ID == id {
			found = &models[i]
			break
		}
	}
	if found == nil || (rq.Principal != nil && !rq.Principal.AllowsModel(id)) {
		// TypeNotFound, not TypeInvalidRequest: a raiser sets the canonical
		// (Anthropic) spelling and [Error.ForFamily] projects it, so no site has
		// to know which family it is on. This route is OpenAI-shaped and the
		// projection folds it back to invalid_request_error; writing that here
		// would be right by accident.
		return NewError(http.StatusNotFound, TypeNotFound,
			"no such model").WithCode(CodeModelNotFound).WithParam("model")
	}

	buf := getBuf()
	defer putBuf(buf)
	*buf = appendModel(*buf, found)

	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(*buf)))
	w.WriteHeader(http.StatusOK)
	if rq.Method != http.MethodHead {
		_, _ = w.Write(*buf)
	}
	return nil
}

// appendModelList renders the list body.
func appendModelList(dst []byte, models []Model, p Principal) []byte {
	dst = append(dst, `{"object":"list","data":[`...)
	first := true
	for i := range models {
		m := &models[i]
		if p != nil && !p.AllowsModel(m.ID) {
			continue
		}
		if !first {
			dst = append(dst, ',')
		}
		first = false
		dst = appendModel(dst, m)
	}
	return append(dst, ']', '}')
}

// appendModel renders one model object. The list endpoint and the retrieve
// endpoint share it so the two cannot drift; a client that reads `created` from
// one and caches on it must see the same constant from the other.
func appendModel(dst []byte, m *Model) []byte {
	dst = append(dst, `{"id":`...)
	dst = appendJSONString(dst, m.ID)
	dst = append(dst, `,"object":"model","created":`...)
	dst = appendInt(dst, ModelsCreated)
	dst = append(dst, `,"owned_by":`...)
	owner := m.OwnedBy
	if owner == "" {
		owner = DefaultOwnedBy
	}
	dst = appendJSONString(dst, owner)
	return append(dst, '}')
}
