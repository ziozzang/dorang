// Package safejson applies the product's nesting limit before encoding/json
// invokes custom or recursive decoders. Serialization is unchanged.
package safejson

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/ziozzang/dorang/internal/parseguard"
)

type RawMessage = json.RawMessage
type Number = json.Number
type Delim = json.Delim
type Marshaler = json.Marshaler
type Unmarshaler = json.Unmarshaler
type MarshalerError = json.MarshalerError
type UnsupportedValueError = json.UnsupportedValueError

func Marshal(v any) ([]byte, error) { return json.Marshal(v) }
func MarshalIndent(v any, prefix, indent string) ([]byte, error) {
	return json.MarshalIndent(v, prefix, indent)
}
func NewEncoder(w io.Writer) *json.Encoder        { return json.NewEncoder(w) }
func Compact(dst *bytes.Buffer, src []byte) error { return json.Compact(dst, src) }
func Valid(b []byte) bool                         { return parseguard.JSONDepth(b) == nil && json.Valid(b) }
func Unmarshal(b []byte, v any) error {
	if err := parseguard.JSONDepth(b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Decode checks the complete value before materializing its destination.
// Readers still need a byte/time limit at their network or file boundary.
type Decoder struct {
	*json.Decoder
	useNumber, knownFields bool
	depth                  int
}

func NewDecoder(r io.Reader) *Decoder     { return &Decoder{Decoder: json.NewDecoder(r)} }
func (d *Decoder) UseNumber()             { d.useNumber = true; d.Decoder.UseNumber() }
func (d *Decoder) DisallowUnknownFields() { d.knownFields = true; d.Decoder.DisallowUnknownFields() }
func (d *Decoder) Decode(v any) error {
	var raw json.RawMessage
	if err := d.Decoder.Decode(&raw); err != nil {
		return err
	}
	if err := parseguard.JSONDepthWithin(raw, parseguard.MaxDepth-d.depth); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if d.useNumber {
		dec.UseNumber()
	}
	if d.knownFields {
		dec.DisallowUnknownFields()
	}
	return dec.Decode(v)
}

func (d *Decoder) Token() (json.Token, error) {
	tok, err := d.Decoder.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); ok {
		switch delim {
		case '{', '[':
			d.depth++
		case '}', ']':
			d.depth--
		}
		if d.depth > parseguard.MaxDepth {
			return nil, parseguard.ErrDepth
		}
	}
	return tok, nil
}
