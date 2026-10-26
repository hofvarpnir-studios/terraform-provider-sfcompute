package runtime

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// Resource is the generic implementation behind every generated resource.
type Resource struct {
	def    *ResourceDefinition
	client *Client
	names  names

	once       sync.Once
	schema     schema.Schema
	objType    tftypes.Object
	computed   map[string]bool // computed-only attributes
	optComp    map[string]bool // optional+computed attributes
	createOnly map[string]bool // configurable attributes the update cannot change
	preserve   map[string]bool // configurable attributes whose API echo is not authoritative
	stable     map[string]bool
	initErr    error
}

var (
	_ resource.ResourceWithConfigure   = (*Resource)(nil)
	_ resource.ResourceWithModifyPlan  = (*Resource)(nil)
	_ resource.ResourceWithImportState = (*Resource)(nil)
)

// NewResource returns a resource for def.
func NewResource(def *ResourceDefinition) resource.Resource {
	return &Resource{def: def, names: newNames(def.Renames)}
}

func (r *Resource) init(ctx context.Context) error {
	r.once.Do(func() {
		s := r.def.Schema(ctx)
		r.schema = s
		t, ok := s.Type().TerraformType(ctx).(tftypes.Object)
		if !ok {
			r.initErr = fmt.Errorf("schema of %s is not an object", r.def.Name)
			return
		}
		r.objType = t
		updatable := map[string]bool{}
		if r.def.Update != nil {
			for _, f := range r.def.Update.Body {
				updatable[r.names.attr(f)] = true
			}
		}
		r.computed, r.optComp, r.createOnly, r.preserve, r.stable = map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
		for name, a := range s.Attributes {
			switch {
			case a.IsComputed() && !a.IsOptional() && !a.IsRequired():
				r.computed[name] = true
				continue
			case a.IsComputed() && a.IsOptional():
				r.optComp[name] = true
			}
			if !updatable[name] {
				r.createOnly[name] = true
				r.preserve[name] = true
			}
		}
		for _, n := range r.def.ShapeMismatch {
			if _, ok := s.Attributes[n]; ok && !r.computed[n] {
				r.preserve[n] = true
			}
		}
		for _, n := range r.def.Stable {
			if r.computed[n] {
				r.stable[n] = true
			}
		}
	})
	return r.initErr
}

// Metadata implements resource.Resource.
func (r *Resource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_" + r.def.Name
}

// Schema implements resource.Resource.
func (r *Resource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	if err := r.init(ctx); err != nil {
		resp.Diagnostics.AddError("Invalid generated schema", err.Error())
		return
	}
	s := r.schema
	s.MarkdownDescription = r.description()
	s.Description = s.MarkdownDescription
	resp.Schema = s
}

func (r *Resource) description() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Manages an SF Compute %s (created with `%s %s`", strings.ReplaceAll(r.def.Name, "_", " "), r.def.Create.Method, r.def.Create.Path)
	if r.def.Update != nil {
		fmt.Fprintf(&sb, ", updated in place with `%s %s`", r.def.Update.Method, r.def.Update.Path)
	} else {
		sb.WriteString("; it cannot be updated in place, so any change replaces it")
	}
	sb.WriteString(").")
	if r.def.Spending {
		fmt.Fprintf(&sb, "\n\n~> **Spends money.** Creating this resource is blocked unless the provider is configured with `allow_spending = true`, because %s.", r.def.SpendingNote)
	}
	return sb.String()
}

// Configure implements resource.ResourceWithConfigure.
func (r *Resource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("expected *runtime.Client, got %T", req.ProviderData))
		return
	}
	r.client = c
}

// ImportState implements resource.ResourceWithImportState: the import ID is
// the resource's ID or resource path.
func (r *Resource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *Resource) typeName() string { return "sfcompute_" + r.def.Name }

// SpendingBlockedError is the diagnostic shown when a spending resource
// would be created without allow_spending.
func SpendingBlockedError(typeName, note string) (string, string) {
	return "Creating " + typeName + " requires allow_spending = true",
		fmt.Sprintf("%s is guarded because %s.\n\nThe provider is configured with allow_spending = false (the default), so it refuses to create this resource. If you intend to spend money, set allow_spending = true in the provider \"sfcompute\" block.", typeName, note)
}

