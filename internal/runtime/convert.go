package runtime

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// names translates between attribute names and JSON property names.
type names struct {
	toJSON   map[string]string
	fromJSON map[string]string
}

func newNames(renames map[string]string) names {
	n := names{toJSON: renames, fromJSON: map[string]string{}}
	for tf, js := range renames {
		n.fromJSON[js] = tf
	}
	return n
}

func (n names) json(attr string) string {
	if j, ok := n.toJSON[attr]; ok {
		return j
	}
	return attr
}

func (n names) attr(jsonName string) string {
	if a, ok := n.fromJSON[jsonName]; ok {
		return a
	}
	return jsonName
}

// toJSON converts a Terraform value to a JSON-compatible Go value. include is
// false for null and unknown values, which are omitted from requests.
func (n names) toJSONValue(v tftypes.Value) (out any, include bool, err error) {
	if !v.IsKnown() || v.IsNull() {
		return nil, false, nil
	}
	typ := v.Type()
	switch {
	case typ.Is(tftypes.String):
		var s string
		err := v.As(&s)
		return s, true, err
	case typ.Is(tftypes.Number):
		var f big.Float
		if err := v.As(&f); err != nil {
			return nil, false, err
		}
		return numberJSON(&f), true, nil
	case typ.Is(tftypes.Bool):
		var b bool
		err := v.As(&b)
		return b, true, err
	}
	switch typ.(type) {
	case tftypes.List, tftypes.Set, tftypes.Tuple:
		var elems []tftypes.Value
		if err := v.As(&elems); err != nil {
			return nil, false, err
		}
		arr := make([]any, 0, len(elems))
		for _, e := range elems {
			j, ok, err := n.toJSONValue(e)
			if err != nil {
				return nil, false, err
			}
			if !ok {
				j = nil
			}
			arr = append(arr, j)
		}
		return arr, true, nil
	case tftypes.Map:
		var m map[string]tftypes.Value
		if err := v.As(&m); err != nil {
			return nil, false, err
		}
		obj := make(map[string]any, len(m))
		for k, e := range m {
			j, ok, err := n.toJSONValue(e)
			if err != nil {
				return nil, false, err
			}
			if ok {
				obj[k] = j
			}
		}
		return obj, true, nil
	case tftypes.Object:
		var m map[string]tftypes.Value
		if err := v.As(&m); err != nil {
			return nil, false, err
		}
		obj := make(map[string]any, len(m))
		for k, e := range m {
			j, ok, err := n.toJSONValue(e)
			if err != nil {
				return nil, false, err
			}
			if ok {
				obj[n.json(k)] = j
			}
		}
		return obj, true, nil
	}
	return nil, false, fmt.Errorf("unsupported Terraform type %s", typ)
}

func numberJSON(f *big.Float) json.Number {
	if f.IsInt() {
		i, _ := f.Int(nil)
		return json.Number(i.String())
	}
	f64, _ := f.Float64()
	return json.Number(strconv.FormatFloat(f64, 'g', -1, 64))
}

// decoder converts JSON values into Terraform values of a given type. It is
// lenient: a value whose shape does not fit the type becomes null (or, for
// strings, a sensible string rendering) and a warning is recorded, because
// failing an apply over a cosmetic schema mismatch would orphan resources.
type decoder struct {
	names    names
	warnings []string
}

func (d *decoder) warnf(path, format string, args ...any) {
	d.warnings = append(d.warnings, path+": "+fmt.Sprintf(format, args...))
}

