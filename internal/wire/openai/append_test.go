package openai

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ziozzang/dorang/internal/wire/wirejson"
)

// The differential between this package's two serializers.
//
// Every wire type here has both a MarshalJSON — which encoding/json calls, and
// which reaches its nested types through encoding/json too — and an AppendJSON,
// which writes into its parent's buffer and reaches its nested types directly.
// They are two implementations of COMPATIBILITY 2.1a's byte contract and the
// only thing that makes the second one safe is that the first one is still here
// to be compared against.
//
// The comparison is driven by DECODING arbitrary bytes rather than by
// constructing values, for the reason the decode work found: the interesting
// values are the ones a caller can actually produce — an Extra map with a key
// that sorts oddly, a content array that arrived empty, a tool_choice carrying
// whitespace — and a hand-built value graph contains exactly the cases its
// author thought of.

// appendAgrees renders v both ways and requires the same bytes.
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

// decodeThenAgree decodes body into a fresh value of the type ptr points at and,
// when that succeeds, compares the two encoders on the result.
func decodeThenAgree[T any](t *testing.T, what string, body []byte) {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		return
	}
	appendAgrees(t, what, v)
}

var appendSeeds = []string{
	`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
	`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"a && b <x>"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,AA","detail":"low"}}]}]}`,
	`{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[` +
		`{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"},"index":0}]}]}`,
	`{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"f",` +
		`"parameters":{ "type" : "object" }},"cache_control":{"type":"ephemeral","ttl":"5m"}}],` +
		`"tool_choice":{ "type" : "function" , "function" : { "name" : "f" } }}`,
	`{"model":"m","messages":[{"role":"user","content":"x","vendor_thing":{"a":[1,2]}}],` +
		`"stop":["a","b"],"temperature":0.7,"top_p":1e-7,"logit_bias":{"b":1,"a":-2},` +
		`"metadata":{"z":"1","a":"2"},"seed":-9007199254740991,"unknown_top":null}`,
	`{"model":"m","messages":[{"role":"user","content":[]}],"stop":"one"}`,
	`{"id":"r","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"out","refusal":null},"finish_reason":"stop",` +
		`"logprobs":{ "content" : [ ] },"provider_specific_fields":{"native_finish_reason":"x"}}],` +
		`"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,` +
		`"prompt_tokens_details":{"cached_tokens":0,"audio_tokens":4},` +
		`"completion_tokens_details":{"reasoning_tokens":1},"cost":0.001},"timings":{"x":1}}`,
	`{}`,
	`{"messages":null}`,
	`{"model":"\u0000\ud800 \u2028 é","messages":[{"role":"","content":"\ud800"}]}`,
	// The other surfaces, so the list below is exercised rather than skipped.
	`{"model":"m","input":"say hi","instructions":"be brief","tools":[{"type":"web_search"}],` +
		`"text":{"format":{"type":"json_schema","name":"s","schema":{ "type":"object" }}},` +
		`"usage":{"input_tokens":1,"output_tokens":2,"input_tokens_details":{"cached_tokens":1},` +
		`"output_tokens_details":{"reasoning_tokens":0}},"vendor":[1,2]}`,
	`{"model":"m","prompt":["a","b"],"max_tokens":4,"echo":true,"logprobs":2,"vendor":{}}`,
	`{"model":"m","input":"hello","voice":"alloy","response_format":"mp3","speed":1.25,"x":1}`,
	`{"task":"transcribe","language":"en","duration":1.5,"text":"hi",` +
		`"segments":[{"id":0,"seek":0,"start":0,"end":1,"text":"hi","tokens":[1,2],` +
		`"temperature":0,"avg_logprob":-0.1,"compression_ratio":1,"no_speech_prob":0}],` +
		`"usage":{"type":"tokens","input_tokens":1,"output_tokens":2,"total_tokens":3}}`,
	`{"model":"m","prompt":"a cat","n":2,"size":"1024x1024","quality":"hd","vendor":true}`,
	`{"created":1,"data":[{"url":"http://x/y","revised_prompt":"a cat"}],` +
		`"usage":{"total_tokens":3,"input_tokens":1,"output_tokens":2}}`,
	`{"model":"m","input":["a","b"],"vendor":1}`,
	`"a && b <tag> \u2028"`,
	`["one","two"]`,
	`[{"type":"text","text":"a"},{"type":"input_text","text":"b"}]`,
	`[]`,
	`null`,
	`{"id":"modr-1","model":"m","results":[{"flagged":true,` +
		`"categories":{"hate":false},"category_scores":{"hate":0.1},` +
		`"category_applied_input_types":{"hate":["text"]},"vendor":9}]}`,
}

