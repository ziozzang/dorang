package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/store"
	"github.com/ziozzang/dorang/internal/wire/openai"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// The Responses API's server-side state.
//
// DESIGN §9.2 [R1-C7]: `responses_store` is not optional. The endpoint carries
// `store: true` plus `previous_response_id`, and a gateway that serves it with
// nowhere to keep the state has only two runtime options, both of which break
// the compatibility promise — reject the request, or ignore the reference and
// answer from a truncated conversation.
//
// dorang owns the state rather than delegating it upstream, and that is a
// decision with consequences worth stating:
//
//   - The response id is dorang's. An upstream id would be meaningless to this
//     store and would change under a fail-back hop, so a client's saved
//     reference would resolve on one attempt and 404 on the next.
//   - The upstream is asked to store NOTHING. Two stores would be two answers to
//     "what is the conversation", and they diverge the first time a hop lands on
//     a different deployment.
//   - A stored exchange is scoped to the credential that created it. The lookup
//     takes the owner as part of the key, so another caller's response id is
//     indistinguishable from one that does not exist.

// DefaultResponsesTTL is how long a stored response is resolvable.
//
// It is a default, not a policy: §9.2 says entries expire and retention is
// configurable per tier. Thirty days matches the vendor's own window, which is
// what a client's saved reference was built against.
const DefaultResponsesTTL = 30 * 24 * time.Hour

// responseIDPrefix marks an id as dorang's own. Clients match on it and some
// log scrapers key on it, so it is the vendor's spelling rather than a dorang
// one.
const responseIDPrefix = "resp_"

// storedExchange is what one row's `items` column holds.
//
// It is an object rather than a bare item array because two different readers
// need two different things from it, and reconstructing either from the other
// is lossy:
//
//   - Chaining needs input ++ response.output, as conversation history.
//   - GET /v1/responses/{id} needs the RESPONSE THAT WAS SERVED, byte for byte,
//     including usage and the echoed request fields. Re-rendering it from items
//     would drop the token counts and hand the client dorang's second opinion
//     of its own answer.
//
// The output items are not duplicated: they live inside Response and chaining
// reads them from there.
type storedExchange struct {
	Input    []openai.ResponseItem `json:"input"`
	Response json.RawMessage       `json:"response"`
}

// responsesStore is the app-side view of `responses_store`.
type responsesStore struct {
	st  *store.Store
	ttl time.Duration
	now func() time.Time
}

func newResponsesStore(st *store.Store, now func() time.Time) *responsesStore {
	if st == nil {
		return nil
	}
	return &responsesStore{st: st, ttl: DefaultResponsesTTL, now: now}
}

// notFound is the one answer a caller gets for "no such response" and for
// "someone else's response". They are deliberately the same (COMPATIBILITY
// §11.2's "Unknown resource" row): distinguishing them turns the endpoint into
// an oracle over other callers' ids.
func responseNotFound() error {
	// The canonical spelling; server.Error.ForFamily projects it onto the
	// caller's family (COMPATIBILITY §11.2's two type columns).
	return server.NewError(http.StatusNotFound, server.TypeNotFound,
		"no such response").WithCode("not_found").WithParam("previous_response_id")
}

func (r *responsesStore) get(ctx context.Context, id, owner string) (*storedExchange, *store.StoredResponse, error) {
	if r == nil {
		return nil, nil, server.NewError(http.StatusNotImplemented, server.TypeNotImplemented,
			"this deployment has no store, so the Responses API cannot keep server-side state").
			WithCode("responses_store_unavailable")
	}
	row, err := r.st.GetStoredResponse(ctx, id, owner)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, responseNotFound()
		}
		return nil, nil, err
	}
	var ex storedExchange
	if err := json.Unmarshal(row.Items, &ex); err != nil {
		return nil, nil, server.NewError(http.StatusInternalServerError, server.TypeAPIError,
			"the stored response could not be read").WithCode("internal_error")
	}
	return &ex, row, nil
}