func (d *decoder) value(typ tftypes.Type, j any, path string) tftypes.Value {
	if j == nil {
		return tftypes.NewValue(typ, nil)
	}
	switch {
	case typ.Is(tftypes.String):
		switch t := j.(type) {
		case string:
			return tftypes.NewValue(typ, t)
		case json.Number:
			return tftypes.NewValue(typ, t.String())
		case bool:
			return tftypes.NewValue(typ, strconv.FormatBool(t))
		case map[string]any:
			// A reference the API expands into a summary object, e.g.
			// {"id": "pool_x", "name": "dev"} where a string is expected.
			for _, k := range []string{"resource_path", "id", "name"} {
				if s, ok := t[k].(string); ok && s != "" {
					return tftypes.NewValue(typ, s)
				}
			}
		}
		b, _ := json.Marshal(j)
		d.warnf(path, "expected a string, got %s; storing it JSON-encoded", jsonKind(j))
		return tftypes.NewValue(typ, string(b))
	case typ.Is(tftypes.Number):
		var s string
		switch t := j.(type) {
		case json.Number:
			s = t.String()
		case string:
			s = t
		case float64:
			s = strconv.FormatFloat(t, 'g', -1, 64)
		default:
			d.warnf(path, "expected a number, got %s", jsonKind(j))
			return tftypes.NewValue(typ, nil)
		}
		f, _, err := big.ParseFloat(s, 10, 512, big.ToNearestEven)
		if err != nil {
			d.warnf(path, "expected a number, got %q", s)
			return tftypes.NewValue(typ, nil)
		}
		return tftypes.NewValue(typ, f)
	case typ.Is(tftypes.Bool):
		switch t := j.(type) {
		case bool:
			return tftypes.NewValue(typ, t)
		case string:
			if b, err := strconv.ParseBool(t); err == nil {
				return tftypes.NewValue(typ, b)
			}
		}
		d.warnf(path, "expected a boolean, got %s", jsonKind(j))
		return tftypes.NewValue(typ, nil)
	}
	switch t := typ.(type) {
	case tftypes.List, tftypes.Set:
		arr, ok := j.([]any)
		if !ok {
			d.warnf(path, "expected an array, got %s", jsonKind(j))
			return tftypes.NewValue(typ, nil)
		}
		var et tftypes.Type
		if l, ok := t.(tftypes.List); ok {
			et = l.ElementType
		} else {
			et = t.(tftypes.Set).ElementType
		}
		elems := make([]tftypes.Value, 0, len(arr))
		for i, e := range arr {
			elems = append(elems, d.value(et, e, fmt.Sprintf("%s[%d]", path, i)))
		}
		if _, isSet := t.(tftypes.Set); isSet {
			elems = dedupeValues(elems)
		}
		return tftypes.NewValue(typ, elems)
	case tftypes.Tuple:
		arr, ok := j.([]any)
		if !ok || len(arr) != len(t.ElementTypes) {
			d.warnf(path, "expected a %d-element array", len(t.ElementTypes))
			return tftypes.NewValue(typ, nil)
		}
		elems := make([]tftypes.Value, len(arr))
		for i, e := range arr {
			elems[i] = d.value(t.ElementTypes[i], e, fmt.Sprintf("%s[%d]", path, i))
		}
		return tftypes.NewValue(typ, elems)
	case tftypes.Map:
		obj, ok := j.(map[string]any)
		if !ok {
			d.warnf(path, "expected an object, got %s", jsonKind(j))
			return tftypes.NewValue(typ, nil)
		}
		m := make(map[string]tftypes.Value, len(obj))
		for k, e := range obj {
			m[k] = d.value(t.ElementType, e, path+"."+k)
		}
		return tftypes.NewValue(typ, m)
	case tftypes.Object:
		obj, ok := j.(map[string]any)
		if !ok {
			d.warnf(path, "expected an object, got %s", jsonKind(j))
			return tftypes.NewValue(typ, nil)
		}
		m := make(map[string]tftypes.Value, len(t.AttributeTypes))
		for name, at := range t.AttributeTypes {
			m[name] = d.value(at, obj[d.names.json(name)], path+"."+name)
		}
		return tftypes.NewValue(typ, m)
	}
	d.warnf(path, "unsupported Terraform type %s", typ)
	return tftypes.NewValue(typ, nil)
}

func dedupeValues(in []tftypes.Value) []tftypes.Value {
	out := in[:0]
	for _, v := range in {
		dup := false
		for _, o := range out {
			if o.Equal(v) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, v)
		}
	}
	return out
}

func jsonKind(j any) string {
	switch j.(type) {
	case nil:
		return "null"
	case string:
		return "a string"
	case json.Number, float64:
		return "a number"
	case bool:
		return "a boolean"
	case []any:
		return "an array"
	case map[string]any:
		return "an object"
	}
	return fmt.Sprintf("%T", j)
}

