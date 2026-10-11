package registry

import (
	"bytes"
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"

	"sleuth/internal/flow"
)

// ToJSON returns an OpenAPI document as JSON. A document that already is JSON is returned as
// it is; anything else is read as YAML (most published specs are) and converted:
//
//   - every mapping key becomes a string, so an unquoted status code (`200:`) is "200";
//   - a number keeps its exact text (json.Number), as everywhere else in Sleuth;
//   - a date or timestamp (`example: 2024-01-01`) stays the text it was, not a time value;
//   - anchors, aliases and merge keys (`<<`) are resolved.
func ToJSON(b []byte) ([]byte, error) {
	if trimmed := bytes.TrimSpace(b); len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		return b, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("neither JSON nor YAML: %w", err)
	}
	v, err := yamlValue(&doc, 0)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

const maxYAMLDepth = 256

func yamlValue(n *yaml.Node, depth int) (any, error) {
	if depth > maxYAMLDepth {
		return nil, fmt.Errorf("YAML nested more than %d levels (a cyclic anchor?)", maxYAMLDepth)
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return yamlValue(n.Content[0], depth+1)
	case yaml.AliasNode:
		return yamlValue(n.Alias, depth+1)
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := yamlValue(c, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.MappingNode:
		out := map[string]any{}
		var merges []*yaml.Node
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, val := n.Content[i], n.Content[i+1]
			if k.Tag == "!!merge" {
				merges = append(merges, val)
				continue
			}
			v, err := yamlValue(val, depth+1)
			if err != nil {
				return nil, err
			}
			out[k.Value] = v
		}
		for _, m := range merges { // explicit keys win over merged ones
			mv, err := yamlValue(m, depth+1)
			if err != nil {
				return nil, err
			}
			sources := []any{mv}
			if list, ok := mv.([]any); ok {
				sources = list
			}
			for _, src := range sources {
				if sm, ok := src.(map[string]any); ok {
					for k, v := range sm {
						if _, set := out[k]; !set {
							out[k] = v
						}
					}
				}
			}
		}
		return out, nil
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!null":
			return nil, nil
		case "!!bool":
			var b bool
			if err := n.Decode(&b); err != nil {
				return nil, err
			}
			return b, nil
		case "!!int", "!!float":
			if flow.IsNumberLiteral(n.Value) {
				return json.Number(n.Value), nil
			}
			var v any // 0x1F, 1_000, .inf: let YAML read it
			if err := n.Decode(&v); err != nil {
				return nil, err
			}
			return v, nil
		default: // !!str, !!timestamp, !!binary, custom tags: the text as written
			return n.Value, nil
		}
	}
	return nil, fmt.Errorf("unexpected YAML node kind %d", n.Kind)
}
