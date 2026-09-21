package config

import (
	"fmt"
	"gopkg.in/yaml.v3"
	"sort"
)

// SetupEdit patches only named fields, preserving unowned configuration.
// Values must contain references, never secret contents. Validation belongs to
// the transactional caller, which has the deployed secret environment.
type SetupEdit struct {
	Parameters         map[string]any
	Section, IDKey, ID string
	Fields             map[string]any
	Remove             []string
	Create             bool
	Model              string
}

func EditSetup(data []byte, edit SetupEdit) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	root := documentRoot(&doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("configuration must be a mapping")
	}
	var seq *yaml.Node
	if edit.Section == "deployments" {
		models := ensureSequence(root, "models")
		var group *yaml.Node
		for _, m := range models.Content {
			if scalarEquals(m, "name", edit.Model) {
				group = m
				break
			}
		}
		if group == nil {
			group = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setScalar(group, "name", edit.Model, "!!str")
			models.Content = append(models.Content, group)
		}
		seq = ensureSequence(group, "deployments")
	} else if edit.Section == "providers" || edit.Section == "credentials" {
		seq = ensureSequence(root, edit.Section)
	} else {
		return nil, fmt.Errorf("unsupported setup section")
	}
	var target *yaml.Node
	if edit.Section != "deployments" {
		for _, v := range seq.Content {
			if scalarEquals(v, edit.IDKey, edit.ID) {
				target = v
				break
			}
		}
	}
	if edit.Create && target != nil {
		return nil, fmt.Errorf("item already exists; open its edit form")
	}
	if !edit.Create && target == nil {
		return nil, fmt.Errorf("item no longer exists")
	}
	if target == nil {
		target = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		seq.Content = append(seq.Content, target)
		if edit.IDKey != "" {
			setScalar(target, edit.IDKey, edit.ID, "!!str")
		}
	}
	for _, key := range edit.Remove {
		for i := 0; i+1 < len(target.Content); i += 2 {
			if target.Content[i].Value == key {
				target.Content = append(target.Content[:i], target.Content[i+2:]...)
				break
			}
		}
	}
	// Sorting makes patches reproducible without rearranging existing fields.
	keys := sortedSetupKeys(edit.Fields)
	for _, key := range keys {
		var n yaml.Node
		if err := n.Encode(edit.Fields[key]); err != nil {
			return nil, err
		}
		if old := mapValue(target, key); old != nil {
			head, line, foot := old.HeadComment, old.LineComment, old.FootComment
			*old = n
			old.HeadComment = head
			old.LineComment = line
			old.FootComment = foot
		} else {
			target.Content = append(target.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &n)
		}
	}

	if len(edit.Parameters) > 0 {
		params := mapValue(target, "params")
		if params == nil {
			params = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			target.Content = append(target.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "params"}, params)
		}
		for _, key := range sortedSetupKeys(edit.Parameters) {
			setScalar(params, key, fmt.Sprint(edit.Parameters[key]), "!!str")
		}
	}
	return encode(&doc)
}
func ensureSequence(m *yaml.Node, key string) *yaml.Node {
	if v := mapValue(m, key); v != nil {
		return v
	}
	v := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	return v
}

func sortedSetupKeys(m map[string]any) []string {
	k := make([]string, 0, len(m))
	for key := range m {
		k = append(k, key)
	}
	sort.Strings(k)
	return k
}

// ReadSetup shares the file lock used by configuration writers.
func ReadSetup(path string) ([]byte, error) { return readLocked(path) }