// mergeKnown returns planned with every unknown part replaced by the
// corresponding part of actual. Known planned values always win: Terraform
// rejects an apply result that differs from a known planned value.
func mergeKnown(planned, actual tftypes.Value) (tftypes.Value, error) {
	if planned.IsFullyKnown() {
		return planned, nil
	}
	if !planned.IsKnown() {
		if actual.IsKnown() {
			return actual, nil
		}
		return tftypes.NewValue(planned.Type(), nil), nil
	}
	if actual.IsNull() || !actual.IsKnown() {
		return unknownsToNull(planned)
	}
	switch t := planned.Type().(type) {
	case tftypes.Object:
		var pm, am map[string]tftypes.Value
		if err := planned.As(&pm); err != nil {
			return planned, err
		}
		if err := actual.As(&am); err != nil {
			return planned, err
		}
		out := make(map[string]tftypes.Value, len(pm))
		for k, pv := range pm {
			av, ok := am[k]
			if !ok {
				av = tftypes.NewValue(t.AttributeTypes[k], nil)
			}
			mv, err := mergeKnown(pv, av)
			if err != nil {
				return planned, err
			}
			out[k] = mv
		}
		return tftypes.NewValue(t, out), nil
	case tftypes.List, tftypes.Set, tftypes.Tuple:
		var pl, al []tftypes.Value
		if err := planned.As(&pl); err != nil {
			return planned, err
		}
		if err := actual.As(&al); err != nil {
			return planned, err
		}
		out := make([]tftypes.Value, len(pl))
		for i, pv := range pl {
			av := tftypes.NewValue(pv.Type(), nil)
			if i < len(al) {
				av = al[i]
			}
			mv, err := mergeKnown(pv, av)
			if err != nil {
				return planned, err
			}
			out[i] = mv
		}
		return tftypes.NewValue(t, out), nil
	case tftypes.Map:
		var pm, am map[string]tftypes.Value
		if err := planned.As(&pm); err != nil {
			return planned, err
		}
		if err := actual.As(&am); err != nil {
			return planned, err
		}
		out := make(map[string]tftypes.Value, len(pm))
		for k, pv := range pm {
			av, ok := am[k]
			if !ok {
				av = tftypes.NewValue(t.ElementType, nil)
			}
			mv, err := mergeKnown(pv, av)
			if err != nil {
				return planned, err
			}
			out[k] = mv
		}
		return tftypes.NewValue(t, out), nil
	}
	return unknownsToNull(planned)
}

func unknownsToNull(v tftypes.Value) (tftypes.Value, error) {
	return tftypes.Transform(v, func(_ *tftypes.AttributePath, v tftypes.Value) (tftypes.Value, error) {
		if !v.IsKnown() {
			return tftypes.NewValue(v.Type(), nil), nil
		}
		return v, nil
	})
}

// emptyish reports whether v is null, an empty string, or an empty
// collection. APIs routinely return {} for a field that was never set (or
// null for one set to {}); such pairs are not drift.
func emptyish(v tftypes.Value) bool {
	if !v.IsKnown() {
		return false
	}
	if v.IsNull() {
		return true
	}
	if v.Type().Is(tftypes.String) {
		var s string
		_ = v.As(&s)
		return s == ""
	}
	switch v.Type().(type) {
	case tftypes.List, tftypes.Set, tftypes.Tuple:
		var l []tftypes.Value
		_ = v.As(&l)
		return len(l) == 0
	case tftypes.Map:
		var m map[string]tftypes.Value
		_ = v.As(&m)
		return len(m) == 0
	}
	return false
}

// scalarString renders a primitive Terraform value for a URL.
func scalarString(v tftypes.Value) (string, error) {
	j, ok, err := names{}.toJSONValue(v)
	if err != nil || !ok {
		return "", err
	}
	switch t := j.(type) {
	case string:
		return t, nil
	case json.Number:
		return t.String(), nil
	case bool:
		return strconv.FormatBool(t), nil
	}
	return "", fmt.Errorf("cannot use a %s value as a URL parameter", v.Type())
}

func objectAttrs(v tftypes.Value) (map[string]tftypes.Value, error) {
	if v.IsNull() || !v.IsKnown() {
		return map[string]tftypes.Value{}, nil
	}
	var m map[string]tftypes.Value
	if err := v.As(&m); err != nil {
		return nil, err
	}
	return m, nil
}

func sortedAttrNames(t tftypes.Object) []string {
	out := make([]string, 0, len(t.AttributeTypes))
	for k := range t.AttributeTypes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func describeWarnings(w []string) string {
	if len(w) > 10 {
		w = append(w[:10:10], fmt.Sprintf("... and %d more", len(w)-10))
	}
	return strings.Join(w, "\n")
}
