// Package wiretest holds the comparison every adapter's pass-through test
// needs.
//
// It exists for the reason [wirejson]'s doc comment gives about the Extra
// mechanism: a property that each adapter asserts its own way is a property
// that holds in one adapter and quietly does not in the next. The property here
// is "the client received what the upstream sent", and it has to mean the same
// thing in internal/wire/openai, internal/wire/anthropic and
// internal/wire/rerank or the pass-through guarantee is three different
// guarantees.
//
// It does not import testing. Callers report failures themselves, so nothing
// here constrains how a test is structured or drags the testing package into a
// non-test build.
//
// [wirejson]: github.com/ziozzang/dorang/internal/wire/wirejson
package wiretest

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// Leaves flattens a JSON document to path -> literal value.
//
// Paths are dotted for object members and bracketed for array elements:
// `choices[0].message.role`. An empty object or array is itself a leaf, so
// `"choices":[]` is compared rather than vanishing.
//
// Numbers keep their SOURCE TEXT, through json.Number. "40.0" and "40" are
// different leaves here, and that is the point: decoding a number to float64
// and re-encoding it is precisely the operation that rewrites a literal on the
// wire, and a comparison that normalizes both sides the same way cannot see it
// happen.
func Leaves(b []byte) (map[string]string, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	out := make(map[string]string)
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			if len(x) == 0 {
				out[path] = "{}"
				return
			}
			for k, sub := range x {
				p := k
				if path != "" {
					p = path + "." + k
				}
				walk(p, sub)
			}
		case []any:
			if len(x) == 0 {
				out[path] = "[]"
				return
			}
			for i, sub := range x {
				walk(path+"["+strconv.Itoa(i)+"]", sub)
			}
		case json.Number:
			out[path] = x.String()
		case string:
			out[path] = strconv.Quote(x)
		case bool:
			out[path] = strconv.FormatBool(x)
		default: // nil
			out[path] = "null"
		}
	}
	walk("", v)
	return out, nil
}

// Subtree narrows a leaf map to one prefix, keeping the FULL paths so that two
// subtrees taken from differently-shaped documents still compare by name.
func Subtree(leaves map[string]string, prefix string) map[string]string {
	out := make(map[string]string)
	for k, v := range leaves {
		if k == prefix || strings.HasPrefix(k, prefix+".") || strings.HasPrefix(k, prefix+"[") {
			out[k] = v
		}
	}
	return out
}

// Diff reports the leaves of want that got is missing or disagrees about, as
// sorted lines, or "" when nothing was lost.
//
// Leaves present only in got are NOT a failure. Adding a field is not losing
// one, and the encoders here legitimately add — an object discriminator, a
// synthesized finish_reason — where the upstream sent nothing.
func Diff(want, got map[string]string) string {
	var lines []string
	for k, v := range want {
		g, ok := got[k]
		if !ok {
			lines = append(lines, "  DROPPED  "+k+" = "+v)
			continue
		}
		if g != v {
			lines = append(lines, "  CHANGED  "+k+": want "+v+", got "+g)
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
