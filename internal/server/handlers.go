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
func (s *Server) baseRoutes() []*Route {
	inference := func(pattern, name string, family Family) *Route {
		return &Route{
			Pattern:   pattern,
			Methods:   MethodPOST,
			Name:      name,
			Family:    family,
			NeedsBody: true,
			Handler:   s.handleInference,
		}
	}
	models := func(pattern string) *Route {
		return &Route{
			Pattern: pattern,
			Methods: MethodGET | MethodHEAD,
			Name:    "models",
			Family:  FamilyModels,
			Handler: s.handleModels,
		}
	}
	health := func(pattern, name string, kind healthKind) *Route {
		return &Route{
			Pattern: pattern,
			Methods: MethodGET | MethodHEAD | MethodOPTIONS,
			Name:    name,
			Family:  FamilyHealth,
			Public:  true,
			Handler: s.healthHandler(kind),
		}
	}

	return []*Route{
		inference("/v1/chat/completions", "chat_completions", FamilyOpenAIChat),
		inference("/chat/completions", "chat_completions", FamilyOpenAIChat),
		inference("/v1/embeddings", "embeddings", FamilyOpenAIEmbeddings),
		inference("/embeddings", "embeddings", FamilyOpenAIEmbeddings),
		inference("/v1/messages", "messages", FamilyAnthropicMessages),
		inference("/v1/messages/count_tokens", "messages_count_tokens", FamilyAnthropicCountTokens),

		models("/v1/models"),
		models("/models"),

		// "liveliness" is not a typo and not a synonym dorang chose. The
		// widely-deployed proxy this surface has to interoperate with spells it
		// that way, container manifests in the field probe that exact path, and
		// the correctly-spelled "liveness" exists alongside it. Serving only one
		// breaks half the deployments.
		health("/health/liveliness", "health_liveness", healthLive),
		health("/health/liveness", "health_liveness", healthLive),
		health("/health/readiness", "health_readiness", healthReady),
		health("/health", "health", healthOverall),

		{
			Pattern: "/metrics",
			Methods: MethodGET | MethodHEAD,
			Name:    "metrics",
			Family:  FamilyMetrics,
			Public:  true,
			Handler: s.handleMetrics,
		},
	}
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
	// COMPATIBILITY §7.2 puts tag-routing misses at 401, and the reference
	// proxy answers a model outside a key's allow-list the same way. It reads
	// oddly — the caller authenticated fine — but it is what deployed clients
	// branch on, and a 403 here would be a divergence they notice.
	if rq.Principal != nil && !rq.Principal.AllowsModel(rq.Model) {
		return NewError(http.StatusUnauthorized, TypeAuthentication,
			"this key is not allowed to use the requested model").
			WithCode("model_not_allowed").WithParam("model")
	}

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
			if o := rq.srv.snap.Load().observer; o != nil {
				n := len(b)
				b = append(b, `,"shadow":`...)
				if grown := o.Health(b); len(grown) > n+10 {
					b = grown
				} else {
					b = b[:n]
				}
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
		dst = append(dst, '}')
	}
	return append(dst, ']', '}')
}
