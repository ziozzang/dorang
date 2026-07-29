package anthropic

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ziozzang/dorang/internal/wire/wirejson"
)

// The differential between this package's two serializers. See
// internal/wire/openai/append_test.go for what it is and why it is driven by
// decoding rather than by construction.

func appendAgrees(t *testing.T, what string, v any) {
	t.Helper()
	a, ok := v.(wirejson.Appender)
	if !ok {
		t.Fatalf("%s: %T does not implement wirejson.Appender", what, v)
	}
	want, wantErr := Marshal(v)
	got, gotErr := wirejson.MarshalAppender(a)
	if (wantErr == nil) != (gotErr == nil) {
		t.Fatalf("%s: MarshalJSON err=%v, AppendJSON err=%v", what, wantErr, gotErr)
	}
	if wantErr != nil {
		return
	}
	if string(want) != string(got) {
		t.Fatalf("%s:\n  MarshalJSON %s\n  AppendJSON  %s", what, want, got)
	}
}

func decodeThenAgree[T any](t *testing.T, what string, body []byte) {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		return
	}
	appendAgrees(t, what, v)
}

var appendSeeds = []string{
	`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
	`{"model":"m","max_tokens":16,"system":[{"type":"text","text":"a && b <x>",` +
		`"cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"t"},{"type":"image","source":{"type":"base64",` +
		`"media_type":"image/png","data":"AA"}}]}]}`,
	`{"model":"m","max_tokens":1,"messages":[{"role":"assistant","content":[` +
		`{"type":"tool_use","id":"t1","name":"f","input":{ "q" : 1 }},` +
		`{"type":"thinking","thinking":"","signature":"sig"}]}],` +
		`"tools":[{"name":"f","input_schema":{ "type" : "object" }}],` +
		`"metadata":{"user_id":"u","vendor":1},"context_management":{"x":[1]}}`,
	`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"r"}]},` +
		`{"type":"search_result","source":"a string where an image has an object"}]}]}`,
	`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[` +
		`{"type":"text","text":"out"}],"stop_reason":"end_turn","stop_sequence":null,` +
		`"usage":{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":3,"x":9}}`,
	`{}`,
	`{"messages":null}`,
	`"a && b <tag> \u2028"`,
	`[{"type":"text","text":"a"},{"type":"tool_use","id":"i","name":"n","input":{}}]`,
	`[]`,
	`null`,
}

// FuzzAppendAgreesWithMarshal is the differential.
func FuzzAppendAgreesWithMarshal(f *testing.F) {
	for _, s := range appendSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		decodeThenAgree[Request](t, "Request", body)
		decodeThenAgree[Response](t, "Response", body)
		decodeThenAgree[Message](t, "Message", body)
		decodeThenAgree[ContentBlock](t, "ContentBlock", body)
		decodeThenAgree[Tool](t, "Tool", body)
		decodeThenAgree[Usage](t, "Usage", body)
		decodeThenAgree[Metadata](t, "Metadata", body)
		decodeThenAgree[BlockList](t, "BlockList", body)
	})
}

func TestAppendAgreesOnSeeds(t *testing.T) {
	for _, s := range appendSeeds {
		body := []byte(s)
		decodeThenAgree[Request](t, "Request", body)
		decodeThenAgree[Response](t, "Response", body)
		decodeThenAgree[Message](t, "Message", body)
		decodeThenAgree[ContentBlock](t, "ContentBlock", body)
		decodeThenAgree[Tool](t, "Tool", body)
		decodeThenAgree[Usage](t, "Usage", body)
		decodeThenAgree[Metadata](t, "Metadata", body)
		decodeThenAgree[BlockList](t, "BlockList", body)
	}
}

// TestEveryMarshalerIsAnAppender is the coverage claim. This package's wire
// types are all on the request or response path of the same route, so there is
// no exemption list.
func TestEveryMarshalerIsAnAppender(t *testing.T) {
	for _, v := range []any{
		Request{}, Response{}, Message{}, ContentBlock{}, BlockList{}, Tool{},
		Metadata{}, Usage{},
	} {
		if _, ok := v.(wirejson.Appender); !ok {
			t.Errorf("%s implements MarshalJSON but not AppendJSON",
				reflect.TypeOf(v).Name())
		}
	}
}

// TestRequestSubtreeIsPlanned asserts the property rather than a proxy for it:
// encoding a Messages request must not hand a single value back to
// encoding/json. See the openai test of the same name.
func TestRequestSubtreeIsPlanned(t *testing.T) {
	req, err := DecodeRequest([]byte(appendSeeds[2]))
	if err != nil {
		t.Fatal(err)
	}
	before := wirejson.Delegations()
	if _, err := MarshalRequest(req, &EncodeOptions{DefaultMaxTokens: 16}); err != nil {
		t.Fatal(err)
	}
	if n := wirejson.Delegations() - before; n != 0 {
		t.Errorf("encoding a Messages request delegated %d values to encoding/json", n)
	}
}
