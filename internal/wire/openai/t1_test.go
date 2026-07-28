package openai

import (
	"bytes"
	"mime"
	"mime/multipart"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/canonical"
)

// Golden bytes for the moderations, audio and image surfaces.
//
// Each one decodes to an internal/canonical type and encodes from it. There is
// no assertion here that the output equals the input body — that would pass for
// a pass-through — the assertions are on the NEUTRAL form in the middle and on
// the exact bytes that come out of it.

// ---------------------------------------------------------------------------
// Moderations
// ---------------------------------------------------------------------------

func TestModerationRequestGolden(t *testing.T) {
	body := []byte(`{"input":"is this ok?","model":"omni-moderation:latest"}`)
	req, err := DecodeModerationRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "omni-moderation:latest" {
		t.Errorf("model %q — the name is opaque (DESIGN §2.1)", req.Model)
	}
	if len(req.Inputs) != 1 || req.Inputs[0].Text != "is this ok?" {
		t.Fatalf("inputs %+v", req.Inputs)
	}
	if !req.StringForm {
		t.Error("the bare-string form was not recorded, so a round trip would rewrite the caller's request")
	}
	got, err := MarshalModerationRequest(req, "upstream-id")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"input":"is this ok?","model":"upstream-id"}`
	if string(got) != want {
		t.Fatalf("request bytes\n got: %s\nwant: %s", got, want)
	}
}

func TestModerationArrayInputRoundTrip(t *testing.T) {
	body := []byte(`{"input":["a","b"],"model":"m"}`)
	req, err := DecodeModerationRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.StringForm {
		t.Error("an array input was recorded as the string form")
	}
	got, err := MarshalModerationRequest(req, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"input":["a","b"],"model":"m"}` {
		t.Fatalf("array form did not round trip: %s", got)
	}
}

// TestModerationResponseGolden pins the three parallel category objects. They
// are rendered from an ordered slice rather than from Go maps, because
// encoding/json sorts a map and would reorder every response.
func TestModerationResponseGolden(t *testing.T) {
	upstream := []byte(`{"id":"modr-1","model":"upstream-id","results":[{"flagged":true,` +
		`"categories":{"violence":true,"hate":false},` +
		`"category_scores":{"violence":0.9,"hate":0.01},` +
		`"category_applied_input_types":{"violence":["text"]}}]}`)
	resp, err := DecodeModerationResponse(upstream, "omni-moderation:latest")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Model != "omni-moderation:latest" {
		t.Errorf("model %q — §7.2 puts the CLIENT's name in the body", resp.Model)
	}
	if len(resp.Results) != 1 || !resp.Results[0].Flagged {
		t.Fatalf("results %+v", resp.Results)
	}
	got, err := MarshalModerationResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"modr-1","model":"omni-moderation:latest","results":[{"flagged":true,` +
		`"categories":{"hate":false,"violence":true},` +
		`"category_scores":{"hate":0.01,"violence":0.9},` +
		`"category_applied_input_types":{"violence":["text"]}}]}`
	if string(got) != want {
		t.Fatalf("response bytes\n got: %s\nwant: %s", got, want)
	}
}

// ---------------------------------------------------------------------------
// Audio
// ---------------------------------------------------------------------------

func TestSpeechRequestGolden(t *testing.T) {
	body := []byte(`{"model":"tts:1","input":"hello","voice":"alloy","response_format":"wav"}`)
	req, err := DecodeSpeechRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Voice != "alloy" || req.Format != "wav" {
		t.Fatalf("neutral form %+v", req)
	}
	got, err := MarshalSpeechRequest(req, "upstream-id")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"upstream-id","input":"hello","voice":"alloy","response_format":"wav"}`
	if string(got) != want {
		t.Fatalf("request bytes\n got: %s\nwant: %s", got, want)
	}
	if mt := SpeechMediaType(req.Format); mt != "audio/wav" {
		t.Errorf("media type %q for wav", mt)
	}
	// The default container is mp3, and a client handed the wrong label plays
	// nothing — so the empty case is pinned too.
	if mt := SpeechMediaType(""); mt != "audio/mpeg" {
		t.Errorf("default media type %q", mt)
	}
}

