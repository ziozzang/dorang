package rerank

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
	want, wantErr := wirejson.Marshal(v)
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

func agreeAll(t *testing.T, body []byte) {
	t.Helper()
	decodeThenAgree[Request](t, "Request", body)
	decodeThenAgree[Response](t, "Response", body)
	decodeThenAgree[Document](t, "Document", body)
	decodeThenAgree[Usage](t, "Usage", body)
	decodeThenAgree[Meta](t, "Meta", body)
	decodeThenAgree[BilledUnits](t, "BilledUnits", body)
}

var appendSeeds = []string{
	`{"model":"m","query":"q","documents":["a","b"],"top_n":2}`,
	`{"model":"m","query":"a && b <x>","documents":[{ "text" : "d" , "title" : "t" }],` +
		`"return_documents":true,"rank_fields":["title"],"vendor":{"a":[1,2]}}`,
	`{"id":"r","results":[{"index":0,"relevance_score":0.5,` +
		`"document":{ "text" : "d" }}],"usage":{"total_tokens":3,"input_tokens":1},` +
		`"meta":{"billed_units":{"search_units":1,"input_tokens":2},"api_version":{"v":"1"}}}`,
	`{}`,
	`"a bare string document"`,
}

// FuzzAppendAgreesWithMarshal is the differential.
func FuzzAppendAgreesWithMarshal(f *testing.F) {
	for _, s := range appendSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) { agreeAll(t, body) })
}

func TestAppendAgreesOnSeeds(t *testing.T) {
	for _, s := range appendSeeds {
		agreeAll(t, []byte(s))
	}
}

// TestEveryMarshalerIsAnAppender is the coverage claim.
func TestEveryMarshalerIsAnAppender(t *testing.T) {
	for _, v := range []any{
		Request{}, Response{}, Document{}, Usage{}, Meta{}, BilledUnits{},
	} {
		if _, ok := v.(wirejson.Appender); !ok {
			t.Errorf("%s implements MarshalJSON but not AppendJSON",
				reflect.TypeOf(v).Name())
		}
	}
}

// TestRequestSubtreeIsPlanned asserts the property rather than a proxy for it.
// See the openai test of the same name.
func TestRequestSubtreeIsPlanned(t *testing.T) {
	req, err := DecodeRequest([]byte(appendSeeds[1]))
	if err != nil {
		t.Fatal(err)
	}
	before := wirejson.Delegations()
	if _, err := MarshalRequest(req, nil); err != nil {
		t.Fatal(err)
	}
	if n := wirejson.Delegations() - before; n != 0 {
		t.Errorf("encoding a rerank request delegated %d values to encoding/json", n)
	}
}
