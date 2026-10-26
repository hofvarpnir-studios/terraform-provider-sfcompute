package codegen

import (
	"fmt"
)

// attributeKinds are the type keys an attribute object in the provider code
// specification can carry (exactly one per attribute).
var attributeKinds = []string{
	"bool", "float64", "int64", "list", "list_nested", "map", "map_nested",
	"number", "object", "set", "set_nested", "single_nested", "string",
}

// PatchCodeSpec adjusts the provider code specification produced by
// tfplugingen-openapi before tfplugingen-framework turns it into Go:
//
//   - The attribute that names a resource's or data source's workspace is
//     made optional (optional+computed), because the runtime fills it from
//     the provider's `workspace` setting when it is not configured.
//   - Attributes the plan expects to exist are checked, so a schema change
//     that would break the runtime fails generation instead of applies.
func PatchCodeSpec(codeSpec map[string]any, plan *Plan) ([]string, error) {
	var warnings []string
	resources := indexByName(codeSpec["resources"])
	dataSources := indexByName(codeSpec["datasources"])

	for _, rp := range plan.Resources {
		r, ok := resources[rp.Name]
		if !ok {
			return nil, fmt.Errorf("resource %s: missing from provider code spec (tfplugingen-openapi skipped it; see its warnings)", rp.Name)
		}
		attrs := attributesByName(r)
		for _, f := range rp.Create.Body {
			if _, ok := attrs[TerraformIdentifier(f)]; !ok {
				warnings = append(warnings, fmt.Sprintf("resource %s: request field %q was not mapped to an attribute (unsupported type?); it can never be set", rp.Name, f))
			}
		}
		for _, op := range append([]OpPlan{rp.Read, rp.Delete}, rp.BeforeDelete...) {
			for _, p := range op.PathParams {
				if _, ok := attrs[p.Attr]; !ok {
					return nil, fmt.Errorf("resource %s: path parameter {%s} of %s %s has no attribute %q (add an alias)", rp.Name, p.Name, op.Method, op.Path, p.Attr)
				}
			}
		}
		if rp.WorkspaceAttr != "" {
			if err := makeOptional(attrs, rp.WorkspaceAttr, "If omitted, the provider's `workspace` is used."); err != nil {
				return nil, fmt.Errorf("resource %s: %w", rp.Name, err)
			}
		}
	}
	for _, dp := range plan.DataSources {
		d, ok := dataSources[dp.Name]
		if !ok {
			return nil, fmt.Errorf("data source %s: missing from provider code spec (tfplugingen-openapi skipped it; see its warnings)", dp.Name)
		}
		attrs := attributesByName(d)
		for _, p := range append(append([]ParamPlan{}, dp.Read.PathParams...), dp.Read.Query...) {
			if _, ok := attrs[p.Attr]; !ok {
				return nil, fmt.Errorf("data source %s: parameter %q has no attribute %q", dp.Name, p.Name, p.Attr)
			}
		}
		if dp.WorkspaceAttr != "" {
			if err := makeOptional(attrs, dp.WorkspaceAttr, "If omitted, the provider's `workspace` is used."); err != nil {
				return nil, fmt.Errorf("data source %s: %w", dp.Name, err)
			}
		}
	}
	return warnings, nil
}

func makeOptional(attrs map[string]map[string]any, name, note string) error {
	a, ok := attrs[name]
	if !ok {
		return fmt.Errorf("attribute %q not found", name)
	}
	for _, kind := range attributeKinds {
		body, ok := a[kind].(map[string]any)
		if !ok {
			continue
		}
		if body["computed_optional_required"] == "required" {
			body["computed_optional_required"] = "computed_optional"
		}
		desc, _ := body["description"].(string)
		if desc != "" {
			desc += " "
		}
		body["description"] = desc + note
		return nil
	}
	return fmt.Errorf("attribute %q has no recognised type", name)
}

func indexByName(v any) map[string]map[string]any {
	out := map[string]map[string]any{}
	list, _ := v.([]any)
	for _, x := range list {
		if m, ok := x.(map[string]any); ok {
			if n, ok := m["name"].(string); ok {
				out[n] = m
			}
		}
	}
	return out
}

func attributesByName(obj map[string]any) map[string]map[string]any {
	schema, _ := obj["schema"].(map[string]any)
	return indexByName(schema["attributes"])
}