// agreeAll runs one body through every type that has an AppendJSON.
//
// It is one list rather than one call per target so that the fuzzer and the
// ordinary test cannot drift apart: a type added to the fast path and not to
// this list is caught by TestEveryMarshalerIsAnAppender, and a type on this
// list is checked by both.
func agreeAll(t *testing.T, body []byte) {
	t.Helper()
	decodeThenAgree[Request](t, "Request", body)
	decodeThenAgree[Response](t, "Response", body)
	decodeThenAgree[Message](t, "Message", body)
	decodeThenAgree[Part](t, "Part", body)
	decodeThenAgree[Tool](t, "Tool", body)
	decodeThenAgree[Usage](t, "Usage", body)
	decodeThenAgree[Choice](t, "Choice", body)
	decodeThenAgree[PromptTokensDetails](t, "PromptTokensDetails", body)
	decodeThenAgree[CompletionTokensDetails](t, "CompletionTokensDetails", body)
	decodeThenAgree[ResponsesRequest](t, "ResponsesRequest", body)
	decodeThenAgree[ResponseItem](t, "ResponseItem", body)
	decodeThenAgree[ResponsePart](t, "ResponsePart", body)
	decodeThenAgree[ResponsesTool](t, "ResponsesTool", body)
	decodeThenAgree[ResponsesUsage](t, "ResponsesUsage", body)
	decodeThenAgree[CompletionRequest](t, "CompletionRequest", body)
	decodeThenAgree[CompletionResponse](t, "CompletionResponse", body)
	decodeThenAgree[CompletionChoice](t, "CompletionChoice", body)
	decodeThenAgree[SpeechRequest](t, "SpeechRequest", body)
	decodeThenAgree[TranscriptionResponse](t, "TranscriptionResponse", body)
	decodeThenAgree[TranscriptionSegment](t, "TranscriptionSegment", body)
	decodeThenAgree[TranscriptionUsage](t, "TranscriptionUsage", body)
	decodeThenAgree[ImageRequest](t, "ImageRequest", body)
	decodeThenAgree[ImageResponse](t, "ImageResponse", body)
	decodeThenAgree[ImageUsage](t, "ImageUsage", body)
	decodeThenAgree[ModerationRequest](t, "ModerationRequest", body)
	decodeThenAgree[ModerationResponse](t, "ModerationResponse", body)
	decodeThenAgree[ModerationResult](t, "ModerationResult", body)
	// The string-or-array types decode from a bare string or a bare array, so
	// they only see the seeds that are one.
	decodeThenAgree[Content](t, "Content", body)
	decodeThenAgree[StopSequences](t, "StopSequences", body)
	decodeThenAgree[ResponseInput](t, "ResponseInput", body)
	decodeThenAgree[ResponseContent](t, "ResponseContent", body)
}

// FuzzAppendAgreesWithMarshal is the differential.
func FuzzAppendAgreesWithMarshal(f *testing.F) {
	for _, s := range appendSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) { agreeAll(t, body) })
}

// TestAppendAgreesOnSeeds runs the differential's corpus as an ordinary test, so
// `go test` alone catches a divergence the fuzzer would need to be run to find.
func TestAppendAgreesOnSeeds(t *testing.T) {
	for _, s := range appendSeeds {
		agreeAll(t, []byte(s))
	}
}

