package canonical

import (
	"bytes"
	"mime"
	"mime/multipart"
	"strings"
	"testing"
)

// The multipart form is the neutral representation of the three inference
// bodies that are not JSON (DESIGN §2.1's audio and image rows). It gets the
// same guarantee the JSON paths get from StrictBytes, by a different mechanism:
// field lookup is an exact map key.

func TestFormLookupIsExact(t *testing.T) {
	f := &Form{Values: map[string][]string{"model": {"m"}, "Model": {"expensive"}}}
	if got := f.Get("model"); got != "m" {
		t.Errorf("model %q", got)
	}
	if got := f.Get("MODEL"); got != "" {
		t.Errorf("a fold-matched field resolved: %q — this is the W10 bypass shape", got)
	}
}

// TestFormBracketedSpelling: a repeated field is sent as `name[]` by some HTTP
// libraries and as `name` by others, and which one a client uses says nothing
// about intent.
func TestFormBracketedSpelling(t *testing.T) {
	f := &Form{Values: map[string][]string{
		"timestamp_granularities":   {"word"},
		"timestamp_granularities[]": {"segment", "extra"},
	}}
	got := f.All("timestamp_granularities")
	if len(got) != 3 || got[0] != "word" {
		t.Fatalf("values %v", got)
	}
}

// TestFormTypedReadsRefuseGarbage: an unparseable number is not silently zero,
// and only the two exact boolean spellings are booleans. "1" is not true, and
// reading it as true enables something the caller never asked for.
func TestFormTypedReads(t *testing.T) {
	f := &Form{Values: map[string][]string{
		"temperature": {"0.25"}, "n": {"3"}, "stream": {"true"},
		"bad_float": {"warm"}, "bad_int": {"many"}, "truthy": {"1"},
	}}
	if v, ok := f.Float("temperature"); !ok || v != 0.25 {
		t.Errorf("temperature %v %v", v, ok)
	}
	if v, ok := f.Int("n"); !ok || v != 3 {
		t.Errorf("n %v %v", v, ok)
	}
	if v, ok := f.Bool("stream"); !ok || !v {
		t.Errorf("stream %v %v", v, ok)
	}
	for _, name := range []string{"bad_float", "missing"} {
		if _, ok := f.Float(name); ok {
			t.Errorf("%s parsed as a float", name)
		}
	}
	if _, ok := f.Int("bad_int"); ok {
		t.Error("bad_int parsed as an int")
	}
	if v, ok := f.Bool("truthy"); ok || v {
		t.Error(`"1" was read as a boolean`)
	}
}

// TestFormEncodeIsDeterministic: the field order is the caller's, then the rest
// sorted. A body whose bytes depend on map iteration cannot be golden-tested,
// and a request dorang cannot golden-test is one nobody can diff against a
// reference capture.
func TestFormEncodeIsDeterministic(t *testing.T) {
	build := func() *Form {
		return &Form{
			Values: map[string][]string{
				"model": {"m"}, "prompt": {"p"}, "zeta": {"z"}, "alpha": {"a"},
			},
			Files: []File{{Field: "file", Name: "a.wav", MediaType: "audio/wav", Data: []byte("RIFF")}},
		}
	}
	order := []string{"model", "prompt"}
	first, err := build().Encode("BOUNDARY", order)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		again, err := build().Encode("BOUNDARY", order)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("encoding is not deterministic:\n%s\n%s", first, again)
		}
	}
	body := string(first)
	if strings.Index(body, `name="model"`) > strings.Index(body, `name="prompt"`) {
		t.Error("the caller's field order was not honoured")
	}
	if strings.Index(body, `name="alpha"`) > strings.Index(body, `name="zeta"`) {
		t.Error("the remaining fields were not sorted")
	}
	if strings.Index(body, `name="alpha"`) < strings.Index(body, `name="prompt"`) {
		t.Error("an unordered field was emitted before an ordered one")
	}
}

func TestFormRoundTrip(t *testing.T) {
	want := &Form{
		Values: map[string][]string{
			"model": {"whisper:1"},
			"tags":  {"a", "b"},
		},
		Files: []File{
			{Field: "file", Name: "clip.wav", MediaType: "audio/wav", Data: []byte("RIFFdata")},
			{Field: "mask", Name: "m.png", MediaType: "image/png", Data: []byte("MASK")},
		},
	}
	body, err := want.Encode("dorangBOUNDARY", []string{"model"})
	if err != nil {
		t.Fatal(err)
	}
	_, params, err := mime.ParseMediaType("multipart/form-data; boundary=dorangBOUNDARY")
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	got := &Form{Values: map[string][]string{}}
	for {
		part, err := mr.NextPart()
		if err != nil {
			break
		}
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(part)
		if fn := part.FileName(); fn != "" {
			got.Files = append(got.Files, File{
				Field: part.FormName(), Name: fn,
				MediaType: part.Header.Get("Content-Type"), Data: buf.Bytes(),
			})
		} else {
			got.Values[part.FormName()] = append(got.Values[part.FormName()], buf.String())
		}
		_ = part.Close()
	}

	// The model name is opaque and carries a ':' — nothing splits it (§2.1).
	if got.Get("model") != "whisper:1" {
		t.Errorf("model %q", got.Get("model"))
	}
	if len(got.All("tags")) != 2 {
		t.Errorf("tags %v", got.Values["tags"])
	}
	if len(got.Files) != 2 {
		t.Fatalf("files %d", len(got.Files))
	}
	if f := got.File("file"); f == nil || string(f.Data) != "RIFFdata" || f.MediaType != "audio/wav" {
		t.Errorf("file %+v", f)
	}
	if f := got.File("mask"); f == nil || string(f.Data) != "MASK" {
		t.Errorf("mask %+v", f)
	}
}

// TestFormEscapesFilenames: a filename with a quote in it must not break out of
// the Content-Disposition header. mime/multipart's own escaper is unexported,
// so this package carries a copy — and a copy needs a test.
func TestFormEscapesFilenames(t *testing.T) {
	f := &Form{Files: []File{{Field: `f"ield`, Name: `a".wav`, Data: []byte("x")}}}
	body, err := f.Encode("B", nil)
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(bytes.NewReader(body), "B")
	part, err := mr.NextPart()
	if err != nil {
		t.Fatalf("the escaped header did not parse back: %v\n%s", err, body)
	}
	if part.FormName() != `f"ield` || part.FileName() != `a".wav` {
		t.Errorf("field %q file %q", part.FormName(), part.FileName())
	}
}