// ---------------------------------------------------------------------------
// Request path
// ---------------------------------------------------------------------------

// decodeResponses decodes POST /v1/responses and resolves its state reference.
func (d *dispatcher) decodeResponses(st *dispatchState, rq *server.Request, c *call, body []byte) error {
	var w openai.ResponsesRequest
	if err := canonical.StrictUnmarshal(body, &w); err != nil {
		return server.NewError(http.StatusBadRequest, server.TypeInvalidRequest,
			err.Error()).WithCode("invalid_request")
	}
	if w.Stream {
		// The Responses event stream is a different protocol from the
		// chat-completions one — a dozen typed events with their own sequence
		// numbering — not a framing of the same chunks. Answering the
		// non-streaming body to a client that asked for events would be a
		// half-working endpoint, which DESIGN §0.2 rejects in favour of a named
		// 501.
		return server.NewError(http.StatusNotImplemented, server.TypeNotImplemented,
			"streamed responses are not implemented; omit stream for the complete response").
			WithCode("responses_stream_not_implemented").WithParam("stream")
	}
	creq, err := openai.ResponsesRequestToCanonical(&w)
	if err != nil {
		return server.NewError(http.StatusBadRequest, server.TypeInvalidRequest,
			err.Error()).WithCode("invalid_request")
	}

	c.kind, c.clientAPI, c.creq = callResponses, catalog.APIOpenAIResponses, creq
	c.responseID = responseIDPrefix + rq.ID
	c.respEcho = &openai.ResponsesOptions{
		ID:                 c.responseID,
		Model:              rq.Model,
		Instructions:       w.Instructions,
		MaxOutputTokens:    w.MaxOutputTokens,
		Temperature:        w.Temperature,
		TopP:               w.TopP,
		Tools:              w.Tools,
		ToolChoice:         w.ToolChoice,
		ParallelToolCalls:  w.ParallelToolCalls,
		Text:               w.Text,
		Reasoning:          w.Reasoning,
		Store:              w.Store,
		PreviousResponseID: w.PreviousResponseID,
		Truncation:         w.Truncation,
		Metadata:           w.Metadata,
		User:               w.User,
	}

	if w.PreviousResponseID == "" {
		return nil
	}
	ex, _, err := st.responses.get(rq.Context(), w.PreviousResponseID, principalID(rq))
	if err != nil {
		return err
	}
	prev, perr := replayMessages(ex)
	if perr != nil {
		return perr
	}
	// The replayed turns go in FRONT of the new input, which is what the
	// reference has always meant by chaining. Appending them would answer the
	// old question with the new context.
	creq.Messages = append(prev, creq.Messages...)
	// The reference is resolved here and only here. It is dorang's own id,
	// so it must not travel upstream: a host that resolves references would
	// answer 404 for it, and one that applied it would chain the history a
	// second time on top of the replay above. The caller still sees it echoed
	// on the answer and the stored row still records it.
	c.prevResponseID = creq.PreviousResponseID
	creq.PreviousResponseID = ""
	return nil
}

// replayMessages rebuilds the conversation a stored exchange represents.
func replayMessages(ex *storedExchange) ([]canonical.Message, error) {
	msgs := openai.ItemsToMessages(ex.Input)
	if len(ex.Response) > 0 {
		var prev openai.ResponsesResponse
		if err := json.Unmarshal(ex.Response, &prev); err != nil {
			return nil, server.NewError(http.StatusInternalServerError, server.TypeAPIError,
				"the stored response could not be read").WithCode("internal_error")
		}
		msgs = append(msgs, openai.ItemsToMessages(prev.Output)...)
	}
	return msgs, nil
}

