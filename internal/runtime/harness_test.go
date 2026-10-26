package runtime_test

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/hofvarpnir-studios/terraform-provider-sfcompute/internal/mockapi"
	"github.com/hofvarpnir-studios/terraform-provider-sfcompute/internal/provider"
	"github.com/hofvarpnir-studios/terraform-provider-sfcompute/internal/runtime"
)

// unknown marks a value as unknown when building test values.
type unknownT struct{}

var unknown = unknownT{}

type harness struct {
	t      *testing.T
	ctx    context.Context
	srv    *mockapi.Server
	client *runtime.Client
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	spec, err := mockapi.LoadSpec("../../spec/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := mockapi.New(spec)
	srv.SeedFixtures()
	client, err := runtime.NewClient(srv.URL, srv.Token, "terraform-provider-sfcompute/test")
	if err != nil {
		t.Fatal(err)
	}
	client.RetryWaitMin = time.Millisecond
	client.RetryWaitMax = 5 * time.Millisecond
	client.DeleteConflictTimeout = 50 * time.Millisecond
	h := &harness{t: t, ctx: context.Background(), srv: srv, client: client}
	t.Cleanup(func() {
		srv.Close()
		for _, v := range srv.Violations() {
			t.Errorf("request/response did not match spec/openapi.json: %s", v)
		}
	})
	return h
}

func resourceDef(t *testing.T, name string) *runtime.ResourceDefinition {
	t.Helper()
	for i := range provider.ResourceDefinitions {
		if provider.ResourceDefinitions[i].Name == name {
			return &provider.ResourceDefinitions[i]
		}
	}
	t.Fatalf("no generated resource %q", name)
	return nil
}

func dataSourceDef(t *testing.T, name string) *runtime.DataSourceDefinition {
	t.Helper()
	for i := range provider.DataSourceDefinitions {
		if provider.DataSourceDefinitions[i].Name == name {
			return &provider.DataSourceDefinitions[i]
		}
	}
	t.Fatalf("no generated data source %q", name)
	return nil
}

func noErrors(t *testing.T, what string, d diag.Diagnostics) {
	t.Helper()
	for _, x := range d {
		if x.Severity() == diag.SeverityError {
			t.Fatalf("%s: %s: %s", what, x.Summary(), x.Detail())
		}
	}
}

// tfv converts a Go test value to a Terraform value of typ.
func tfv(t *testing.T, typ tftypes.Type, v any) tftypes.Value {
	t.Helper()
	if v == unknown {
		return tftypes.NewValue(typ, tftypes.UnknownValue)
	}
	if v == nil {
		return tftypes.NewValue(typ, nil)
	}
	switch {
	case typ.Is(tftypes.String):
		return tftypes.NewValue(typ, v.(string))
	case typ.Is(tftypes.Bool):
		return tftypes.NewValue(typ, v.(bool))
	case typ.Is(tftypes.Number):
		switch n := v.(type) {
		case int:
			return tftypes.NewValue(typ, big.NewFloat(float64(n)))
		case int64:
			return tftypes.NewValue(typ, big.NewFloat(float64(n)))
		case float64:
			return tftypes.NewValue(typ, big.NewFloat(n))
		}
	}
	switch tt := typ.(type) {
	case tftypes.List:
		var out []tftypes.Value
		for _, e := range v.([]any) {
			out = append(out, tfv(t, tt.ElementType, e))
		}
		return tftypes.NewValue(typ, out)
	case tftypes.Set:
		var out []tftypes.Value
		for _, e := range v.([]any) {
			out = append(out, tfv(t, tt.ElementType, e))
		}
		return tftypes.NewValue(typ, out)
	case tftypes.Map:
		out := map[string]tftypes.Value{}
		for k, e := range v.(map[string]any) {
			out[k] = tfv(t, tt.ElementType, e)
		}
		return tftypes.NewValue(typ, out)
	case tftypes.Object:
		m := v.(map[string]any)
		out := map[string]tftypes.Value{}
		for name, at := range tt.AttributeTypes {
			out[name] = tfv(t, at, m[name])
		}
		for k := range m {
			if _, ok := tt.AttributeTypes[k]; !ok {
				t.Fatalf("test value sets unknown attribute %q", k)
			}
		}
		return tftypes.NewValue(typ, out)
	}
	t.Fatalf("cannot build %T for %s", v, typ)
	return tftypes.Value{}
}

// testResource drives one generated resource the way Terraform would.
type testResource struct {
	h     *harness
	r     *runtime.Resource
	sch   tfsdk.State
	typ   tftypes.Object
	state tftypes.Value
}

func (h *harness) resource(name string) *testResource {
	h.t.Helper()
	r := runtime.NewResource(resourceDef(h.t, name)).(*runtime.Resource)
	var cr resource.ConfigureResponse
	r.Configure(h.ctx, resource.ConfigureRequest{ProviderData: h.client}, &cr)
	noErrors(h.t, "configure", cr.Diagnostics)
	var sr resource.SchemaResponse
	r.Schema(h.ctx, resource.SchemaRequest{}, &sr)
	noErrors(h.t, "schema", sr.Diagnostics)
	typ := sr.Schema.Type().TerraformType(h.ctx).(tftypes.Object)
	return &testResource{h: h, r: r, sch: tfsdk.State{Schema: sr.Schema}, typ: typ, state: tftypes.NewValue(typ, nil)}
}

func (tr *testResource) config(cfg map[string]any) tftypes.Value {
	return tfv(tr.h.t, tr.typ, cfg)
}

// proposed mimics Terraform core's proposed new state: configured values,
// prior state for unset computed attributes on update, unknown for unset
// computed attributes on create (and, as the framework does, for every unset
// computed attribute when anything changes).
func (tr *testResource) proposed(cfg map[string]any) tftypes.Value {
	t := tr.h.t
	prior := map[string]tftypes.Value{}
	if !tr.state.IsNull() {
		_ = tr.state.As(&prior)
	}
	out := map[string]tftypes.Value{}
	changed := tr.state.IsNull()
	for name, at := range tr.typ.AttributeTypes {
		a := tr.sch.Schema.GetAttributes()[name]
		v, set := cfg[name]
		switch {
		case set:
			out[name] = tfv(t, at, v)
			if !tr.state.IsNull() && !out[name].Equal(prior[name]) {
				changed = true
			}
		case a.IsComputed() && !tr.state.IsNull():
			out[name] = prior[name]
		case a.IsComputed():
			out[name] = tftypes.NewValue(at, tftypes.UnknownValue)
		default:
			out[name] = tftypes.NewValue(at, nil)
			if !tr.state.IsNull() && !prior[name].IsNull() {
				changed = true
			}
		}
	}
	if changed && !tr.state.IsNull() {
		for name, at := range tr.typ.AttributeTypes {
			if _, set := cfg[name]; !set && tr.sch.Schema.GetAttributes()[name].IsComputed() {
				out[name] = tftypes.NewValue(at, tftypes.UnknownValue)
			}
		}
	}
	return tftypes.NewValue(tr.typ, out)
}

func (tr *testResource) plan(cfg map[string]any) (*resource.ModifyPlanResponse, tftypes.Value) {
	return tr.planWith(cfg, tr.proposed(cfg))
}

func (tr *testResource) planWith(cfg map[string]any, proposed tftypes.Value) (*resource.ModifyPlanResponse, tftypes.Value) {
	s := tr.sch.Schema
	req := resource.ModifyPlanRequest{
		Config: tfsdk.Config{Schema: s, Raw: tr.config(cfg)},
		Plan:   tfsdk.Plan{Schema: s, Raw: proposed},
		State:  tfsdk.State{Schema: s, Raw: tr.state},
	}
	resp := &resource.ModifyPlanResponse{Plan: req.Plan}
	tr.r.ModifyPlan(tr.h.ctx, req, resp)
	return resp, resp.Plan.Raw
}

// apply plans and applies cfg (create or update), failing on errors.
func (tr *testResource) apply(cfg map[string]any) {
	tr.h.t.Helper()
	pr, planned := tr.plan(cfg)
	noErrors(tr.h.t, "plan", pr.Diagnostics)
	if len(pr.RequiresReplace) > 0 {
		tr.h.t.Fatalf("unexpected replacement of %v", pr.RequiresReplace)
	}
	tr.applyPlanned(cfg, planned)
}

func (tr *testResource) applyPlanned(cfg map[string]any, planned tftypes.Value) diag.Diagnostics {
	tr.h.t.Helper()
	s := tr.sch.Schema
	config := tfsdk.Config{Schema: s, Raw: tr.config(cfg)}
	plan := tfsdk.Plan{Schema: s, Raw: planned}
	if tr.state.IsNull() {
		resp := &resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: tftypes.NewValue(tr.typ, nil)}}
		tr.r.Create(tr.h.ctx, resource.CreateRequest{Config: config, Plan: plan}, resp)
		if !resp.Diagnostics.HasError() {
			tr.checkConsistent(planned, resp.State.Raw)
			tr.state = resp.State.Raw
		}
		return resp.Diagnostics
	}
	resp := &resource.UpdateResponse{State: tfsdk.State{Schema: s, Raw: tr.state}}
	tr.r.Update(tr.h.ctx, resource.UpdateRequest{Config: config, Plan: plan, State: tfsdk.State{Schema: s, Raw: tr.state}}, resp)
	if !resp.Diagnostics.HasError() {
		tr.checkConsistent(planned, resp.State.Raw)
		tr.state = resp.State.Raw
	}
	return resp.Diagnostics
}

