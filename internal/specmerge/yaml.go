package specmerge

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// YAMLToJSON parses one YAML document into plain JSON-compatible values:
// map[string]any, []any, string, json.Number, bool and nil. Mapping keys are
// always strings (so `200:` and `'200':` are the same key), and numbers keep
// their textual form where it is valid JSON, so the output does not change
// between runs or Go versions.
func YAMLToJSON(data []byte) (any, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 {
		return nil, fmt.Errorf("empty YAML document")
	}
	return convertNode(&doc)
}

func convertNode(n *yaml.Node) (any, error) {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) != 1 {
			return nil, fmt.Errorf("line %d: expected exactly one document", n.Line)
		}
		return convertNode(n.Content[0])
	case yaml.AliasNode:
		return convertNode(n.Alias)
	case yaml.MappingNode:
		out := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if k.Kind == yaml.ScalarNode && k.Tag == "!!merge" {
				return nil, fmt.Errorf("line %d: YAML merge keys are not supported", k.Line)
			}
			if k.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("line %d: non-scalar mapping key", k.Line)
			}
			if _, dup := out[k.Value]; dup {
				return nil, fmt.Errorf("line %d: duplicate key %q", k.Line, k.Value)
			}
			cv, err := convertNode(v)
			if err != nil {
				return nil, err
			}
			out[k.Value] = cv
		}
		return out, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			cv, err := convertNode(c)
			if err != nil {
				return nil, err
			}
			out = append(out, cv)
		}
		return out, nil
	case yaml.ScalarNode:
		return convertScalar(n)
	default:
		return nil, fmt.Errorf("line %d: unsupported YAML node kind %v", n.Line, n.Kind)
	}
}

func convertScalar(n *yaml.Node) (any, error) {
	switch n.ShortTag() {
	case "!!null":
		return nil, nil
	case "!!bool":
		var b bool
		if err := n.Decode(&b); err != nil {
			return nil, fmt.Errorf("line %d: %w", n.Line, err)
		}
		return b, nil
	case "!!int":
		s := strings.ReplaceAll(n.Value, "_", "")
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return json.Number(strconv.FormatInt(i, 10)), nil
		}
		var i int64
		if err := n.Decode(&i); err != nil {
			return nil, fmt.Errorf("line %d: %w", n.Line, err)
		}
		return json.Number(strconv.FormatInt(i, 10)), nil
	case "!!float":
		var f float64
		if err := n.Decode(&f); err != nil {
			return nil, fmt.Errorf("line %d: %w", n.Line, err)
		}
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, fmt.Errorf("line %d: %q cannot be represented in JSON", n.Line, n.Value)
		}
		if json.Valid([]byte(n.Value)) {
			return json.Number(n.Value), nil
		}
		return json.Number(strconv.FormatFloat(f, 'g', -1, 64)), nil
	default:
		// !!str, !!timestamp, !!binary and custom tags all stay strings.
		return n.Value, nil
	}
}
