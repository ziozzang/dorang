package wirejson

import (
	"bytes"
	"encoding/json"
	"testing"
	"unicode/utf8"
)

func TestMalformedUTF8ReplacementMatchesSupportedToolchain(t *testing.T) {
	for _, s := range []string{"\xff", "a\xc0\xafz", "\xed\xa0\x80", "\xf4\x90\x80\x80", "�\xff�"} {
		want, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		got := AppendString(nil, s)
		if !bytes.Equal(got, want) || !utf8.Valid(got) {
			t.Fatalf("input %x: got %s, want %s", s, got, want)
		}
	}
}