// checkConsistent enforces Terraform's rule that every known planned value
// survives apply unchanged and that the result is wholly known.
func (tr *testResource) checkConsistent(planned, result tftypes.Value) {
	t := tr.h.t
	t.Helper()
	if !result.IsFullyKnown() {
		t.Fatalf("apply result contains unknown values: %s", result)
	}
	_ = tftypes.Walk(planned, func(p *tftypes.AttributePath, pv tftypes.Value) (bool, error) {
		if !pv.IsFullyKnown() {
			return true, nil
		}
		rv, _, err := tftypes.WalkAttributePath(result, p)
		if err != nil {
			t.Errorf("planned %s missing from result: %v", p, err)
			return false, nil
		}
		if !rv.(tftypes.Value).Equal(pv) {
			t.Errorf("provider produced inconsistent result: %s planned %s, got %s", p, pv, rv)
		}
		return false, nil
	})
}

// refresh reads the resource; it returns false if it was removed.
func (tr *testResource) refresh() bool {
	tr.h.t.Helper()
	s := tr.sch.Schema
	resp := &resource.ReadResponse{State: tfsdk.State{Schema: s, Raw: tr.state}}
	tr.r.Read(tr.h.ctx, resource.ReadRequest{State: tfsdk.State{Schema: s, Raw: tr.state}}, resp)
	noErrors(tr.h.t, "read", resp.Diagnostics)
	tr.state = resp.State.Raw
	return !tr.state.IsNull()
}