// ModifyPlan implements resource.ResourceWithModifyPlan. It applies the
// generic planning rules every generated resource shares:
//   - spending resources cannot be planned for creation without allow_spending;
//   - an unset workspace defaults to the provider's workspace;
//   - changing a create-only attribute requires replacement;
//   - optional+computed attributes left unset, and stable computed attributes,
//     keep their prior value instead of becoming "known after apply".
func (r *Resource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // destroy
	}
	if err := r.init(ctx); err != nil {
		resp.Diagnostics.AddError("Invalid generated schema", err.Error())
		return
	}
	plan, err := objectAttrs(req.Plan.Raw)
	if err != nil {
		resp.Diagnostics.AddError("Reading plan", err.Error())
		return
	}
	cfg, err := objectAttrs(req.Config.Raw)
	if err != nil {
		resp.Diagnostics.AddError("Reading configuration", err.Error())
		return
	}
	changed := false

	if req.State.Raw.IsNull() {
		// Create.
		if r.def.WorkspaceAttr != "" {
			if v := cfg[r.def.WorkspaceAttr]; v.IsNull() {
				switch {
				case r.client == nil:
					// Provider configuration not known yet.
				case r.client.DefaultWorkspace != "":
					plan[r.def.WorkspaceAttr] = tftypes.NewValue(tftypes.String, r.client.DefaultWorkspace)
					changed = true
				default:
					resp.Diagnostics.AddAttributeError(path.Root(r.def.WorkspaceAttr), "Missing workspace",
						fmt.Sprintf("%s needs a workspace: set %q on the resource, or set `workspace` in the provider configuration (e.g. \"sfc:workspace:<account>:<workspace>\").", r.typeName(), r.def.WorkspaceAttr))
				}
			}
		}
		if r.def.Spending && r.client != nil && !r.client.AllowSpending {
			summary, detail := SpendingBlockedError(r.typeName(), r.def.SpendingNote)
			resp.Diagnostics.AddError(summary, detail)
		}
	} else {
		state, err := objectAttrs(req.State.Raw)
		if err != nil {
			resp.Diagnostics.AddError("Reading state", err.Error())
			return
		}
		var replace []string
		for _, name := range sortedAttrNames(r.objType) {
			pv, sv := plan[name], state[name]
			switch {
			case r.computed[name]:
				if r.stable[name] && !pv.IsKnown() && !sv.IsNull() {
					plan[name] = sv
					changed = true
				}
				continue
			case r.optComp[name] && cfg[name].IsNull() && !pv.IsKnown():
				// Unset optional+computed attribute: the API keeps its
				// value unless told otherwise.
				plan[name] = sv
				pv = sv
				changed = true
			}
			if r.createOnly[name] && !pv.Equal(sv) {
				replace = append(replace, name)
			}
		}
		for _, name := range replace {
			resp.RequiresReplace = append(resp.RequiresReplace, path.Root(name))
		}
		if len(replace) > 0 && r.def.Spending && r.client != nil && !r.client.AllowSpending {
			summary, detail := SpendingBlockedError(r.typeName(), r.def.SpendingNote)
			resp.Diagnostics.AddError(summary, detail+fmt.Sprintf("\n\n(The change to %s forces a replacement, which would create a new %s.)", strings.Join(replace, ", "), r.typeName()))
		}
	}
	if changed {
		resp.Plan.Raw = tftypes.NewValue(r.objType, plan)
	}
}

func (r *Resource) ready(ctx context.Context, diags *diag.Diagnostics) bool {
	if err := r.init(ctx); err != nil {
		diags.AddError("Invalid generated schema", err.Error())
		return false
	}
	if r.client == nil {
		diags.AddError("Provider not configured", "The sfcompute provider has not been configured.")
		return false
	}
	return true
}

func (r *Resource) pathFor(op Operation, vals map[string]tftypes.Value) (string, error) {
	params := map[string]string{}
	for _, p := range op.PathParams {
		v, ok := vals[p.Attr]
		if !ok || !v.IsKnown() || v.IsNull() {
			return "", fmt.Errorf("%s %s: attribute %q (path parameter {%s}) has no value", op.Method, op.Path, p.Attr, p.Name)
		}
		s, err := scalarString(v)
		if err != nil {
			return "", err
		}
		params[p.Name] = s
	}
	return ExpandPath(op.Path, params)
}

