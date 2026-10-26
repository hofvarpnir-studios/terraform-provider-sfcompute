// Package codegen holds the SF Compute-specific steps of the code generation
// pipeline that surround HashiCorp's tfplugingen-openapi and
// tfplugingen-framework:
//
//  1. Prepare: rewrite OpenAPI 3.1 constructs that tfplugingen-openapi v0.3
//     cannot map (allOf composition, non-null unions, const, $ref siblings)
//     into plain equivalents.
//  2. Resolve: expand generator_config.yml by deriving resource and data
//     source entries from REST path conventions, and write the plain config
//     tfplugingen-openapi consumes.
//  3. Patch: adjust the provider code specification tfplugingen-openapi
//     produced (for example make `workspace` optional so it can default from
//     the provider configuration).
//  4. Registry: emit the Go table that binds each generated schema to its API
//     operations for the generic CRUD runtime in internal/runtime.
package codegen

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// Prepared is a preprocessed OpenAPI document.
type Prepared struct {
	Doc      map[string]any
	Warnings []string
}

// Prepare returns a copy of doc in which every schema reachable from an
// operation is self-contained and uses only constructs tfplugingen-openapi
// understands:
//
//   - `$ref`s to #/components/schemas are inlined (sibling keywords such as
//     description override the target's), so 3.1-style `$ref` + description
//     keeps its description.
//   - `allOf` is flattened into one object schema (properties merged,
//     required lists unioned).
//   - `oneOf`/`anyOf` whose only alternatives are `null` and one schema are
//     replaced by that schema; unions of several string schemas become a
//     plain string; unions of several object schemas become one object with
//     the union of their properties (required = properties required by every
//     alternative).
//   - `type: [T, "null"]` becomes `type: T`.
//   - `const: v` becomes `enum: [v]`, except that the constant `object`
//     discriminator property every SF Compute response carries is dropped.
//   - readOnly properties are removed from request bodies and writeOnly
//     properties from responses.
//   - `example`/`examples` are dropped, and `pattern`s that Go's RE2 engine
//     cannot compile are dropped (the generated validators would panic).
func Prepare(doc map[string]any) (*Prepared, error) {
	p := &preparer{
		schemas: map[string]any{},
		done:    map[string]map[string]any{},
		active:  map[string]bool{},
	}
	if comps, ok := doc["components"].(map[string]any); ok {
		if s, ok := comps["schemas"].(map[string]any); ok {
			p.schemas = s
		}
	}
	out := deepCopy(doc).(map[string]any)
	paths, _ := out["paths"].(map[string]any)
	for _, path := range sortedKeys(paths) {
		item, ok := paths[path].(map[string]any)
		if !ok {
			continue
		}
		for _, key := range sortedKeys(item) {
			switch key {
			case "parameters":
				params, err := p.params(item[key], path)
				if err != nil {
					return nil, err
				}
				item[key] = params
				continue
			}
			op, ok := item[key].(map[string]any)
			if !ok || !isHTTPMethod(key) {
				continue
			}
			where := strings.ToUpper(key) + " " + path
			if params, ok := op["parameters"]; ok {
				np, err := p.params(params, where)
				if err != nil {
					return nil, err
				}
				op["parameters"] = np
			}
			if rb, ok := op["requestBody"].(map[string]any); ok {
				if err := p.content(rb, where+" request", modeRequest); err != nil {
					return nil, err
				}
			}
			if resps, ok := op["responses"].(map[string]any); ok {
				for _, code := range sortedKeys(resps) {
					r, ok := resps[code].(map[string]any)
					if !ok {
						continue
					}
					if err := p.content(r, where+" "+code, modeResponse); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	// Component schemas have all been inlined where used; keep them (also
	// prepared) so the document stays valid for anyone reading it.
	if comps, ok := out["components"].(map[string]any); ok {
		if s, ok := comps["schemas"].(map[string]any); ok {
			ns := map[string]any{}
			for _, name := range sortedKeys(s) {
				v, err := p.ref("#/components/schemas/"+name, modeNeutral)
				if err != nil {
					return nil, err
				}
				ns[name] = v
			}
			comps["schemas"] = ns
		}
	}
	sort.Strings(p.warnings)
	return &Prepared{Doc: out, Warnings: dedupe(p.warnings)}, nil
}

type mode int

const (
	modeNeutral mode = iota
	modeRequest
	modeResponse
)

type preparer struct {
	schemas  map[string]any
	done     map[string]map[string]any // cache key (mode+ref) -> prepared schema
	active   map[string]bool
	warnings []string
}

func (p *preparer) warnf(format string, args ...any) {
	p.warnings = append(p.warnings, fmt.Sprintf(format, args...))
}

func (p *preparer) params(v any, where string) (any, error) {
	list, ok := v.([]any)
	if !ok {
		return v, nil
	}
	out := make([]any, 0, len(list))
	for _, item := range list {
		pm, ok := item.(map[string]any)
		if !ok {
			out = append(out, item)
			continue
		}
		pm = deepCopy(pm).(map[string]any)
		if s, ok := pm["schema"]; ok {
			ns, err := p.schema(s, modeRequest)
			if err != nil {
				return nil, fmt.Errorf("%s parameter %v: %w", where, pm["name"], err)
			}
			pm["schema"] = ns
		}
		delete(pm, "example")
		delete(pm, "examples")
		out = append(out, pm)
	}
	return out, nil
}

func (p *preparer) content(holder map[string]any, where string, m mode) error {
	content, ok := holder["content"].(map[string]any)
	if !ok {
		return nil
	}
	for _, ct := range sortedKeys(content) {
		media, ok := content[ct].(map[string]any)
		if !ok {
			continue
		}
		if s, ok := media["schema"]; ok {
			ns, err := p.schema(s, m)
			if err != nil {
				return fmt.Errorf("%s %s: %w", where, ct, err)
			}
			media["schema"] = ns
		}
		delete(media, "example")
		delete(media, "examples")
	}
	return nil
}

func (p *preparer) ref(ref string, m mode) (map[string]any, error) {
	const prefix = "#/components/schemas/"
	if !strings.HasPrefix(ref, prefix) {
		return nil, fmt.Errorf("unsupported $ref %q", ref)
	}
	name := strings.TrimPrefix(ref, prefix)
	key := fmt.Sprintf("%d:%s", m, name)
	if v, ok := p.done[key]; ok {
		return deepCopy(v).(map[string]any), nil
	}
	if p.active[key] {
		return nil, fmt.Errorf("recursive schema %s is not supported", name)
	}
	target, ok := p.schemas[name]
	if !ok {
		return nil, fmt.Errorf("dangling $ref %q", ref)
	}
	p.active[key] = true
	v, err := p.schema(target, m)
	delete(p.active, key)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	p.done[key] = v
	return deepCopy(v).(map[string]any), nil
}

// schema prepares one schema object (recursively).
func (p *preparer) schema(v any, m mode) (map[string]any, error) {
	s, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("schema is %T, not an object", v)
	}
	s = shallowCopy(s)

	if ref, ok := s["$ref"].(string); ok {
		delete(s, "$ref")
		base, err := p.ref(ref, m)
		if err != nil {
			return nil, err
		}
		// Sibling keywords (3.1) override the referenced schema's.
		for k, sv := range s {
			if k == "description" || k == "default" || k == "deprecated" || k == "readOnly" || k == "writeOnly" || k == "title" {
				base[k] = sv
			}
		}
		s = base
		// The inlined schema is already prepared.
		return s, nil
	}

	delete(s, "example")
	delete(s, "examples")

	if all, ok := s["allOf"].([]any); ok {
		delete(s, "allOf")
		merged, err := p.flattenAllOf(all, m)
		if err != nil {
			return nil, err
		}
		for k, v := range merged {
			if _, exists := s[k]; !exists || k == "properties" || k == "required" {
				if k == "properties" || k == "required" {
					s[k] = mergeKeyword(k, s[k], v)
					continue
				}
				s[k] = v
			}
		}
	}

	for _, kw := range []string{"oneOf", "anyOf"} {
		alts, ok := s[kw].([]any)
		if !ok {
			continue
		}
		delete(s, kw)
		delete(s, "discriminator")
		u, err := p.union(alts, m)
		if err != nil {
			return nil, err
		}
		for k, v := range u {
			if _, exists := s[k]; !exists {
				s[k] = v
			}
		}
	}

	if t, ok := s["type"].([]any); ok {
		var nonNull []any
		for _, x := range t {
			if x != "null" {
				nonNull = append(nonNull, x)
			}
		}
		switch len(nonNull) {
		case 0:
			s["type"] = "null"
		case 1:
			s["type"] = nonNull[0]
		default:
			s["type"] = nonNull
		}
	}

	if c, ok := s["const"]; ok {
		delete(s, "const")
		if _, has := s["enum"]; !has {
			s["enum"] = []any{c}
		}
		if _, isString := c.(string); isString && s["type"] == nil {
			s["type"] = "string"
		}
	}

	if pat, ok := s["pattern"].(string); ok {
		if _, err := regexp.Compile(pat); err != nil {
			p.warnf("dropped pattern %q: not valid RE2 (%v)", pat, err)
			delete(s, "pattern")
		}
	}

	if props, ok := s["properties"].(map[string]any); ok {
		np := map[string]any{}
		required := stringList(s["required"])
		for _, name := range sortedKeys(props) {
			ps, err := p.schema(props[name], m)
			if err != nil {
				return nil, fmt.Errorf("property %s: %w", name, err)
			}
			if name == "object" && isConstString(ps) {
				// Every SF Compute response carries a constant
				// "object": "<type>" discriminator; it is noise in a
				// Terraform schema.
				required = remove(required, name)
				continue
			}
			if m == modeRequest && ps["readOnly"] == true {
				required = remove(required, name)
				continue
			}
			if m == modeResponse && ps["writeOnly"] == true {
				required = remove(required, name)
				continue
			}
			np[name] = ps
		}
		s["properties"] = np
		if len(required) > 0 {
			s["required"] = toAnyList(required)
		} else {
			delete(s, "required")
		}
	}
	if items, ok := s["items"]; ok {
		ni, err := p.schema(items, m)
		if err != nil {
			return nil, fmt.Errorf("items: %w", err)
		}
		s["items"] = ni
	}
	if ap, ok := s["additionalProperties"].(map[string]any); ok {
		na, err := p.schema(ap, m)
		if err != nil {
			return nil, fmt.Errorf("additionalProperties: %w", err)
		}
		s["additionalProperties"] = na
	}
	if pn, ok := s["propertyNames"].(map[string]any); ok {
		// Not understood by the generator; keep it prepared for readers.
		npn, err := p.schema(pn, m)
		if err != nil {
			return nil, err
		}
		s["propertyNames"] = npn
	}
	return s, nil
}

func (p *preparer) flattenAllOf(all []any, m mode) (map[string]any, error) {
	out := map[string]any{"type": "object"}
	props := map[string]any{}
	var required []string
	for i, sub := range all {
		ps, err := p.schema(sub, m)
		if err != nil {
			return nil, fmt.Errorf("allOf[%d]: %w", i, err)
		}
		if t, ok := ps["type"]; ok && t != "object" {
			return nil, fmt.Errorf("allOf[%d] has type %v; only object composition is supported", i, t)
		}
		for k, v := range ps {
			switch k {
			case "properties":
				pm, _ := v.(map[string]any)
				for _, name := range sortedKeys(pm) {
					if prev, ok := props[name]; ok && !reflect.DeepEqual(prev, pm[name]) {
						p.warnf("allOf: property %q defined differently by two subschemas; the later one wins", name)
					}
					props[name] = pm[name]
				}
			case "required":
				for _, r := range stringList(v) {
					if !contains(required, r) {
						required = append(required, r)
					}
				}
			case "type":
			default:
				if _, exists := out[k]; !exists {
					out[k] = v
				}
			}
		}
	}
	out["properties"] = props
	if len(required) > 0 {
		out["required"] = toAnyList(required)
	}
	return out, nil
}

func (p *preparer) union(alts []any, m mode) (map[string]any, error) {
	var prepared []map[string]any
	for i, a := range alts {
		ps, err := p.schema(a, m)
		if err != nil {
			return nil, fmt.Errorf("alternative %d: %w", i, err)
		}
		if ps["type"] == "null" {
			continue
		}
		prepared = append(prepared, ps)
	}
	switch {
	case len(prepared) == 0:
		return map[string]any{"type": "null"}, nil
	case len(prepared) == 1:
		return prepared[0], nil
	}
	allType := func(t string) bool {
		for _, ps := range prepared {
			if ps["type"] != t {
				return false
			}
		}
		return true
	}
	switch {
	case allType("string"):
		// e.g. an ID that may have one of several prefixes. Patterns are
		// combined; enums are unioned.
		out := map[string]any{"type": "string"}
		var pats []string
		var enum []any
		hasEnum := true
		for _, ps := range prepared {
			if pat, ok := ps["pattern"].(string); ok {
				pats = append(pats, "(?:"+pat+")")
			} else {
				pats = nil
				break
			}
		}
		for _, ps := range prepared {
			e, ok := ps["enum"].([]any)
			if !ok {
				hasEnum = false
				break
			}
			enum = append(enum, e...)
		}
		if len(pats) == len(prepared) {
			out["pattern"] = strings.Join(pats, "|")
		}
		if hasEnum {
			out["enum"] = enum
		}
		if d, ok := prepared[0]["description"]; ok {
			out["description"] = d
		}
		return out, nil
	case allType("object"):
		out := map[string]any{"type": "object"}
		props := map[string]any{}
		var common []string
		for i, ps := range prepared {
			pm, _ := ps["properties"].(map[string]any)
			for _, name := range sortedKeys(pm) {
				prev, ok := props[name]
				if !ok {
					props[name] = pm[name]
					continue
				}
				merged, ok := mergeEnumStrings(prev, pm[name])
				if !ok {
					p.warnf("union: property %q has different schemas in different alternatives; the first one wins", name)
					continue
				}
				props[name] = merged
			}
			req := stringList(ps["required"])
			if i == 0 {
				common = req
			} else {
				common = intersect(common, req)
			}
		}
		out["properties"] = props
		if len(common) > 0 {
			out["required"] = toAnyList(common)
		}
		return out, nil
	default:
		var types []string
		for _, ps := range prepared {
			types = append(types, fmt.Sprint(ps["type"]))
		}
		p.warnf("union of %s cannot be represented; left as a string", strings.Join(types, ", "))
		return map[string]any{"type": "string", "description": "JSON-encoded value (the API accepts several shapes here)."}, nil
	}
}

// mergeEnumStrings merges two string schemas that differ only in their enum
// (typical for a union's discriminator property).
func mergeEnumStrings(a, b any) (any, bool) {
	am, ok1 := a.(map[string]any)
	bm, ok2 := b.(map[string]any)
	if !ok1 || !ok2 {
		return nil, false
	}
	if reflect.DeepEqual(am, bm) {
		return am, true
	}
	if am["type"] != "string" || bm["type"] != "string" {
		return nil, false
	}
	ae, _ := am["enum"].([]any)
	be, _ := bm["enum"].([]any)
	if ae == nil || be == nil {
		return nil, false
	}
	out := shallowCopy(am)
	enum := append([]any{}, ae...)
	for _, v := range be {
		if !containsAny(enum, v) {
			enum = append(enum, v)
		}
	}
	out["enum"] = enum
	delete(out, "default")
	return out, true
}

func mergeKeyword(k string, a, b any) any {
	switch k {
	case "properties":
		out := map[string]any{}
		for kk, v := range asMap(b) {
			out[kk] = v
		}
		for kk, v := range asMap(a) {
			out[kk] = v
		}
		return out
	case "required":
		l := stringList(b)
		for _, r := range stringList(a) {
			if !contains(l, r) {
				l = append(l, r)
			}
		}
		return toAnyList(l)
	}
	return a
}

func isConstString(s map[string]any) bool {
	e, ok := s["enum"].([]any)
	return ok && len(e) == 1 && s["type"] == "string"
}

func isHTTPMethod(k string) bool {
	switch k {
	case "get", "put", "post", "delete", "options", "head", "patch", "trace":
		return true
	}
	return false
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func stringList(v any) []string {
	l, _ := v.([]any)
	out := make([]string, 0, len(l))
	for _, x := range l {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func toAnyList(l []string) []any {
	out := make([]any, len(l))
	for i, s := range l {
		out[i] = s
	}
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func containsAny(l []any, v any) bool {
	for _, x := range l {
		if reflect.DeepEqual(x, v) {
			return true
		}
	}
	return false
}

func remove(l []string, s string) []string {
	out := l[:0:0]
	for _, x := range l {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

func intersect(a, b []string) []string {
	var out []string
	for _, x := range a {
		if contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

func dedupe(l []string) []string {
	var out []string
	for i, s := range l {
		if i == 0 || s != l[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func shallowCopy(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = deepCopy(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = deepCopy(vv)
		}
		return out
	default:
		return v
	}
}

// ReadJSON decodes a JSON document preserving number literals.
func ReadJSON(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return doc, nil
}