// storeResponse persists a completed Responses exchange.
//
// It runs after the answer is complete and before it is written: a stored
// response the client never received is a reference it can resolve to a turn it
// never saw. A failure to store is the request's failure, deliberately — a 200
// that silently did not store is a `previous_response_id` that 404s later, with
// nothing anywhere saying why.
func (d *dispatcher) storeResponse(ctx context.Context, st *dispatchState, c *call,
	rq *server.Request, body []byte) error {

	if c.kind != callResponses {
		return nil
	}
	// The surface defaults store to true, so a nil pointer is not false.
	if c.creq.Store != nil && !*c.creq.Store {
		return nil
	}
	if st.responses == nil {
		return server.NewError(http.StatusNotImplemented, server.TypeNotImplemented,
			"this deployment has no store, so `store: true` cannot be honoured; send store: false").
			WithCode("responses_store_unavailable").WithParam("store")
	}
	items, err := openai.MarshalResponsesItems(c.creq.Messages)
	if err != nil {
		return server.NewError(http.StatusInternalServerError, server.TypeAPIError,
			"the response could not be stored").WithCode("internal_error")
	}
	var input []openai.ResponseItem
	if err := json.Unmarshal(items, &input); err != nil {
		return server.NewError(http.StatusInternalServerError, server.TypeAPIError,
			"the response could not be stored").WithCode("internal_error")
	}
	enc, err := json.Marshal(storedExchange{Input: input, Response: body})
	if err != nil {
		return server.NewError(http.StatusInternalServerError, server.TypeAPIError,
			"the response could not be stored").WithCode("internal_error")
	}
	now := d.now()
	row := &store.StoredResponse{
		ID:                 c.responseID,
		CreatedAt:          now,
		ExpiresAt:          now.Add(st.responses.ttl),
		OwnerKeyID:         principalID(rq),
		PreviousResponseID: c.prevResponseID,
		ModelGroup:         c.model,
		Items:              enc,
		ReasoningBlobs:     reasoningBlobs(c.creq.Messages),
	}
	if err := st.responses.st.PutStoredResponse(ctx, row); err != nil {
		d.logf("app: storing response %s: %v", c.responseID, err)
		return server.NewError(http.StatusInternalServerError, server.TypeAPIError,
			"the response could not be stored").WithCode("internal_error")
	}
	return nil
}

