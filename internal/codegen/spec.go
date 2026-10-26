package codegen

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Spec gives typed access to a prepared OpenAPI document.
type Spec struct {
	Doc map[string]any
}

// Operation returns the operation object for method and path.
func (s *Spec) Operation(method, path string) (map[string]any, bool) {
	item, ok := s.pathItem(path)
	if !ok {
		return nil, false
	}
	op, ok := item[strings.ToLower(method)].(map[string]any)
	return op, ok
}

func (s *Spec) pathItem(path string) (map[string]any, bool) {
	paths, _ := s.Doc["paths"].(map[string]any)
	item, ok := paths[path].(map[string]any)
	return item, ok
}

// Paths returns all paths, sorted.
func (s *Spec) Paths() []string {
	paths, _ := s.Doc["paths"].(map[string]any)
	return sortedKeys(paths)
}

// Methods returns the HTTP methods defined on a path, sorted.
func (s *Spec) Methods(path string) []string {
	item, _ := s.pathItem(path)
	var out []string
	for _, k := range sortedKeys(item) {
		if isHTTPMethod(k) {
			out = append(out, k)
		}
	}
	return out
}

// Param is an operation parameter.
type Param struct {
	Name     string
	In       string
	Required bool
	Schema   map[string]any
}

// Params returns path-item and operation parameters merged (operation-level
// parameters override path-level ones with the same name and location).
func (s *Spec) Params(method, path string) []Param {
	item, _ := s.pathItem(path)
	op, _ := s.Operation(method, path)
	byKey := map[string]Param{}
	var order []string
	add := func(list any) {
		l, _ := list.([]any)
		for _, x := range l {
			m, ok := x.(map[string]any)
			if !ok {
				continue
			}
			p := Param{}
			p.Name, _ = m["name"].(string)
			p.In, _ = m["in"].(string)
			p.Required, _ = m["required"].(bool)
			p.Schema, _ = m["schema"].(map[string]any)
			key := p.In + ":" + p.Name
			if _, seen := byKey[key]; !seen {
				order = append(order, key)
			}
			byKey[key] = p
		}
	}
	add(item["parameters"])
	add(op["parameters"])
	out := make([]Param, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	return out
}

// RequestSchema returns the JSON request body schema of an operation.
func (s *Spec) RequestSchema(method, path string) map[string]any {
	op, ok := s.Operation(method, path)
	if !ok {
		return nil
	}
	rb, _ := op["requestBody"].(map[string]any)
	return contentSchema(rb)
}

// ResponseSchema returns the JSON schema of an operation's success response,
// choosing 200, then 201, then the first other 2xx with a schema (the same
// choice tfplugingen-openapi makes).
func (s *Spec) ResponseSchema(method, path string) map[string]any {
	op, ok := s.Operation(method, path)
	if !ok {
		return nil
	}
	resps, _ := op["responses"].(map[string]any)
	for _, code := range []string{"200", "201"} {
		if r, ok := resps[code].(map[string]any); ok {
			if sc := contentSchema(r); sc != nil {
				return sc
			}
		}
	}
	for _, code := range sortedKeys(resps) {
		if strings.HasPrefix(code, "2") {
			if r, ok := resps[code].(map[string]any); ok {
				if sc := contentSchema(r); sc != nil {
					return sc
				}
			}
		}
	}
	return nil
}

func contentSchema(holder map[string]any) map[string]any {
	content, _ := holder["content"].(map[string]any)
	if media, ok := content["application/json"].(map[string]any); ok {
		if sc, ok := media["schema"].(map[string]any); ok {
			return sc
		}
	}
	for _, ct := range sortedKeys(content) {
		if media, ok := content[ct].(map[string]any); ok {
			if sc, ok := media["schema"].(map[string]any); ok {
				return sc
			}
		}
	}
	return nil
}

func properties(schema map[string]any) map[string]any {
	p, _ := schema["properties"].(map[string]any)
	return p
}

// shape renders the structural type of a schema (type, and for strings the
// pattern/enum/format) so request and response representations of the same
// field can be compared.
func shape(s map[string]any) string {
	if s == nil {
		return "<none>"
	}
	var sb strings.Builder
	writeShape(&sb, s)
	return sb.String()
}

func writeShape(sb *strings.Builder, s map[string]any) {
	t := fmt.Sprint(s["type"])
	sb.WriteString(t)
	switch t {
	case "object":
		if ap, ok := s["additionalProperties"].(map[string]any); ok {
			sb.WriteString("<")
			writeShape(sb, ap)
			sb.WriteString(">")
		}
		props := properties(s)
		sb.WriteString("{")
		for _, k := range sortedKeys(props) {
			sb.WriteString(k)
			sb.WriteString(":")
			if ps, ok := props[k].(map[string]any); ok {
				writeShape(sb, ps)
			}
			sb.WriteString(",")
		}
		sb.WriteString("}")
	case "array":
		sb.WriteString("[")
		if is, ok := s["items"].(map[string]any); ok {
			writeShape(sb, is)
		}
		sb.WriteString("]")
	default:
		for _, k := range []string{"format", "pattern", "enum"} {
			if v, ok := s[k]; ok {
				fmt.Fprintf(sb, "|%s=%v", k, v)
			}
		}
	}
}

var validIdentifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

var (
	unsupportedIdentChars = regexp.MustCompile(`[^a-zA-Z0-9_]+`)
	leadingNumbers        = regexp.MustCompile(`^[0-9]*`)
	lowerToUpper          = regexp.MustCompile(`([a-z])[A-Z]`)
)

// TerraformIdentifier converts an OpenAPI property name to the attribute
// name tfplugingen-openapi gives it (a copy of its util.TerraformIdentifier).
func TerraformIdentifier(original string) string {
	if original == "" || validIdentifier.MatchString(original) && !strings.HasPrefix(original, "0") {
		return original
	}
	s := unsupportedIdentChars.ReplaceAllString(original, "")
	s = leadingNumbers.ReplaceAllString(s, "")
	s = lowerToUpper.ReplaceAllStringFunc(s, func(m string) string {
		r, size := utf8.DecodeRuneInString(m)
		return string(r) + "_" + strings.ToLower(m[size:])
	})
	return strings.ToLower(s)
}

// collectRenames records every property name in a schema tree whose
// Terraform attribute name differs from its JSON name.
func collectRenames(s map[string]any, into map[string]string) error {
	if s == nil {
		return nil
	}
	props := properties(s)
	for _, name := range sortedKeys(props) {
		tf := TerraformIdentifier(name)
		if tf != name {
			if prev, ok := into[tf]; ok && prev != name {
				return fmt.Errorf("JSON properties %q and %q both map to attribute %q", prev, name, tf)
			}
			into[tf] = name
		}
		if ps, ok := props[name].(map[string]any); ok {
			if err := collectRenames(ps, into); err != nil {
				return err
			}
		}
	}
	if is, ok := s["items"].(map[string]any); ok {
		if err := collectRenames(is, into); err != nil {
			return err
		}
	}
	if ap, ok := s["additionalProperties"].(map[string]any); ok {
		if err := collectRenames(ap, into); err != nil {
			return err
		}
	}
	return nil
}

// singular turns a plural path segment into a resource name.
func singular(seg string) string {
	switch {
	case strings.HasSuffix(seg, "ies"):
		return strings.TrimSuffix(seg, "ies") + "y"
	case strings.HasSuffix(seg, "sses"), strings.HasSuffix(seg, "xes"), strings.HasSuffix(seg, "ches"), strings.HasSuffix(seg, "shes"):
		return strings.TrimSuffix(seg, "es")
	case strings.HasSuffix(seg, "ss"):
		return seg
	case strings.HasSuffix(seg, "s"):
		return strings.TrimSuffix(seg, "s")
	}
	return seg
}

func sortedStrings(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
