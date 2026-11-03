// Package safeyaml bounds YAML bytes, nesting and alias expansion before
// decoding application values, including callers which decode into yaml.Node.
package safeyaml

import (
	"bytes"
	"errors"
	"io"

	"go.yaml.in/yaml/v3"

	"github.com/ziozzang/dorang/internal/parseguard"
)

type Node = yaml.Node
type TypeError = yaml.TypeError
type Marshaler = yaml.Marshaler
type Unmarshaler = yaml.Unmarshaler

const (
	DocumentNode = yaml.DocumentNode
	MappingNode  = yaml.MappingNode
	SequenceNode = yaml.SequenceNode
	ScalarNode   = yaml.ScalarNode
)
const maxNodes = 200000
const maxSeparators = 100000

// The library checks duplicate mapping keys pairwise. Bound map width so
// that a small but very wide document cannot consume quadratic CPU time.
const maxMappingPairs = 1024
const maxExpansionRatio = 100

var ErrExpansion = errors.New("YAML exceeds structural or alias expansion limit")

func Marshal(v any) ([]byte, error)        { return yaml.Marshal(v) }
func NewEncoder(w io.Writer) *yaml.Encoder { return yaml.NewEncoder(w) }
func Unmarshal(b []byte, v any) error {
	if err := validate(b); err != nil {
		return err
	}
	return yaml.Unmarshal(b, v)
}

func validate(b []byte) error {
	if int64(len(b)) > parseguard.MaxFileBytes {
		return parseguard.ErrSize
	}
	// Bound the syntax tree's cost before asking the parser to allocate it.
	// This intentionally counts punctuation inside scalars/comments too: the
	// preflight is conservative, not another implementation of YAML syntax.
	separators := 0
	for _, c := range b {
		switch c {
		case '\n', '[', ']', '{', '}', ',', ':', '-', '?':
			separators++
			if separators > maxSeparators {
				return ErrExpansion
			}
		}
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	remaining := maxNodes
	expandedBytes := min(2*parseguard.MaxFileBytes, max(int64(len(b)), 1024)*maxExpansionRatio)
	var walk func(*yaml.Node, int) error
	walk = func(n *yaml.Node, depth int) error {
		if n == nil {
			return nil
		}
		if depth > parseguard.MaxDepth {
			return parseguard.ErrDepth
		}
		if n.Kind == yaml.MappingNode && len(n.Content)/2 > maxMappingPairs {
			return ErrExpansion
		}
		remaining--
		expandedBytes -= int64(len(n.Value))
		if remaining < 0 || expandedBytes < 0 {
			return ErrExpansion
		}
		if n.Kind == yaml.AliasNode {
			return walk(n.Alias, depth+1)
		}
		for _, child := range n.Content {
			if err := walk(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := walk(&n, 0); err != nil {
			return err
		}
	}
}

type Decoder struct {
	r     io.Reader
	dec   *yaml.Decoder
	known bool
	err   error
}

func NewDecoder(r io.Reader) *Decoder { return &Decoder{r: r} }
func (d *Decoder) KnownFields(v bool) {
	d.known = v
	if d.dec != nil {
		d.dec.KnownFields(v)
	}
}
func (d *Decoder) Decode(v any) error {
	if d.dec == nil && d.err == nil {
		var b []byte
		b, d.err = parseguard.Read(d.r, parseguard.MaxFileBytes)
		if d.err == nil {
			d.err = validate(b)
		}
		if d.err == nil {
			d.dec = yaml.NewDecoder(bytes.NewReader(b))
			d.dec.KnownFields(d.known)
		}
	}
	if d.err != nil {
		return d.err
	}
	return d.dec.Decode(v)
}