func (r *Resource) bodyFor(op Operation, vals map[string]tftypes.Value) (map[string]any, error) {
	if len(op.Body) == 0 {
		return nil, nil
	}
	out := map[string]any{}
	for _, field := range op.Body {
		v, ok := vals[r.names.attr(field)]
		if !ok {
			continue
		}
		j, include, err := r.names.toJSONValue(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		if include {
			out[field] = j
		}
	}
	return out, nil
}

func (r *Resource) call(ctx context.Context, op Operation, vals map[string]tftypes.Value, withBody bool) (any, error) {
	p, err := r.pathFor(op, vals)
	if err != nil {
		return nil, err
	}
	req := Request{Method: op.Method, Path: p, IdempotencyHeader: op.IdempotencyHeader}
	if withBody {
		b, err := r.bodyFor(op, vals)
		if err != nil {
			return nil, err
		}
		if b != nil {
			req.Body = b
		}
	}
	return r.client.Do(ctx, req)
}

// applied builds the new state after create or update: known planned values
// are kept, unknown ones are filled from the API response.
func (r *Resource) applied(planned map[string]tftypes.Value, response map[string]any) (tftypes.Value, []string, error) {
	d := &decoder{names: r.names}
	out := make(map[string]tftypes.Value, len(r.objType.AttributeTypes))
	for _, name := range sortedAttrNames(r.objType) {
		at := r.objType.AttributeTypes[name]
		pv, ok := planned[name]
		if !ok {
			pv = tftypes.NewValue(at, tftypes.UnknownValue)
		}
		api := d.value(at, response[r.names.json(name)], name)
		mv, err := mergeKnown(pv, api)
		if err != nil {
			return tftypes.Value{}, nil, fmt.Errorf("%s: %w", name, err)
		}
		out[name] = mv
	}
	return tftypes.NewValue(r.objType, out), d.warnings, nil
}

// refreshed builds the new state after a read.
func (r *Resource) refreshed(prior map[string]tftypes.Value, response map[string]any) tftypes.Value {
	d := &decoder{names: r.names}
	out := make(map[string]tftypes.Value, len(r.objType.AttributeTypes))
	for _, name := range sortedAttrNames(r.objType) {
		at := r.objType.AttributeTypes[name]
		pv, ok := prior[name]
		if !ok || !pv.IsKnown() {
			pv = tftypes.NewValue(at, nil)
		}
		raw, present := response[r.names.json(name)]
		switch {
		case !present:
			// Write-only or not echoed (e.g. cloud-init user data).
			out[name] = pv
		case r.computed[name]:
			out[name] = d.value(at, raw, name)
		case r.preserve[name] && !pv.IsNull():
			// The API cannot change it in place, or echoes it in a
			// different form (an ID for a resource path, an object for
			// an ID); trust what was applied.
			out[name] = pv
		default:
			api := d.value(at, raw, name)
			if emptyish(pv) && emptyish(api) {
				out[name] = pv
			} else {
				out[name] = api
			}
		}
	}
	return tftypes.NewValue(r.objType, out)
}

func (r *Resource) logWarnings(ctx context.Context, warnings []string) {
	for _, w := range warnings {
		tflog.Warn(ctx, "SF Compute API response did not match the schema", map[string]any{"resource": r.typeName(), "detail": w})
	}
}

// Create implements resource.Resource.
func (r *Resource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	if !r.ready(ctx, &resp.Diagnostics) {
		return
	}
	if r.def.Spending && !r.client.AllowSpending {
		summary, detail := SpendingBlockedError(r.typeName(), r.def.SpendingNote)
		resp.Diagnostics.AddError(summary, detail)
		return
	}
	plan, err := objectAttrs(req.Plan.Raw)
	if err != nil {
		resp.Diagnostics.AddError("Reading plan", err.Error())
		return
	}
	out, err := r.call(ctx, r.def.Create, plan, true)
	if err != nil {
		resp.Diagnostics.AddError("Creating "+r.typeName(), err.Error())
		return
	}
	obj, ok := out.(map[string]any)
	if !ok {
		resp.Diagnostics.AddError("Creating "+r.typeName(), fmt.Sprintf("%s %s returned no object (got %s)", r.def.Create.Method, r.def.Create.Path, jsonKind(out)))
		return
	}
	state, warnings, err := r.applied(plan, obj)
	if err != nil {
		resp.Diagnostics.AddError("Recording "+r.typeName(), fmt.Sprintf("The resource was created (response: id=%v) but its state could not be built: %v", obj["id"], err))
		return
	}
	r.logWarnings(ctx, warnings)
	resp.State.Raw = state
}

// Read implements resource.Resource.
func (r *Resource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if !r.ready(ctx, &resp.Diagnostics) {
		return
	}
	prior, err := objectAttrs(req.State.Raw)
	if err != nil {
		resp.Diagnostics.AddError("Reading state", err.Error())
		return
	}
	out, err := r.call(ctx, r.def.Read, prior, false)
	if StatusOf(err) == http.StatusNotFound {
		tflog.Info(ctx, "resource no longer exists; removing it from state", map[string]any{"resource": r.typeName()})
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Reading "+r.typeName(), err.Error())
		return
	}
	obj, ok := out.(map[string]any)
	if !ok {
		resp.Diagnostics.AddError("Reading "+r.typeName(), fmt.Sprintf("%s %s returned no object (got %s)", r.def.Read.Method, r.def.Read.Path, jsonKind(out)))
		return
	}
	resp.State.Raw = r.refreshed(prior, obj)
}

// Update implements resource.Resource.
func (r *Resource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if !r.ready(ctx, &resp.Diagnostics) {
		return
	}
	if r.def.Update == nil {
		resp.Diagnostics.AddError("Updating "+r.typeName(), "This resource cannot be updated in place; this is a provider bug (the change should have been planned as a replacement).")
		return
	}
	plan, err := objectAttrs(req.Plan.Raw)
	if err != nil {
		resp.Diagnostics.AddError("Reading plan", err.Error())
		return
	}
	state, err := objectAttrs(req.State.Raw)
	if err != nil {
		resp.Diagnostics.AddError("Reading state", err.Error())
		return
	}
	// Path parameters (the ID) come from state: the plan may still show
	// them as unknown.
	vals := make(map[string]tftypes.Value, len(plan))
	for k, v := range plan {
		vals[k] = v
	}
	for _, p := range r.def.Update.PathParams {
		if v := vals[p.Attr]; !v.IsKnown() || v.IsNull() {
			vals[p.Attr] = state[p.Attr]
		}
	}
	out, err := r.call(ctx, *r.def.Update, vals, true)
	if err != nil {
		resp.Diagnostics.AddError("Updating "+r.typeName(), err.Error())
		return
	}
	obj, ok := out.(map[string]any)
	if !ok {
		// No body (e.g. 204): read the result back.
		out, err = r.call(ctx, r.def.Read, vals, false)
		if err != nil {
			resp.Diagnostics.AddError("Reading "+r.typeName()+" after update", err.Error())
			return
		}
		if obj, ok = out.(map[string]any); !ok {
			resp.Diagnostics.AddError("Reading "+r.typeName()+" after update", "the API returned no object")
			return
		}
	}
	newState, warnings, err := r.applied(vals, obj)
	if err != nil {
		resp.Diagnostics.AddError("Recording "+r.typeName(), err.Error())
		return
	}
	r.logWarnings(ctx, warnings)
	resp.State.Raw = newState
}

// Delete implements resource.Resource.
func (r *Resource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	if !r.ready(ctx, &resp.Diagnostics) {
		return
	}
	state, err := objectAttrs(req.State.Raw)
	if err != nil {
		resp.Diagnostics.AddError("Reading state", err.Error())
		return
	}
	for _, op := range r.def.BeforeDelete {
		_, err := r.call(ctx, op, state, false)
		if s := StatusOf(err); err != nil && s != http.StatusNotFound && s != http.StatusConflict {
			resp.Diagnostics.AddError("Deleting "+r.typeName(), fmt.Sprintf("%s %s (before delete): %v", op.Method, op.Path, err))
			return
		}
	}
	deadline := time.Now().Add(r.client.DeleteConflictTimeout)
	wait := r.client.RetryWaitMin
	for {
		_, err := r.call(ctx, r.def.Delete, state, false)
		status := StatusOf(err)
		switch {
		case err == nil, status == http.StatusNotFound:
			return
		case containsInt(r.def.DeleteAccept, status):
			resp.Diagnostics.AddWarning("Removed "+r.typeName()+" from state",
				fmt.Sprintf("%s %s answered %d; the resource is removed from Terraform state without further action:\n%v", r.def.Delete.Method, r.def.Delete.Path, status, err))
			return
		case status == http.StatusConflict && time.Now().Before(deadline):
			tflog.Info(ctx, "delete conflicted; retrying", map[string]any{"resource": r.typeName(), "wait": wait.String()})
			select {
			case <-ctx.Done():
				resp.Diagnostics.AddError("Deleting "+r.typeName(), ctx.Err().Error())
				return
			case <-time.After(wait):
			}
			if wait *= 2; wait > r.client.RetryWaitMax {
				wait = r.client.RetryWaitMax
			}
		default:
			resp.Diagnostics.AddError("Deleting "+r.typeName(), err.Error())
			return
		}
	}
}

func containsInt(l []int, v int) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}