// reasoningBlobs collects the opaque integrity-bearing reasoning handles of
// DESIGN §10.2 so they can be replayed byte-identically.
//
// They are stored beside the items rather than only inside them because that is
// what the column is for (§9.2) and because a future retention policy may want
// to drop them without dropping the conversation: they are the largest and the
// shortest-lived part of a stored exchange.
func reasoningBlobs(msgs []canonical.Message) []byte {
	var out []string
	for i := range msgs {
		for j := range msgs[i].Content {
			b := &msgs[i].Content[j]
			if b.Kind == canonical.KindThinking && b.Thinking != nil && b.Thinking.Signature != "" {
				out = append(out, b.Thinking.Signature)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	enc, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return enc
}

// ---------------------------------------------------------------------------
// Sub-resources
// ---------------------------------------------------------------------------

// extraRoutes is every route internal/app mounts on top of the built-in table.
//
// They are one list because internal/server applies last-registration-wins per
// pattern, and a route split across two lists would make which one is live
// depend on the order these are concatenated in.
func (a *App) extraRoutes() []server.Route {
	out := append(a.batchRoutes(), a.responsesRoutes()...)
	return append(out, a.adminRoutes()...)
}

// responsesRoutes mounts the stateful half of the Responses API.
//
// internal/server registers POST /v1/responses itself, because that one is an
// inference route and needs nothing but a dispatcher. These need the store, so
// they are mounted here and override the built-in declaration by pattern.
func (a *App) responsesRoutes() []server.Route {
	byMethod := func(m map[string]server.Handler) server.Handler {
		return func(w http.ResponseWriter, rq *server.Request) error {
			if h, ok := m[rq.Method]; ok {
				return h(w, rq)
			}
			return server.NewError(http.StatusMethodNotAllowed, server.TypeInvalidRequest,
				"method not allowed on this route").WithCode("method_not_allowed")
		}
	}
	return []server.Route{
		{
			Pattern: "/v1/responses/{id}",
			Methods: server.MethodGET | server.MethodDELETE,
			Name:    "responses_object",
			Family:  server.FamilyOpenAIResponses,
			// The sub-resources read a stored response back and delete it. No
			// model is called on any of them — the inference half is
			// /v1/responses, which is ModelAuthGate — so there is no allow-list
			// decision to make here. Ownership is enforced separately, against
			// principalID(rq) rather than a request parameter.
			ModelAuth: server.ModelAuthNone,
			Handler: byMethod(map[string]server.Handler{
				http.MethodGet:    a.handleResponseRetrieve,
				http.MethodDelete: a.handleResponseDelete,
			}),
		},
		{
			Pattern:   "/v1/responses/{id}/input_items",
			Methods:   server.MethodGET,
			Name:      "responses_input_items",
			Family:    server.FamilyOpenAIResponses,
			ModelAuth: server.ModelAuthNone,
			Handler:   a.handleResponseInputItems,
		},
		{
			Pattern:   "/v1/responses/{id}/cancel",
			Methods:   server.MethodPOST,
			Name:      "responses_cancel",
			Family:    server.FamilyOpenAIResponses,
			ModelAuth: server.ModelAuthNone,
			Handler:   a.handleResponseCancel,
		},
	}
}

func (a *App) responsesState() *responsesStore {
	if st := a.dispatch.state(); st != nil {
		return st.responses
	}
	return nil
}

func (a *App) handleResponseRetrieve(w http.ResponseWriter, rq *server.Request) error {
	ex, _, err := a.responsesState().get(rq.Context(), rq.Param("id"), principalID(rq))
	if err != nil {
		return err
	}
	// The bytes that were served, not a re-rendering of them.
	return writeBody(w, ex.Response, "application/json")
}

func (a *App) handleResponseDelete(w http.ResponseWriter, rq *server.Request) error {
	rs := a.responsesState()
	if rs == nil {
		return server.NewError(http.StatusNotImplemented, server.TypeNotImplemented,
			"this deployment has no store, so the Responses API cannot keep server-side state").
			WithCode("responses_store_unavailable")
	}
	id := rq.Param("id")
	ok, err := rs.st.DeleteStoredResponse(rq.Context(), id, principalID(rq))
	if err != nil {
		return err
	}
	if !ok {
		return responseNotFound()
	}
	return writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "object": "response.deleted", "deleted": true,
	})
}

func (a *App) handleResponseInputItems(w http.ResponseWriter, rq *server.Request) error {
	ex, _, err := a.responsesState().get(rq.Context(), rq.Param("id"), principalID(rq))
	if err != nil {
		return err
	}
	items := ex.Input
	if items == nil {
		items = []openai.ResponseItem{}
	}
	// Ids are synthesized here rather than stored, because the input items of a
	// converted request never had upstream ids: they were built from a neutral
	// message list. A client pages on them, so they have to be stable for one
	// response, which a positional id is and a random one is not.
	for i := range items {
		if items[i].ID == "" {
			items[i].ID = "msg_" + rq.Param("id") + "_" + strconv.Itoa(i)
		}
	}
	out := map[string]any{"object": "list", "data": items, "has_more": false}
	if len(items) > 0 {
		out["first_id"] = items[0].ID
		out["last_id"] = items[len(items)-1].ID
	}
	return writeJSON(w, http.StatusOK, out)
}

// handleResponseCancel is the one Responses sub-resource dorang does not serve.
//
// Cancellation applies only to a BACKGROUND response, and dorang has no
// background mode: every response it serves is complete before its id exists,
// so there is never a window in which cancelling one would mean anything. A 501
// with a named code says that; a 200 claiming to have cancelled something would
// be a lie a client acts on (DESIGN §0.2).
func (a *App) handleResponseCancel(w http.ResponseWriter, rq *server.Request) error {
	return server.NewError(http.StatusNotImplemented, server.TypeNotImplemented,
		"background responses are not implemented, so there is nothing to cancel").
		WithCode("responses_cancel_not_implemented").WithParam(rq.Path)
}
