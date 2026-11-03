package safejson

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ziozzang/dorang/internal/parseguard"
)

type customDecoder struct{ called bool }

func (c *customDecoder) UnmarshalJSON([]byte) error { c.called = true; return nil }

func TestDepthCheckedBeforeCustomDecoder(t *testing.T) {
	b := []byte(strings.Repeat("[", 129) + "0" + strings.Repeat("]", 129))
	for _, decode := range []func(any) error{
		func(v any) error { return Unmarshal(b, v) },
		func(v any) error { return NewDecoder(strings.NewReader(string(b))).Decode(v) },
	} {
		v := &customDecoder{}
		if err := decode(v); !errors.Is(err, parseguard.ErrDepth) || v.called {
			t.Fatalf("err=%v callback=%v", err, v.called)
		}
	}
	if Valid(b) {
		t.Fatal("deep document passed direct-decoder validity gate")
	}
}

func TestDecoderPreservesNumbersAndStrictFields(t *testing.T) {
	d := NewDecoder(strings.NewReader(`{"n":9007199254740993} {"n":2}`))
	d.UseNumber()
	var v map[string]any
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	if n, ok := v["n"].(Number); !ok || n.String() != "9007199254740993" {
		t.Fatalf("%T %v", v["n"], v["n"])
	}
	if !d.More() {
		t.Fatal("missing trailing document")
	}
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	if err := d.Decode(&v); err != io.EOF {
		t.Fatal(err)
	}
	d = NewDecoder(strings.NewReader(`{"unknown":1}`))
	d.DisallowUnknownFields()
	if err := d.Decode(&struct{}{}); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestTokenAndDecodeShareDepthBudget(t *testing.T) {
	d := NewDecoder(strings.NewReader(strings.Repeat("[", 129) + "0" + strings.Repeat("]", 129)))
	if _, err := d.Token(); err != nil {
		t.Fatal(err)
	}
	var v any
	if err := d.Decode(&v); !errors.Is(err, parseguard.ErrDepth) {
		t.Fatal(err)
	}
	d = NewDecoder(strings.NewReader(strings.Repeat("[", 129)))
	for i := 0; i < 128; i++ {
		if _, err := d.Token(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Token(); !errors.Is(err, parseguard.ErrDepth) {
		t.Fatal(err)
	}
}