func TestTranscriptionFormRoundTrip(t *testing.T) {
	form := &canonical.Form{
		Values: map[string][]string{
			"model":                       {"whisper:1"},
			"language":                    {"ko"},
			"response_format":             {"verbose_json"},
			"timestamp_granularities[]":   {"word", "segment"},
			"a_backend_knob_dorang_lacks": {"7"},
		},
		Files: []canonical.File{{
			Field: "file", Name: "clip.wav", MediaType: "audio/wav", Data: []byte("RIFFdata"),
		}},
	}
	req, err := DecodeTranscriptionForm(form, false)
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "whisper:1" || req.Language != "ko" {
		t.Fatalf("neutral form %+v", req)
	}
	if len(req.TimestampGranularities) != 2 {
		t.Errorf("granularities %v — the bracketed spelling is the same parameter", req.TimestampGranularities)
	}
	if req.Extra["a_backend_knob_dorang_lacks"] != "7" {
		t.Errorf("an unmodelled form field was dropped: %v", req.Extra)
	}
	if string(req.File.Data) != "RIFFdata" {
		t.Errorf("file bytes %q", req.File.Data)
	}

	body, _, err := EncodeTranscriptionForm(req, "upstream-id", "BOUNDARY")
	if err != nil {
		t.Fatal(err)
	}
	got := parseFormBack(t, body, "BOUNDARY")
	if got.Get("model") != "upstream-id" {
		t.Errorf("the upstream model did not reach the form: %q", got.Get("model"))
	}
	if f := got.File("file"); f == nil || string(f.Data) != "RIFFdata" || f.Name != "clip.wav" {
		t.Errorf("the file part did not survive: %+v", f)
	}
	if len(got.All("timestamp_granularities")) != 2 {
		t.Errorf("granularities did not survive: %v", got.Values)
	}
}

// TestTranslationDropsLanguage is a per-endpoint parameter rule, not a general
// one: the translation surface's output is English by definition and rejects an
// input-language hint. Forwarding it produces a 400 the caller has never seen.
func TestTranslationDropsLanguage(t *testing.T) {
	form := &canonical.Form{
		Values: map[string][]string{"model": {"m"}, "language": {"ko"}},
		Files:  []canonical.File{{Field: "file", Name: "a.wav", Data: []byte("x")}},
	}
	req, err := DecodeTranscriptionForm(form, true)
	if err != nil {
		t.Fatal(err)
	}
	if req.Language != "" {
		t.Errorf("language %q reached a translation request", req.Language)
	}
	body, _, err := EncodeTranscriptionForm(req, "m", "B")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("language")) {
		t.Errorf("language reached the wire: %s", body)
	}
}

func TestTranscriptionResponseGolden(t *testing.T) {
	upstream := []byte(`{"task":"transcribe","language":"korean","duration":2.5,"text":"안녕",` +
		`"usage":{"type":"tokens","input_tokens":14,"output_tokens":4,"total_tokens":18}}`)
	resp, err := DecodeTranscriptionResponse(upstream, "application/json")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "안녕" || resp.Language != "korean" {
		t.Fatalf("neutral form %+v", resp)
	}
	if resp.Usage.InputTokens != 14 || resp.Usage.OutputTokens != 4 {
		t.Errorf("usage %+v", resp.Usage)
	}
	if _, ok := resp.Extra["task"]; !ok {
		t.Errorf("the unmodelled `task` member was dropped: %v", resp.Extra)
	}
	got, mt, err := MarshalTranscriptionResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if mt != "application/json" {
		t.Errorf("media type %q", mt)
	}
	// Raw UTF-8, not \uXXXX, and no HTML escaping (COMPATIBILITY 2.1a).
	want := `{"text":"안녕","language":"korean","duration":2.5,` +
		`"usage":{"type":"tokens","input_tokens":14,"output_tokens":4,"total_tokens":18},"task":"transcribe"}`
	if string(got) != want {
		t.Fatalf("response bytes\n got: %s\nwant: %s", got, want)
	}
}

