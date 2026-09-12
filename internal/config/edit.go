package config

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Surgical config editing.
//
// These functions edit the YAML node tree in place rather than re-serializing a
// decoded [Config]. The difference is the operator's file: a struct round-trip
// drops comments, reorders keys, expands every default to its written form, and
// re-renders secret references — turning a hand-tuned config into a machine
// dump on the first edit. A node-tree edit changes only the bytes it must and
// leaves everything else, comments included, exactly as written.
//
// None of these validate the result. Editing text cannot know whether the
// result routes; the caller must [Load] the returned bytes and rebuild before
// trusting them (see the app's config writer), because a file that parses is
// not a file that serves.

// documentRoot returns the mapping at the root of a parsed document, or nil.
func documentRoot(doc *yaml.Node) *yaml.Node {
	n := doc
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		n = n.Content[0]
	}
	return n
}

// mapValue returns the value node for a key in a mapping, or nil. A mapping's
// Content alternates key, value, key, value.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// scalarEquals reports whether a mapping's key holds the given scalar value.
func scalarEquals(m *yaml.Node, key, want string) bool {
	v := mapValue(m, key)
	return v != nil && v.Kind == yaml.ScalarNode && v.Value == want
}

// setScalar sets (or inserts) a scalar key on a mapping. A new key is appended,
// which keeps existing keys and their comments where they are.
func setScalar(m *yaml.Node, key, value, tag string) {
	if v := mapValue(m, key); v != nil {
		v.Kind = yaml.ScalarNode
		v.Tag = tag
		v.Value = value
		v.Style = 0
		return
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value},
	)
}

// findDeployment locates a (group, provider, upstream) deployment mapping in a
// models sequence. A model may carry the same triple more than once — the same
// provider and upstream behind different credentials is a real pattern, and the
// router disambiguates such duplicates with a `#n` suffix on the deployment id
// — so occurrence selects which one (0-based, matching that suffix: 0 is the
// bare id, 1 is `#1`, and so on). It is scoped to the named model, exactly as
// the id's disambiguation is.
func findDeployment(models *yaml.Node, group, provider, upstream string, occurrence int) (*yaml.Node, error) {
	if models == nil || models.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("config: models is not a sequence")
	}
	match := 0
	for _, model := range models.Content {
		if model.Kind != yaml.MappingNode || !scalarEquals(model, "name", group) {
			continue
		}
		deps := mapValue(model, "deployments")
		if deps == nil || deps.Kind != yaml.SequenceNode {
			continue
		}
		for _, d := range deps.Content {
			if d.Kind != yaml.MappingNode {
				continue
			}
			if scalarEquals(d, "provider", provider) && scalarEquals(d, "upstream_model", upstream) {
				if match == occurrence {
					return d, nil
				}
				match++
			}
		}
	}
	return nil, fmt.Errorf("config: no deployment %s/%s/%s occurrence %d (found %d)",
		group, provider, upstream, occurrence, match)
}

// encode re-renders an edited document at the two-space indent the config uses.
func encode(doc *yaml.Node) ([]byte, error) {
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("config: encode: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("config: encode close: %w", err)
	}
	return out.Bytes(), nil
}

// SetDeploymentEnabled returns the config YAML with the named deployment's
// `enabled` field set to the given value, editing the node tree so comments and
// every untouched value are preserved. occurrence selects among duplicate
// triples in the model (see [findDeployment]). It does not validate the result.
func SetDeploymentEnabled(data []byte, group, provider, upstream string, occurrence int, enabled bool) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	root := documentRoot(&doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config: top level is not a mapping")
	}
	dep, err := findDeployment(mapValue(root, "models"), group, provider, upstream, occurrence)
	if err != nil {
		return nil, err
	}
	setScalar(dep, "enabled", boolText(enabled), "!!bool")
	return encode(&doc)
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
