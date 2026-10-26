package codegen

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	doc, err := ReadJSON([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func render(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

const miniSpec = `{
  "openapi": "3.1.0",
  "paths": {
    "/v2/widgets": {
      "post": {
        "parameters": [{"name": "Idempotency-Key", "in": "header", "schema": {"type": "string"}}],
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/CreateWidget"}}}},
        "responses": {"201": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}}}
      },
      "get": {
        "parameters": [{"name": "workspace", "in": "query", "schema": {"type": "string"}},
                       {"name": "starting_after", "in": "query", "schema": {"type": "string"}},
                       {"name": "id", "in": "query", "schema": {"type": "array", "items": {"type": "string"}}}],
        "responses": {"200": {"content": {"application/json": {"schema": {
          "type": "object",
          "properties": {"data": {"type": "array", "items": {"$ref": "#/components/schemas/Widget"}},
                         "has_more": {"type": "boolean"},
                         "cursor": {"oneOf": [{"type": "null"}, {"type": "string"}]}}}}}}}
      }
    },
    "/v2/widgets/{id}": {
      "get": {
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}},
                       {"name": "verbose", "in": "query", "schema": {"type": "boolean"}}],
        "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}}}
      },
      "patch": {
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "requestBody": {"content": {"application/json": {"schema": {"type": "object", "properties": {"name": {"type": ["string", "null"]}}}}}},
        "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}}}
      },
      "delete": {
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {"204": {"description": "gone"}}
      }
    },
    "/preview/v2/widgets": {
      "post": {
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/CreateWidget"}}}},
        "responses": {"201": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}}}
      }
    },
    "/preview/v2/widgets/{id}": {
      "get": {"responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Widget"}}}}}},
      "delete": {"responses": {"204": {"description": "gone"}}}
    },
    "/preview/v2/me": {
      "get": {"responses": {"200": {"content": {"application/json": {"schema": {"type": "object", "properties": {"account_id": {"type": "string"}}}}}}}}
    },
    "/v2/widgets/{id}/frobnicate": {
      "post": {"parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}], "responses": {"200": {"description": "ok"}}}
    }
  },
  "components": {"schemas": {
    "Name": {"type": "string", "minLength": 1, "pattern": "[a-z]+"},
    "WorkspaceRef": {"type": "string", "pattern": "wksp_.*"},
    "CreateWidget": {
      "type": "object",
      "required": ["workspace", "color"],
      "properties": {
        "workspace": {"$ref": "#/components/schemas/WorkspaceRef", "description": "Workspace to create it in."},
        "name": {"oneOf": [{"type": "null"}, {"$ref": "#/components/schemas/Name"}]},
        "color": {"type": "string", "enum": ["red", "blue"]},
        "secret": {"type": "string", "writeOnly": true},
        "server_side": {"type": "string", "readOnly": true}
      }
    },
    "Scope": {"type": "object", "required": ["id"], "properties": {
      "id": {"oneOf": [{"type": "string", "pattern": "wdg_.*"}, {"type": "string", "pattern": "w_.*"}]},
      "workspace": {"type": "object", "properties": {"id": {"type": "string"}, "name": {"type": "string"}}}
    }},
    "Widget": {"allOf": [
      {"$ref": "#/components/schemas/Scope"},
      {"type": "object", "required": ["object"], "properties": {
        "object": {"const": "widget", "type": "string"},
        "name": {"$ref": "#/components/schemas/Name"},
        "color": {"type": "string", "enum": ["red", "blue"]},
        "createdAt": {"type": "integer"},
        "secret": {"type": "string", "writeOnly": true},
        "actor": {"oneOf": [
          {"type": "object", "required": ["type", "id"], "properties": {"type": {"const": "token"}, "id": {"type": "string"}}},
          {"type": "object", "required": ["type"], "properties": {"type": {"const": "system"}}}
        ]}
      }}
    ]}
  }}
}`

func prepared(t *testing.T) *Spec {
	t.Helper()
	p, err := Prepare(mustJSON(t, miniSpec))
	if err != nil {
		t.Fatal(err)
	}
	return &Spec{Doc: p.Doc}
}

func TestPrepareFlattensComposition(t *testing.T) {
	s := prepared(t)
	w := s.ResponseSchema("get", "/v2/widgets/{id}")
	if w["type"] != "object" || w["allOf"] != nil {
		t.Fatalf("allOf not flattened: %s", render(w))
	}
	props := properties(w)
	for _, want := range []string{"id", "workspace", "name", "color", "createdAt", "actor"} {
		if props[want] == nil {
			t.Errorf("flattened Widget lacks %q: %s", want, render(props))
		}
	}
	if props["object"] != nil {
		t.Error("constant object discriminator should be dropped")
	}
	if props["secret"] != nil {
		t.Error("writeOnly property should be dropped from responses")
	}
	if got := render(w["required"]); got != `["id"]` {
		t.Errorf("required = %s, want [\"id\"] (object removed)", got)
	}
	if id := props["id"].(map[string]any); id["type"] != "string" || id["oneOf"] != nil || id["pattern"] != "(?:wdg_.*)|(?:w_.*)" {
		t.Errorf("string union not merged: %s", render(id))
	}
	actor := props["actor"].(map[string]any)
	if render(actor["required"]) != `["type"]` {
		t.Errorf("object union required should be the intersection: %s", render(actor))
	}
	if typ := properties(actor)["type"].(map[string]any); render(typ["enum"]) != `["token","system"]` {
		t.Errorf("discriminator enums not merged: %s", render(typ))
	}
}

func TestPrepareRequestSchemas(t *testing.T) {
	s := prepared(t)
	c := s.RequestSchema("post", "/v2/widgets")
	props := properties(c)
	if props["server_side"] != nil {
		t.Error("readOnly property should be dropped from requests")
	}
	if props["secret"] == nil {
		t.Error("writeOnly property belongs in requests")
	}
	ws := props["workspace"].(map[string]any)
	if ws["description"] != "Workspace to create it in." || ws["pattern"] != "wksp_.*" || ws["$ref"] != nil {
		t.Errorf("$ref with sibling description not inlined correctly: %s", render(ws))
	}
	name := props["name"].(map[string]any)
	if name["type"] != "string" || name["oneOf"] != nil {
		t.Errorf("nullable oneOf not simplified: %s", render(name))
	}
	patch := s.RequestSchema("patch", "/v2/widgets/{id}")
	if pn := properties(patch)["name"].(map[string]any); pn["type"] != "string" {
		t.Errorf("type [string, null] not simplified: %s", render(pn))
	}
}

func TestPrepareDropsPatternsGoCannotCompile(t *testing.T) {
	p, err := Prepare(mustJSON(t, `{"paths": {"/x": {"get": {"responses": {"200": {"content": {"application/json": {"schema": {"type": "object", "properties": {"a": {"type": "string", "pattern": "^(?!foo).*$"}}}}}}}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	s := &Spec{Doc: p.Doc}
	a := properties(s.ResponseSchema("get", "/x"))["a"].(map[string]any)
	if _, ok := a["pattern"]; ok {
		t.Error("lookahead pattern should have been dropped")
	}
	if len(p.Warnings) != 1 {
		t.Errorf("expected one warning, got %v", p.Warnings)
	}
}

func TestPrepareRejectsRecursiveSchemas(t *testing.T) {
	_, err := Prepare(mustJSON(t, `{"paths": {"/x": {"get": {"responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Node"}}}}}}}},
	  "components": {"schemas": {"Node": {"type": "object", "properties": {"child": {"$ref": "#/components/schemas/Node"}}}}}}`))
	if err == nil || !strings.Contains(err.Error(), "recursive") {
		t.Fatalf("expected recursion error, got %v", err)
	}
}

func resolveMini(t *testing.T, cfgYAML string) *Resolution {
	t.Helper()
	cfg, err := ParseConfig([]byte(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Resolve(prepared(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestResolveDerivesFromPathConventions(t *testing.T) {
	res := resolveMini(t, `
provider: {name: test}
derive: {resources: true, data_sources: true, prefixes: [/v2, /preview/v2]}
skip:
  data_sources:
    me: renamed
resources:
  widget:
    spending: false
data_sources:
  account:
    read: {path: /preview/v2/me, method: GET}
`)
	if len(res.Plan.Resources) != 1 {
		t.Fatalf("want 1 resource, got %+v", res.Plan.Resources)
	}
	r := res.Plan.Resources[0]
	if r.Name != "widget" || r.Create.Path != "/v2/widgets" || r.Update == nil || r.Update.Method != "PATCH" || r.Delete.Method != "DELETE" {
		t.Errorf("unexpected derived resource: %+v", r)
	}
	if r.Spending {
		t.Error("widget was classified free")
	}
	if r.Create.IdempotencyHeader != "Idempotency-Key" {
		t.Error("idempotency header not detected")
	}
	if got := strings.Join(r.Create.Body, ","); got != "color,name,secret,workspace" {
		t.Errorf("create body = %s", got)
	}
	if got := strings.Join(r.Update.Body, ","); got != "name" {
		t.Errorf("update body = %s", got)
	}
	// workspace: string in, object out. secret: not echoed. name, color: same shape.
	if got := strings.Join(r.ShapeMismatch, ","); got != "secret,workspace" {
		t.Errorf("shape mismatch = %s", got)
	}
	if r.WorkspaceAttr != "workspace" {
		t.Error("workspace attribute not detected")
	}
	if strings.Join(r.Ignores, ",") != "verbose" {
		t.Errorf("read query params should be ignored in the resource schema: %v", r.Ignores)
	}
	if r.Renames["created_at"] != "createdAt" {
		t.Errorf("rename not recorded: %v", r.Renames)
	}

	ds := map[string]DataSourcePlan{}
	for _, d := range res.Plan.DataSources {
		ds[d.Name] = d
	}
	if w := ds["widgets"]; !w.List || !w.Paginate || w.WorkspaceAttr != "workspace" || len(w.Read.Query) != 3 || !w.Read.Query[2].Array {
		t.Errorf("unexpected list data source: %+v", w)
	}
	if w := ds["widget"]; w.Read.Path != "/v2/widgets/{id}" || w.List {
		t.Errorf("unexpected item data source: %+v", w)
	}
	if _, ok := ds["me"]; ok {
		t.Error("skipped data source generated")
	}
	if a := ds["account"]; a.Read.Path != "/preview/v2/me" {
		t.Errorf("explicit data source missing: %+v", a)
	}
	for _, want := range []string{
		"POST /preview/v2/widgets  not generated: superseded by /v2/widgets (resource widget)",
		"GET /preview/v2/widgets/{id}  not generated: superseded by /v2/widgets/{id} (data source widget)",
		"POST /v2/widgets/{id}/frobnicate  not generated: action endpoint",
		"DELETE /v2/widgets/{id}  resource sfcompute_widget (delete)",
	} {
		if !strings.Contains(strings.Join(strings.Fields(res.Coverage), " "), strings.Join(strings.Fields(want), " ")) {
			t.Errorf("coverage lacks %q:\n%s", want, res.Coverage)
		}
	}
	tf := res.TFGen.Resources["widget"]
	if tf.Schema == nil || strings.Join(tf.Schema.Ignores, ",") != "verbose" {
		t.Errorf("tfplugingen-openapi config lacks ignores: %+v", tf)
	}
}

func TestResolveUnclassifiedResourcesSpend(t *testing.T) {
	res := resolveMini(t, "provider: {name: test}\nderive: {resources: true}\n")
	if !res.Plan.Resources[0].Spending || res.Plan.Resources[0].SpendingNote == "" {
		t.Error("a resource nobody classified must be treated as spending")
	}
}

func TestResolveErrors(t *testing.T) {
	for name, cfg := range map[string]string{
		"missing op":       "provider: {name: t}\nresources:\n  gadget:\n    create: {path: /v2/gadgets, method: POST}\n    read: {path: '/v2/gadgets/{id}', method: GET}\n    delete: {path: '/v2/gadgets/{id}', method: DELETE}\n",
		"not derivable":    "provider: {name: t}\nresources:\n  gadget: {spending: false}\n",
		"skip+configured":  "provider: {name: t}\nderive: {resources: true}\nskip: {resources: {widget: x}}\nresources:\n  widget: {spending: false}\n",
		"non-GET data src": "provider: {name: t}\ndata_sources:\n  frob:\n    read: {path: '/v2/widgets/{id}/frobnicate', method: POST}\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := ParseConfig([]byte(cfg))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Resolve(prepared(t), c); err == nil {
				t.Error("expected an error")
			}
		})
	}
	if _, err := ParseConfig([]byte("provider: {name: t}\nresources:\n  w: {spendng: false}\n")); err == nil {
		t.Error("typo in config key should be rejected")
	}
}

func TestPatchCodeSpecMakesWorkspaceOptional(t *testing.T) {
	cs := mustJSON(t, `{"provider": {"name": "t"}, "resources": [{"name": "widget", "schema": {"attributes": [
	  {"name": "workspace", "string": {"computed_optional_required": "required", "description": "Where."}},
	  {"name": "id", "string": {"computed_optional_required": "computed"}}]}}], "datasources": []}`)
	plan := &Plan{Resources: []ResourcePlan{{Name: "widget", WorkspaceAttr: "workspace",
		Read: OpPlan{PathParams: []ParamPlan{{Name: "id", Attr: "id"}}}}}}
	if _, err := PatchCodeSpec(cs, plan); err != nil {
		t.Fatal(err)
	}
	got := render(cs)
	if !strings.Contains(got, `"computed_optional_required":"computed_optional","description":"Where. If omitted, the provider's `+"`workspace`"+` is used."`) {
		t.Errorf("workspace not patched: %s", got)
	}
	plan.Resources[0].Read.PathParams[0].Attr = "widget_id"
	if _, err := PatchCodeSpec(cs, plan); err == nil {
		t.Error("expected an error for a path parameter without attribute")
	}
}

func TestSlim(t *testing.T) {
	src := `// Code generated by terraform-plugin-framework-generator DO NOT EDIT.

package resource_x

import (
	"context"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
)

func XResourceSchema(ctx context.Context) schema.Schema {
	return schema.Schema{
		Attributes: map[string]schema.Attribute{
			"a": schema.SingleNestedAttribute{
				Attributes: map[string]schema.Attribute{},
				CustomType: AType{
					ObjectType: types.ObjectType{
						AttrTypes: AValue{}.AttributeTypes(ctx),
					},
				},
				Computed: true,
			},
		},
	}
}

type XModel struct {
	A AValue ` + "`tfsdk:\"a\"`" + `
}

type AType struct{ basetypes.ObjectType }

// AValue is a value.
type AValue struct{}

func (v AValue) AttributeTypes(ctx context.Context) map[string]attr.Type { return nil }
`
	out, err := Slim([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, gone := range []string{"CustomType", "XModel", "AType", "AValue", `"github.com/hashicorp/terraform-plugin-framework/attr"`, `/types"`} {
		if strings.Contains(s, gone) {
			t.Errorf("%s should have been removed:\n%s", gone, s)
		}
	}
	if !strings.Contains(s, SlimHeader) || !strings.Contains(s, "Computed: true") {
		t.Errorf("unexpected output:\n%s", s)
	}
}

func TestRegistryRenders(t *testing.T) {
	res := resolveMini(t, "provider: {name: test}\nderive: {resources: true, data_sources: true}\nresources:\n  widget:\n    spending: false\n    delete_accept_status: [422]\n")
	src, err := Registry(res.Plan, RegistryOptions{Package: "provider", Module: "example.com/m", GeneratedDir: "internal/generated", RuntimeImport: "example.com/m/internal/runtime"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, want := range []string{
		`resource_widget "example.com/m/internal/generated/resource_widget"`,
		`Schema:        resource_widget.WidgetResourceSchema,`,
		`DeleteAccept:  []int{422},`,
		`IdempotencyHeader: "Idempotency-Key"`,
		`datasource_widgets.WidgetsDataSourceSchema`,
		`Paginate:      true,`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("registry lacks %q", want)
		}
	}
}

// TestRealSpecResolves runs prepare and resolve over the committed snapshot
// and generator config, so a spec refresh that breaks the config fails here
// as well as in `make generate`.
func TestRealSpecResolves(t *testing.T) {
	data, err := os.ReadFile("../../spec/openapi.json")
	if err != nil {
		t.Skip("spec snapshot not available:", err)
	}
	doc, err := ReadJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Prepare(doc)
	if err != nil {
		t.Fatal(err)
	}
	cfgData, err := os.ReadFile("../../generator_config.yml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(cfgData)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Resolve(&Spec{Doc: p.Doc}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	spending := map[string]bool{}
	for _, r := range res.Plan.Resources {
		spending[r.Name] = r.Spending
	}
	for name, want := range map[string]bool{"order": true, "instance": true, "workspace": false, "pool": false, "firewall": false} {
		if got, ok := spending[name]; !ok || got != want {
			t.Errorf("resource %s: spending=%v (present=%v), want %v", name, got, ok, want)
		}
	}
	committed, err := os.ReadFile("../../generator/coverage.txt")
	if err == nil && string(committed) != res.Coverage {
		t.Error("generator/coverage.txt is stale; run `make generate`")
	}
}