// TestTranscriptionRawFormatIsNotRewritten covers response_format: srt.
//
// A gateway that re-renders a subtitle file from parsed fields changes the cue
// numbering, and no client asked it to.
func TestTranscriptionRawFormatIsNotRewritten(t *testing.T) {
	srt := []byte("1\n00:00:00,000 --> 00:00:02,500\n안녕\n")
	resp, err := DecodeTranscriptionResponse(srt, "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Raw == nil {
		t.Fatal("a non-JSON body was parsed as JSON")
	}
	got, mt, err := MarshalTranscriptionResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, srt) || mt != "text/plain" {
		t.Fatalf("raw body\n got %q (%s)\nwant %q", got, mt, srt)
	}
}

// ---------------------------------------------------------------------------
// Images
// ---------------------------------------------------------------------------

func TestImageRequestGolden(t *testing.T) {
	body := []byte(`{"prompt":"a cat","model":"image:1","n":2,"size":"1024x1024","response_format":"b64_json"}`)
	req, err := DecodeImageRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Op != canonical.ImageGenerate || req.Size != "1024x1024" {
		t.Fatalf("neutral form %+v", req)
	}
	got, err := MarshalImageRequest(req, "upstream-id")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"prompt":"a cat","model":"upstream-id","n":2,"size":"1024x1024","response_format":"b64_json"}`
	if string(got) != want {
		t.Fatalf("request bytes\n got: %s\nwant: %s", got, want)
	}
}

func TestImageEditFormRoundTrip(t *testing.T) {
	form := &canonical.Form{
		Values: map[string][]string{
			"model": {"image:1"}, "prompt": {"add a hat"}, "n": {"1"}, "size": {"auto"},
		},
		Files: []canonical.File{
			{Field: "image", Name: "in.png", MediaType: "image/png", Data: []byte("PNG")},
			{Field: "mask", Name: "m.png", MediaType: "image/png", Data: []byte("MASK")},
		},
	}
	req, err := DecodeImageForm(form, canonical.ImageEdit)
	if err != nil {
		t.Fatal(err)
	}
	if req.Op != canonical.ImageEdit || req.Prompt != "add a hat" {
		t.Fatalf("neutral form %+v", req)
	}
	if len(req.Images) != 1 || req.Mask == nil {
		t.Fatalf("image parts %d mask %v", len(req.Images), req.Mask)
	}
	// "auto" is a real size and is why nothing splits this string on 'x'.
	if req.Size != "auto" {
		t.Errorf("size %q", req.Size)
	}
	body, err := EncodeImageForm(req, "upstream-id", "BOUNDARY")
	if err != nil {
		t.Fatal(err)
	}
	got := parseFormBack(t, body, "BOUNDARY")
	if got.Get("model") != "upstream-id" || got.Get("prompt") != "add a hat" {
		t.Errorf("fields %v", got.Values)
	}
	if f := got.File("image"); f == nil || string(f.Data) != "PNG" {
		t.Errorf("image part %+v", f)
	}
	if f := got.File("mask"); f == nil || string(f.Data) != "MASK" {
		t.Errorf("mask part %+v", f)
	}
}

// TestImageVariationDropsPrompt is the second per-endpoint parameter rule: the
// variation surface takes no prompt and rejects one.
func TestImageVariationDropsPrompt(t *testing.T) {
	form := &canonical.Form{
		Values: map[string][]string{"model": {"m"}, "prompt": {"nope"}},
		Files:  []canonical.File{{Field: "image", Name: "a.png", Data: []byte("P")}},
	}
	req, err := DecodeImageForm(form, canonical.ImageVariation)
	if err != nil {
		t.Fatal(err)
	}
	if req.Prompt != "" {
		t.Errorf("prompt %q reached a variation request", req.Prompt)
	}
}

func TestImageResponseGolden(t *testing.T) {
	upstream := []byte(`{"created":1753660800,"data":[{"b64_json":"AAA","revised_prompt":"a cat, sitting"}],` +
		`"size":"1024x1024","output_format":"png",` +
		`"usage":{"input_tokens":9,"output_tokens":1500,"total_tokens":1509,` +
		`"input_tokens_details":{"text_tokens":9,"image_tokens":0}}}`)
	resp, err := DecodeImageResponse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.InputTokens != 9 || resp.Usage.OutputTokens != 1500 {
		t.Errorf("usage %+v", resp.Usage)
	}
	got, err := MarshalImageResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"created":1753660800,"data":[{"b64_json":"AAA","revised_prompt":"a cat, sitting"}],` +
		`"output_format":"png","size":"1024x1024",` +
		`"usage":{"input_tokens":9,"output_tokens":1500,"total_tokens":1509}}`
	if string(got) != want {
		t.Fatalf("response bytes\n got: %s\nwant: %s", got, want)
	}
}

