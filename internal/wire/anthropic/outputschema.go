package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
)

// maxSchemaDepth bounds recursion while resolving references.
//
// A JSON Schema is caller-supplied and `$ref` can point at an ancestor, so
// inlining without a bound is a stack overflow a request can ask for. The
// number is a depth no hand-written schema reaches and every cyclic one does.
const maxSchemaDepth = 64

// errSchemaCycle reports a `$ref` that resolves through itself.
var errSchemaCycle = errors.New("schema references itself")

// anthropicOutputSchema renders a caller's JSON Schema in the shape Anthropic's
// `output_format` accepts.
//
// Two things have to happen and neither is cosmetic.
//
// # References are inlined
//
// Anthropic does not resolve `$ref`, so a schema that factors shared shapes
// into `$defs` — which is what every code generator emits — arrives with
// dangling pointers and is rejected. The definitions are inlined at each use
// and the `$defs` block is removed.
//
// A self-referential schema (a tree node whose children are the same node)
// cannot be inlined at all, and is reported rather than truncated: a schema
// silently cut off at depth 64 is a different contract from the one the caller
// wrote, and they would have no way to know.
//
// # Unsupported keywords are removed
//
// The vocabulary Anthropic enforces is narrower than JSON Schema's. Sending a
// keyword it does not know is a 400 for the whole request, so a keyword that
// only NARROWS an already-expressible shape is dropped and the request is
// served.
//
// This is the one place in this file where something is taken away silently,
// and it is deliberate: the alternative is refusing a request over a
// `description` field. The line is that a removed keyword must not change which
// documents VALIDATE — only how they are described. Anything that changes the
// accepted set (`enum`, `required`, `type`) is kept, and a schema that cannot
// be rendered without changing it is returned as an error for the caller to be
// told about.
func anthropicOutputSchema(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, errors.New("no schema")
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return nil, errors.New("schema is not an object")
	}

	defs := map[string]any{}
	for _, key := range []string{"$defs", "definitions"} {
		if d, ok := root[key].(map[string]any); ok {
			for k, v := range d {
				defs[key+"/"+k] = v
			}
		}
		delete(root, key)
	}

	out, err := resolve(root, defs, 0)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// unsupported names the keywords removed on the way through.
//
// Every one of them DESCRIBES rather than CONSTRAINS, so removing it cannot
// change which documents the schema accepts. `title` and `description` are
// documentation; `$comment` and `examples` likewise; `default` supplies a value
// for an absent member and Anthropic has no notion of it; `$schema` and `$id`
// are document identity.
var unsupported = map[string]bool{
	"title": true, "description": true, "$comment": true,
	"examples": true, "default": true, "$schema": true, "$id": true,
	"readOnly": true, "writeOnly": true, "deprecated": true,
}

// resolve inlines every `$ref` and strips every unsupported keyword.
func resolve(node any, defs map[string]any, depth int) (any, error) {
	if depth > maxSchemaDepth {
		return nil, errSchemaCycle
	}
	switch n := node.(type) {
	case map[string]any:
		if ref, ok := n["$ref"].(string); ok {
			target, err := lookup(ref, defs)
			if err != nil {
				return nil, err
			}
			// The sibling keys of a `$ref` are merged over the target, which is
			// what 2020-12 says they mean and what a generator emitting
			// `{"$ref": ..., "description": ...}` intends.
			merged, err := resolve(target, defs, depth+1)
			if err != nil {
				return nil, err
			}
			mm, ok := merged.(map[string]any)
			if !ok {
				return merged, nil
			}
			for k, v := range n {
				if k == "$ref" || unsupported[k] {
					continue
				}
				rv, err := resolve(v, defs, depth+1)
				if err != nil {
					return nil, err
				}
				mm[k] = rv
			}
			return mm, nil
		}
		out := make(map[string]any, len(n))
		for k, v := range n {
			if unsupported[k] {
				continue
			}
			rv, err := resolve(v, defs, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(n))
		for _, v := range n {
			rv, err := resolve(v, defs, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, rv)
		}
		return out, nil
	default:
		return node, nil
	}
}

// lookup resolves a local `$ref` against the collected definitions.
//
// Only local references are resolvable. A `$ref` at an http(s) URL would mean
// this gateway fetching a document named by the caller, on the request path,
// which is a server-side request forgery with extra steps.
func lookup(ref string, defs map[string]any) (any, error) {
	const prefix = "#/"
	if len(ref) < len(prefix) || ref[:len(prefix)] != prefix {
		return nil, fmt.Errorf("unresolvable $ref %q: only local #/$defs references are inlined, "+
			"and a remote one would make this gateway fetch a document the caller named", ref)
	}
	key := ref[len(prefix):]
	if v, ok := defs[key]; ok {
		return v, nil
	}
	return nil, fmt.Errorf("$ref %q resolves to nothing in this schema", ref)
}
