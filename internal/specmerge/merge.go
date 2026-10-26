package specmerge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// mergeDepth says how many levels of object keys below a top-level key are
// merged key-by-key before values are compared atomically. For example
// paths./v2/orders.post is one unit: two slices may contribute different
// methods for the same path, but if both contribute "post" they must agree
// exactly.
var mergeDepth = map[string]int{
	"paths":      2, // path -> method (or path-level "parameters")
	"webhooks":   2,
	"components": 2, // section (schemas, parameters, ...) -> name
}

// Conflict describes two slices that disagree about the same definition.
type Conflict struct {
	Pointer string // JSON pointer of the definition, e.g. /components/schemas/Order
	First   string // origin that defined it first
	Second  string // origin that disagreed
	A, B    any
	More    int // further origins that agreed with Second
}

func (c Conflict) String() string {
	more := ""
	if c.More > 0 {
		more = fmt.Sprintf(" (and %d more)", c.More)
	}
	return fmt.Sprintf("%s: %s and %s%s disagree\n  first:  %s\n  second: %s",
		c.Pointer, c.First, c.Second, more, abbreviate(c.A), abbreviate(c.B))
}

func abbreviate(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	const max = 400
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}

// ConflictError is returned by Merger.Result when slices disagree.
type ConflictError struct{ Conflicts []Conflict }

func (e *ConflictError) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d conflicting duplicate definition(s):", len(e.Conflicts))
	for _, c := range e.Conflicts {
		sb.WriteString("\n- ")
		sb.WriteString(c.String())
	}
	return sb.String()
}

// Merger accumulates OpenAPI document slices.
type Merger struct {
	doc       map[string]any
	origin    map[string]string // pointer -> first origin
	tags      map[string]any    // name + NUL + canonical JSON -> tag
	conflicts []Conflict
	slices    int
}

// NewMerger returns an empty Merger.
func NewMerger() *Merger {
	return &Merger{doc: map[string]any{}, origin: map[string]string{}, tags: map[string]any{}}
}

// Add merges one document slice. origin names the slice in conflict reports
// (typically the documentation URL it came from).
func (m *Merger) Add(origin string, doc map[string]any) error {
	m.slices++
	for _, k := range sortedKeys(doc) {
		v := doc[k]
		switch {
		case k == "tags":
			// Tags only group operations in documentation. SF Compute's
			// document repeats some tag names with different descriptions
			// within a single slice, so tags are merged as a set of
			// distinct tag objects rather than keyed by name.
			list, ok := v.([]any)
			if !ok {
				return fmt.Errorf("%s: /tags is not a list", origin)
			}
			for i, t := range list {
				tm, ok := t.(map[string]any)
				name, _ := tm["name"].(string)
				if !ok || name == "" {
					return fmt.Errorf("%s: /tags/%d has no name", origin, i)
				}
				b, err := json.Marshal(tm)
				if err != nil {
					return err
				}
				m.tags[name+"\x00"+string(b)] = tm
			}
		case mergeDepth[k] > 0:
			sub, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: /%s is not an object", origin, k)
			}
			dst, _ := m.doc[k].(map[string]any)
			if dst == nil {
				dst = map[string]any{}
				m.doc[k] = dst
			}
			if err := m.mergeObject(origin, "/"+escape(k), dst, sub, mergeDepth[k]); err != nil {
				return err
			}
		default:
			m.mergeAtomic(origin, "/"+escape(k), m.doc, k, v)
		}
	}
	return nil
}

func (m *Merger) mergeObject(origin, ptr string, dst, src map[string]any, depth int) error {
	for _, k := range sortedKeys(src) {
		v := src[k]
		p := ptr + "/" + escape(k)
		if depth <= 1 {
			m.mergeAtomic(origin, p, dst, k, v)
			continue
		}
		sub, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: %s is not an object", origin, p)
		}
		d, _ := dst[k].(map[string]any)
		if d == nil {
			d = map[string]any{}
			dst[k] = d
		}
		if err := m.mergeObject(origin, p, d, sub, depth-1); err != nil {
			return err
		}
	}
	return nil
}

func (m *Merger) mergeAtomic(origin, ptr string, dst map[string]any, key string, v any) {
	existing, ok := dst[key]
	if !ok {
		dst[key] = v
		m.origin[ptr] = origin
		return
	}
	if reflect.DeepEqual(existing, v) {
		return
	}
	for i := range m.conflicts {
		if c := &m.conflicts[i]; c.Pointer == ptr && reflect.DeepEqual(c.B, v) {
			c.More++
			return
		}
	}
	m.conflicts = append(m.conflicts, Conflict{Pointer: ptr, First: m.origin[ptr], Second: origin, A: existing, B: v})
}

// Slices returns how many documents have been added.
func (m *Merger) Slices() int { return m.slices }

// Result returns the merged document, or a *ConflictError listing every
// definition that two slices disagreed about.
func (m *Merger) Result() (map[string]any, error) {
	if len(m.conflicts) > 0 {
		cs := append([]Conflict(nil), m.conflicts...)
		sort.SliceStable(cs, func(i, j int) bool { return cs[i].Pointer < cs[j].Pointer })
		return nil, &ConflictError{Conflicts: cs}
	}
	out := make(map[string]any, len(m.doc)+1)
	for k, v := range m.doc {
		out[k] = v
	}
	if len(m.tags) > 0 {
		// Sorted by name, then canonical JSON.
		tags := make([]any, 0, len(m.tags))
		for _, key := range sortedKeys(m.tags) {
			tags = append(tags, m.tags[key])
		}
		out["tags"] = tags
	}
	return out, nil
}

// Marshal renders a document as indented JSON with sorted object keys and a
// trailing newline, so that regenerating an unchanged API produces a
// byte-identical file.
func Marshal(doc any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// escape encodes a JSON pointer reference token (RFC 6901).
func escape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}