// ---------------------------------------------------------------------------
// Strictness, on every surface that takes a body
// ---------------------------------------------------------------------------

// TestT1DecodesAreCaseSensitive is COMPATIBILITY 2.0 across the whole T1
// surface. A single route that fold-matches `Model` re-opens W10 on that route,
// so the property is asserted per surface rather than once.
func TestT1DecodesAreCaseSensitive(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		model func([]byte) (string, error)
	}{
		{"moderations", `{"Model":"expensive","input":"x"}`, func(b []byte) (string, error) {
			r, err := DecodeModerationRequest(b)
			if err != nil {
				return "", err
			}
			return r.Model, nil
		}},
		{"speech", `{"Model":"expensive","input":"x","voice":"a"}`, func(b []byte) (string, error) {
			r, err := DecodeSpeechRequest(b)
			if err != nil {
				return "", err
			}
			return r.Model, nil
		}},
		{"images", `{"Model":"expensive","prompt":"x"}`, func(b []byte) (string, error) {
			r, err := DecodeImageRequest(b)
			if err != nil {
				return "", err
			}
			return r.Model, nil
		}},
		{"responses", `{"Model":"expensive","input":"x"}`, func(b []byte) (string, error) {
			r, err := DecodeResponsesRequest(b)
			if err != nil {
				return "", err
			}
			return r.Model, nil
		}},
	}
	for _, c := range cases {
		got, err := c.model([]byte(c.body))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != "" {
			t.Errorf("%s: model %q resolved from a differently-cased key — this is the W10 bypass", c.name, got)
		}
	}
}

// parseFormBack reads an encoded multipart body back into a neutral form, so a
// round-trip assertion compares parsed fields rather than a byte layout that
// mime/multipart owns.
func parseFormBack(t *testing.T, body []byte, boundary string) *canonical.Form {
	t.Helper()
	if !strings.Contains(string(body), "--"+boundary) {
		t.Fatalf("the boundary %q is not in the body", boundary)
	}
	_, params, err := mime.ParseMediaType("multipart/form-data; boundary=" + boundary)
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	out := &canonical.Form{Values: map[string][]string{}}
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(part)
		if fn := part.FileName(); fn != "" {
			out.Files = append(out.Files, canonical.File{
				Field: part.FormName(), Name: fn,
				MediaType: part.Header.Get("Content-Type"), Data: buf.Bytes(),
			})
		} else {
			out.Values[part.FormName()] = append(out.Values[part.FormName()], buf.String())
		}
		_ = part.Close()
	}
	return out
}
