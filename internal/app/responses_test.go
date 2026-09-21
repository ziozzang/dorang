package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ziozzang/dorang/internal/canonical"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/store"
	"github.com/ziozzang/dorang/internal/wire/openai"
)

// The Responses API's server-side state (DESIGN §9.2 [R1-C7]).
//
// The wire conversion is tested in internal/wire/openai. What is tested here is
// the half that only exists because dorang keeps the state: a chained request
// resolves to the conversation that was actually served, and a caller cannot
// resolve another caller's.

func newTestResponses(t *testing.T) *responsesStore {
	t.Helper()
	st, _ := openTestStore(t, nil)
	return newResponsesStore(st, func() time.Time {
		return time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	})
}

// storeExchange writes one exchange the way storeResponse does, so the read
// paths are exercised against bytes the write path could actually produce.
func storeExchange(t *testing.T, rs *responsesStore, id, owner string, msgs []canonical.Message, body string) {
	t.Helper()
	items, err := openai.MarshalResponsesItems(msgs)
	if err != nil {
		t.Fatal(err)
	}
	var input []openai.ResponseItem
	if err := json.Unmarshal(items, &input); err != nil {
		t.Fatal(err)
	}
	enc, err := json.Marshal(storedExchange{Input: input, Response: json.RawMessage(body)})
	if err != nil {
		t.Fatal(err)
	}
	now := rs.now()
	err = rs.st.PutStoredResponse(context.Background(), &store.StoredResponse{
		ID:         id,
		CreatedAt:  now,
		ExpiresAt:  now.Add(rs.ttl),
		OwnerKeyID: owner,
		ModelGroup: "qwen3.5:397b",
		Items:      enc,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// requestWithParam builds a handler-level request with one captured path
// parameter, which is what these routes are keyed on.
func requestWithParam(t *testing.T, path, name, value string) *server.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	rq := &server.Request{HTTP: r, Method: http.MethodGet, Path: path}
	return rq.WithParams(server.Param{Name: name, Value: value})
}

// TestStoredExchangeReplaysTheWholeConversation is what previous_response_id
// means. A stored exchange holds the input it was asked with AND the output it
// produced; replaying only the output would answer the next turn from half a
// conversation, which is the "truncated conversation" failure §9.2 names.
func TestStoredExchangeReplaysTheWholeConversation(t *testing.T) {
	rs := newTestResponses(t)
	body := `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m",` +
		`"output":[{"type":"message","role":"assistant",` +
		`"content":[{"type":"output_text","text":"18C","annotations":[]}]}]}`
	storeExchange(t, rs, "resp_1", "key-1",
		[]canonical.Message{canonical.TextMessage(canonical.RoleUser, "weather?")}, body)

	ex, _, err := rs.get(context.Background(), "resp_1", "key-1")
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := replayMessages(ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("replayed %d turns, want 2 (the question and the answer): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != canonical.RoleUser || msgs[0].Content.Flatten() != "weather?" {
		t.Errorf("first turn %+v", msgs[0])
	}
	if msgs[1].Role != canonical.RoleAssistant || msgs[1].Content.Flatten() != "18C" {
		t.Errorf("second turn %+v", msgs[1])
	}
}

// TestStoredExchangeIsScopedToItsOwner: `previous_response_id` is a
// client-supplied identifier for server-side state, so a lookup that ignored
// the owner would make it a cross-tenant read primitive. Both the wrong-owner
// and the no-such-id answers are the same 404 with the same body, so the pair
// is not an existence oracle either.
func TestStoredExchangeIsScopedToItsOwner(t *testing.T) {
	rs := newTestResponses(t)
	storeExchange(t, rs, "resp_1", "key-1", nil, `{"id":"resp_1"}`)

	_, _, wrongOwner := rs.get(context.Background(), "resp_1", "key-2")
	_, _, noSuchID := rs.get(context.Background(), "resp_missing", "key-1")
	for name, err := range map[string]error{"wrong owner": wrongOwner, "no such id": noSuchID} {
		e, ok := err.(*server.Error)
		if !ok {
			t.Fatalf("%s: %v is not a server error", name, err)
		}
		if e.Status != http.StatusNotFound || e.Code != "not_found" {
			t.Errorf("%s: status %d code %q", name, e.Status, e.Code)
		}
	}
	if wrongOwner.Error() != noSuchID.Error() {
		t.Errorf("a hidden response is distinguishable from a missing one:\n %v\n %v",
			wrongOwner, noSuchID)
	}
}

// TestResponsesWithoutAStoreRefusesRatherThanForgetting.
//
// A deployment with no store cannot honour `store: true`. Answering 200 and not
// storing would hand the client a response id that 404s later, with nothing
// anywhere saying why — which is exactly the "promise without state" failure
// §9.2 [R1-C7] describes. A named 501 says it instead.
func TestResponsesWithoutAStoreRefusesRatherThanForgetting(t *testing.T) {
	var rs *responsesStore
	_, _, err := rs.get(context.Background(), "resp_1", "key-1")
	e, ok := err.(*server.Error)
	if !ok {
		t.Fatalf("%v is not a server error", err)
	}
	if e.Status != http.StatusNotImplemented || e.Code != "responses_store_unavailable" {
		t.Errorf("status %d code %q", e.Status, e.Code)
	}
}

// TestReasoningBlobsAreCollected: the opaque integrity-bearing reasoning
// handles of DESIGN §10.2 are stored so they can be replayed byte-identically.
// dorang never synthesizes one, so a conversation without any stores nothing.
func TestReasoningBlobsAreCollected(t *testing.T) {
	msgs := []canonical.Message{{
		Role: canonical.RoleAssistant,
		Content: canonical.Content{
			canonical.ThinkingBlock("thought", "sig-1"),
			canonical.TextBlock("answer"),
		},
	}}
	got := reasoningBlobs(msgs)
	if string(got) != `["sig-1"]` {
		t.Errorf("blobs %s", got)
	}
	if reasoningBlobs([]canonical.Message{canonical.TextMessage(canonical.RoleUser, "hi")}) != nil {
		t.Error("a conversation with no reasoning material stored a blob anyway")
	}
}

// TestAStreamedExchangeStoresWhatTheTerminalCarried: a streamed exchange is
// stored from the terminal event's own response object, so GET and chaining
// see exactly what the buffered path would have served — same row, same
// replay, decided only by the transport.
func TestAStreamedExchangeStoresWhatTheTerminalCarried(t *testing.T) {
	rs := newTestResponses(t)
	d := newDispatcher(nil, nil, func(string, ...any) {}, rs.now)
	st := &dispatchState{responses: rs}
	c := &call{
		kind: callResponses, responseID: "resp_s1", model: "m",
		creq: &canonical.Request{
			Messages: []canonical.Message{canonical.TextMessage(canonical.RoleUser, "weather?")},
		},
	}
	rq := &server.Request{Method: http.MethodPost, Path: "/v1/responses"}
	terminal := `{"id":"resp_s1","object":"response","created_at":1,"status":"completed","model":"m",` +
		`"output":[{"type":"message","role":"assistant",` +
		`"content":[{"type":"output_text","text":"18C","annotations":[]}]}]}`

	d.storeStreamedResponse(context.Background(), st, c, rq, []byte(terminal))

	ex, _, err := rs.get(context.Background(), "resp_s1", "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(ex.Response) != terminal {
		t.Errorf("stored body = %s, want the terminal object byte for byte", ex.Response)
	}
	msgs, err := replayMessages(ex)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Content.Flatten() != "weather?" || msgs[1].Content.Flatten() != "18C" {
		t.Errorf("replayed %+v, want the question and the answer", msgs)
	}
}

// TestResponsesSubResourceHandlers exercises the three routes that only exist
// because the state does.
func TestResponsesSubResourceHandlers(t *testing.T) {
	rs := newTestResponses(t)
	a := &App{dispatch: newDispatcher(nil, nil, func(string, ...any) {}, rs.now)}
	a.dispatch.swap(&dispatchState{responses: rs})

	body := `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[]}`
	storeExchange(t, rs, "resp_1", "",
		[]canonical.Message{canonical.TextMessage(canonical.RoleUser, "weather?")}, body)

	t.Run("retrieve returns the bytes that were served", func(t *testing.T) {
		w := httptest.NewRecorder()
		if err := a.handleResponseRetrieve(w, requestWithParam(t, "/v1/responses/resp_1", "id", "resp_1")); err != nil {
			t.Fatal(err)
		}
		if w.Body.String() != body {
			t.Fatalf("body\n got %s\nwant %s", w.Body.String(), body)
		}
	})

	t.Run("input_items lists the input half", func(t *testing.T) {
		w := httptest.NewRecorder()
		if err := a.handleResponseInputItems(w, requestWithParam(t, "/v1/responses/resp_1/input_items", "id", "resp_1")); err != nil {
			t.Fatal(err)
		}
		got := w.Body.String()
		for _, frag := range []string{`"object":"list"`, `"has_more":false`, `"weather?"`, `"first_id"`} {
			if !strings.Contains(got, frag) {
				t.Errorf("missing %s in %s", frag, got)
			}
		}
	})

	t.Run("cancel is a named 501", func(t *testing.T) {
		w := httptest.NewRecorder()
		err := a.handleResponseCancel(w, requestWithParam(t, "/v1/responses/resp_1/cancel", "id", "resp_1"))
		e, ok := err.(*server.Error)
		if !ok {
			t.Fatalf("%v is not a server error", err)
		}
		if e.Status != http.StatusNotImplemented || e.Code != "responses_cancel_not_implemented" {
			t.Errorf("status %d code %q", e.Status, e.Code)
		}
	})

	t.Run("delete removes it", func(t *testing.T) {
		w := httptest.NewRecorder()
		if err := a.handleResponseDelete(w, requestWithParam(t, "/v1/responses/resp_1", "id", "resp_1")); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(w.Body.String(), `"deleted":true`) {
			t.Fatalf("body %s", w.Body.String())
		}
		w = httptest.NewRecorder()
		if err := a.handleResponseRetrieve(w, requestWithParam(t, "/v1/responses/resp_1", "id", "resp_1")); err == nil {
			t.Error("a deleted response was still retrievable")
		}
	})
}
