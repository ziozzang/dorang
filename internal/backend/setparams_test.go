package backend

import (
	"context"
	"github.com/ziozzang/dorang/pkg/catalog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExperientialRequiresZDRConfirmation(t *testing.T) {
	for _, verdict := range []string{"", "false", "true"} {
		for _, stream := range []bool{false, true} {
			f := newFakeUpstream(t)
			f.header = http.Header{"X-Gateway-Zdr": []string{verdict}}
			f.answer(200, `{"id":"t","object":"chat.completion","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
			p, err := NewProvider(Spec{Name: "p1", Kind: "experiential-zdr", API: catalog.APIOpenAIChat, BaseURL: f.srv.URL, SetParams: map[string]any{"provider": map[string]any{"zdr": true}}})
			if err != nil {
				t.Fatal(err)
			}
			call := chatCall(catalog.APIOpenAIChat)
			call.Stream = stream
			if stream && verdict == "true" {
				f.header.Set("Content-Type", "text/event-stream")
				f.answer(200, "data: "+`{"id":"t","object":"chat.completion.chunk","model":"upstream-model","choices":[{"index":0,"delta":{"content":"OK"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
			}
			w := httptest.NewRecorder()
			res := testBackend("test").Do(context.Background(), target(p), call, w)
			if verdict != "true" {
				if res.Err == nil || res.Err.Code != "zdr_confirmation_missing" || res.FirstByteSent || len(res.Body) != 0 || w.Body.Len() != 0 {
					t.Fatalf("unconfirmed ZDR response escaped: verdict=%q stream=%v", verdict, stream)
				}
			} else if res.Err != nil || w.Result().Header.Get("X-Gateway-Zdr") != "true" || (stream && !res.FirstByteSent) {
				t.Fatalf("confirmed response failed: %v", res.Err)
			}
		}
	}
	if _, err := NewProvider(Spec{Name: "p", Kind: "experiential-zdr", API: catalog.APIOpenAIChat, BaseURL: "https://example.invalid"}); err == nil {
		t.Fatal("ZDR kind accepted without strict request policy")
	}
}

func TestSetParamsCopiesOperatorValuesAndRefusesNonJSON(t *testing.T) {
	policy := map[string]any{"zdr": true}
	p, err := NewProvider(Spec{Name: "p", API: catalog.APIOpenAIChat, BaseURL: "https://example.invalid", SetParams: map[string]any{"provider": policy}})
	if err != nil {
		t.Fatal(err)
	}
	policy["zdr"] = false
	body, err := p.applySetParams([]byte(`{"provider":{"zdr":false}}`), "")
	if err != nil || string(body) != `{"provider":{"zdr":true}}` {
		t.Fatal("policy changed after construction")
	}
	if _, err := p.applySetParams([]byte("multipart"), "multipart/form-data"); err == nil {
		t.Fatal("non-JSON silently ignored operator policy")
	}
}