// TestEveryMarshalerIsAnAppender is the coverage claim, stated as a test rather
// than as a comment.
//
// A wire type that grows a MarshalJSON and no AppendJSON is not WRONG — it falls
// back to encoding/json, which is what its parent used to do for everything —
// but it is a type whose subtree is compacted once per level again, silently.
// The list below is the exemption list, and it is short on purpose: everything
// on the chat, Messages and Responses request paths must be on the fast path.
func TestEveryMarshalerIsAnAppender(t *testing.T) {
	exempt := map[string]bool{
		// Moderation is a cold path and these three build their object by hand
		// from a category list rather than from struct fields.
		"ModerationInput":   true,
		"moderationFlags":   true,
		"moderationScores":  true,
		"moderationApplied": true,
		// Legacy completions' prompt is four wire forms of one field.
		"Prompt": true,
	}
	types := []any{
		Request{}, Response{}, Message{}, Content{}, Part{}, Tool{}, Choice{},
		Usage{}, PromptTokensDetails{}, CompletionTokensDetails{}, StopSequences{},
		ResponsesRequest{}, ResponseInput{}, ResponseItem{}, ResponseContent{},
		ResponsePart{}, ResponsesTool{}, ResponsesUsage{}, InputTokensDetails{},
		OutputTokensDetails{}, CompletionRequest{}, CompletionResponse{},
		CompletionChoice{}, SpeechRequest{}, TranscriptionResponse{},
		TranscriptionSegment{}, TranscriptionUsage{}, ImageRequest{},
		ImageResponse{}, ImageUsage{}, ModerationRequest{}, ModerationResponse{},
		ModerationResult{},
	}
	for _, v := range types {
		name := reflect.TypeOf(v).Name()
		if exempt[name] {
			continue
		}
		if _, ok := v.(wirejson.Appender); !ok {
			t.Errorf("%s implements MarshalJSON but not AppendJSON: its subtree is "+
				"compacted once per nesting level", name)
		}
	}
}

// TestRequestSubtreeIsPlanned is the other half: implementing Appender buys
// nothing if the alias the method hands to the planner is REFUSED, because then
// the value goes straight back to encoding/json and the only thing that changed
// is which function called it.
//
// The assertion is the property itself rather than a proxy for it — the bytes
// are identical either way, by design, so they cannot say which path ran.
// [wirejson.Delegations] counts the values an append run handed back, and for a
// chat request with array content, tool calls, tool declarations and a cache
// breakpoint the answer has to be none.
func TestRequestSubtreeIsPlanned(t *testing.T) {
	body := benchBodyDeep(4 << 10)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	before := wirejson.Delegations()
	out, err := MarshalRequest(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := wirejson.Delegations() - before; n != 0 {
		t.Errorf("encoding a %d byte chat request delegated %d values to "+
			"encoding/json; every type on this path must have an AppendJSON and a "+
			"planned alias", len(out), n)
	}

	// The same for the response half, which the gateway encodes on every
	// request too.
	resp, err := DecodeResponse([]byte(appendSeeds[6]), nil)
	if err != nil {
		t.Fatal(err)
	}
	before = wirejson.Delegations()
	if _, err := MarshalResponse(resp, nil); err != nil {
		t.Fatal(err)
	}
	if n := wirejson.Delegations() - before; n != 0 {
		t.Errorf("encoding a chat response delegated %d values to encoding/json", n)
	}
}

// TestAppendCostsFewerAllocations is the consequence, reported rather than
// tightly bounded: the reflective path allocates a finished document at every
// level of the nesting and the append path allocates the buffer it grows.
func TestAppendCostsFewerAllocations(t *testing.T) {
	body := benchBodyDeep(4 << 10)
	req, err := DecodeRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	w, err := EncodeRequest(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	appended := testing.AllocsPerRun(50, func() {
		if buf, err := wirejson.MarshalAppended(w); err != nil || len(buf) == 0 {
			t.Fatal(err)
		}
	})
	reflective := testing.AllocsPerRun(50, func() {
		if buf, err := Marshal(w); err != nil || len(buf) == 0 {
			t.Fatal(err)
		}
	})
	t.Logf("4 KiB nested chat request: appended %.0f allocs, reflective %.0f",
		appended, reflective)
	if appended >= reflective {
		t.Errorf("the append path allocated %.0f objects against the reflective "+
			"path's %.0f", appended, reflective)
	}
}
