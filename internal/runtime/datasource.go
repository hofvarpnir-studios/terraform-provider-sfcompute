package runtime

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dsschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// maxPages bounds automatic pagination.
const maxPages = 1000

// DataSource is the generic implementation behind every generated data
// source: one GET whose path and query parameters come from configuration
// and whose response fills the remaining attributes.
type DataSource struct {
	def    *DataSourceDefinition
	client *Client
	names  names

	once    sync.Once
	schema  dsschema.Schema
	objType tftypes.Object
	initErr error
}

var _ datasource.DataSourceWithConfigure = (*DataSource)(nil)

// NewDataSource returns a data source for def.
func NewDataSource(def *DataSourceDefinition) datasource.DataSource {
	return &DataSource{def: def, names: newNames(def.Renames)}
}

func (d *DataSource) init(ctx context.Context) error {
	d.once.Do(func() {
		d.schema = d.def.Schema(ctx)
		t, ok := d.schema.Type().TerraformType(ctx).(tftypes.Object)
		if !ok {
			d.initErr = fmt.Errorf("schema of %s is not an object", d.def.Name)
			return
		}
		d.objType = t
	})
	return d.initErr
}

// Metadata implements datasource.DataSource.
func (d *DataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_" + d.def.Name
}

// Schema implements datasource.DataSource.
func (d *DataSource) Schema(ctx context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	if err := d.init(ctx); err != nil {
		resp.Diagnostics.AddError("Invalid generated schema", err.Error())
		return
	}
	s := d.schema
	desc := fmt.Sprintf("Reads %s from SF Compute (`%s %s`).", strings.ReplaceAll(d.def.Name, "_", " "), d.def.Read.Method, d.def.Read.Path)
	if d.def.Paginate {
		desc += " All pages are fetched unless `limit`, `starting_after` or `ending_before` is set."
	}
	s.MarkdownDescription, s.Description = desc, desc
	resp.Schema = s
}

// Configure implements datasource.DataSourceWithConfigure.
func (d *DataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("expected *runtime.Client, got %T", req.ProviderData))
		return
	}
	d.client = c
}

// Read implements datasource.DataSource.
func (d *DataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if err := d.init(ctx); err != nil {
		resp.Diagnostics.AddError("Invalid generated schema", err.Error())
		return
	}
	if d.client == nil {
		resp.Diagnostics.AddError("Provider not configured", "The sfcompute provider has not been configured.")
		return
	}
	cfg, err := objectAttrs(req.Config.Raw)
	if err != nil {
		resp.Diagnostics.AddError("Reading configuration", err.Error())
		return
	}
	typeName := "data.sfcompute_" + d.def.Name

	params := map[string]string{}
	for _, p := range d.def.Read.PathParams {
		v := cfg[p.Attr]
		if v.IsNull() || !v.IsKnown() {
			resp.Diagnostics.AddError("Reading "+typeName, fmt.Sprintf("%q must be set", p.Attr))
			return
		}
		s, err := scalarString(v)
		if err != nil {
			resp.Diagnostics.AddError("Reading "+typeName, err.Error())
			return
		}
		params[p.Name] = s
	}
	reqPath, err := ExpandPath(d.def.Read.Path, params)
	if err != nil {
		resp.Diagnostics.AddError("Reading "+typeName, err.Error())
		return
	}

	query := url.Values{}
	userPaging := false
	usedWorkspace := ""
	for _, p := range d.def.Read.Query {
		v := cfg[p.Attr]
		if v.IsNull() || !v.IsKnown() {
			if p.Attr == d.def.WorkspaceAttr && d.client.DefaultWorkspace != "" {
				query.Set(p.Name, d.client.DefaultWorkspace)
				usedWorkspace = d.client.DefaultWorkspace
			}
			continue
		}
		switch p.Name {
		case "limit", "starting_after", "ending_before":
			userPaging = true
		}
		if p.Array {
			var elems []tftypes.Value
			if err := v.As(&elems); err != nil {
				resp.Diagnostics.AddError("Reading "+typeName, err.Error())
				return
			}
			for _, e := range elems {
				s, err := scalarString(e)
				if err != nil {
					resp.Diagnostics.AddError("Reading "+typeName, err.Error())
					return
				}
				query.Add(p.Name, s)
			}
			continue
		}
		s, err := scalarString(v)
		if err != nil {
			resp.Diagnostics.AddError("Reading "+typeName, err.Error())
			return
		}
		query.Set(p.Name, s)
	}

	out, err := d.client.Do(ctx, Request{Method: d.def.Read.Method, Path: reqPath, Query: query})
	if err != nil {
		resp.Diagnostics.AddError("Reading "+typeName, err.Error())
		return
	}
	obj, ok := out.(map[string]any)
	if !ok {
		resp.Diagnostics.AddError("Reading "+typeName, fmt.Sprintf("expected a JSON object, got %s", jsonKind(out)))
		return
	}
	if d.def.Paginate && !userPaging {
		pages := 1
		data, _ := obj["data"].([]any)
		for {
			more, _ := obj["has_more"].(bool)
			cursor, _ := obj["cursor"].(string)
			if !more || cursor == "" {
				break
			}
			if pages >= maxPages {
				resp.Diagnostics.AddWarning("Reading "+typeName, fmt.Sprintf("stopped after %d pages; results are incomplete", maxPages))
				break
			}
			query.Set("starting_after", cursor)
			next, err := d.client.Do(ctx, Request{Method: d.def.Read.Method, Path: reqPath, Query: query})
			if err != nil {
				resp.Diagnostics.AddError("Reading "+typeName, err.Error())
				return
			}
			if obj, ok = next.(map[string]any); !ok {
				resp.Diagnostics.AddError("Reading "+typeName, fmt.Sprintf("expected a JSON object, got %s", jsonKind(next)))
				return
			}
			page, _ := obj["data"].([]any)
			data = append(data, page...)
			pages++
		}
		obj["data"] = data
	}

	dec := &decoder{names: d.names}
	state := make(map[string]tftypes.Value, len(d.objType.AttributeTypes))
	for _, name := range sortedAttrNames(d.objType) {
		at := d.objType.AttributeTypes[name]
		if v, ok := cfg[name]; ok && !v.IsNull() && v.IsKnown() {
			state[name] = v // configured arguments keep their value
			continue
		}
		if name == d.def.WorkspaceAttr && usedWorkspace != "" {
			if _, inResponse := obj[d.names.json(name)]; !inResponse {
				state[name] = tftypes.NewValue(tftypes.String, usedWorkspace)
				continue
			}
		}
		state[name] = dec.value(at, obj[d.names.json(name)], name)
	}
	for _, w := range dec.warnings {
		tflog.Warn(ctx, "SF Compute API response did not match the schema", map[string]any{"data_source": typeName, "detail": w})
	}
	resp.State.Raw = tftypes.NewValue(d.objType, state)
}