func (tr *testResource) destroy() diag.Diagnostics {
	tr.h.t.Helper()
	s := tr.sch.Schema
	resp := &resource.DeleteResponse{State: tfsdk.State{Schema: s, Raw: tr.state}}
	tr.r.Delete(tr.h.ctx, resource.DeleteRequest{State: tfsdk.State{Schema: s, Raw: tr.state}}, resp)
	if !resp.Diagnostics.HasError() {
		tr.state = tftypes.NewValue(tr.typ, nil)
	}
	return resp.Diagnostics
}

// get returns a top-level attribute of the current state as a Go value.
func (tr *testResource) get(name string) any {
	return goValue(tr.h.t, tr.attr(name))
}

func (tr *testResource) attr(name string) tftypes.Value {
	var m map[string]tftypes.Value
	if err := tr.state.As(&m); err != nil {
		tr.h.t.Fatal(err)
	}
	return m[name]
}

func goValue(t *testing.T, v tftypes.Value) any {
	if !v.IsKnown() {
		return unknown
	}
	if v.IsNull() {
		return nil
	}
	switch {
	case v.Type().Is(tftypes.String):
		var s string
		_ = v.As(&s)
		return s
	case v.Type().Is(tftypes.Bool):
		var b bool
		_ = v.As(&b)
		return b
	case v.Type().Is(tftypes.Number):
		var f big.Float
		_ = v.As(&f)
		i, _ := f.Int64()
		return i
	}
	switch v.Type().(type) {
	case tftypes.List, tftypes.Set, tftypes.Tuple:
		var l []tftypes.Value
		_ = v.As(&l)
		out := make([]any, len(l))
		for i, e := range l {
			out[i] = goValue(t, e)
		}
		return out
	case tftypes.Map, tftypes.Object:
		var m map[string]tftypes.Value
		_ = v.As(&m)
		out := map[string]any{}
		for k, e := range m {
			out[k] = goValue(t, e)
		}
		return out
	}
	t.Fatalf("unsupported %s", v.Type())
	return nil
}

// readDataSource runs a generated data source with cfg.
func (h *harness) readDataSource(name string, cfg map[string]any) (map[string]any, diag.Diagnostics) {
	h.t.Helper()
	d := runtime.NewDataSource(dataSourceDef(h.t, name)).(*runtime.DataSource)
	var cr datasource.ConfigureResponse
	d.Configure(h.ctx, datasource.ConfigureRequest{ProviderData: h.client}, &cr)
	var sr datasource.SchemaResponse
	d.Schema(h.ctx, datasource.SchemaRequest{}, &sr)
	noErrors(h.t, "schema", sr.Diagnostics)
	typ := sr.Schema.Type().TerraformType(h.ctx).(tftypes.Object)
	req := datasource.ReadRequest{Config: tfsdk.Config{Schema: sr.Schema, Raw: tfv(h.t, typ, cfg)}}
	resp := &datasource.ReadResponse{State: tfsdk.State{Schema: sr.Schema, Raw: tftypes.NewValue(typ, nil)}}
	d.Read(h.ctx, req, resp)
	if resp.Diagnostics.HasError() {
		return nil, resp.Diagnostics
	}
	return goValue(h.t, resp.State.Raw).(map[string]any), resp.Diagnostics
}

func hasError(d diag.Diagnostics, substr string) bool {
	for _, x := range d {
		if x.Severity() == diag.SeverityError && (strings.Contains(x.Summary(), substr) || strings.Contains(x.Detail(), substr)) {
			return true
		}
	}
	return false
}
