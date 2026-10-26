package codegen

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Plan is everything later generation steps (and the runtime registry) need
// to know about each generated resource and data source. It is computed from
// the prepared spec and generator_config.yml by Resolve.
type Plan struct {
	Provider    string           `json:"provider"`
	Resources   []ResourcePlan   `json:"resources"`
	DataSources []DataSourcePlan `json:"data_sources"`
}

// OpPlan describes one HTTP operation.
type OpPlan struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	// PathParams maps each {param} in Path to the attribute that supplies it.
	PathParams []ParamPlan `json:"path_params,omitempty"`
	// Query lists query parameters (data sources only).
	Query []ParamPlan `json:"query,omitempty"`
	// Body lists the top-level JSON properties of the request body, or is
	// empty when the operation takes no body.
	Body []string `json:"body,omitempty"`
	// IdempotencyHeader names a header that makes retries of this
	// operation safe (e.g. Idempotency-Key).
	IdempotencyHeader string `json:"idempotency_header,omitempty"`
}

// ParamPlan maps an API parameter to a schema attribute.
type ParamPlan struct {
	Name     string `json:"name"`
	Attr     string `json:"attr"`
	Array    bool   `json:"array,omitempty"`
	Required bool   `json:"required,omitempty"`
}

// ResourcePlan describes one generated resource.
type ResourcePlan struct {
	Name         string   `json:"name"`
	Derived      bool     `json:"derived"`
	Create       OpPlan   `json:"create"`
	Read         OpPlan   `json:"read"`
	Update       *OpPlan  `json:"update,omitempty"`
	Delete       OpPlan   `json:"delete"`
	BeforeDelete []OpPlan `json:"before_delete,omitempty"`
	DeleteAccept []int    `json:"delete_accept,omitempty"`
	Spending     bool     `json:"spending"`
	SpendingNote string   `json:"spending_note,omitempty"`
	Stable       []string `json:"stable"`
	// ShapeMismatch lists create-body fields whose read response
	// representation differs from the request one (e.g. an ID string in,
	// a {id, name} object out), or that the read response omits.
	ShapeMismatch []string `json:"shape_mismatch,omitempty"`
	// WorkspaceAttr is the attribute that defaults to the provider's
	// workspace, if any.
	WorkspaceAttr string `json:"workspace_attr,omitempty"`
	// Renames maps attribute names to JSON property names where they
	// differ.
	Renames map[string]string `json:"renames,omitempty"`
	// Ignores are attributes dropped from the schema (read query params).
	Ignores []string `json:"ignores,omitempty"`
}

// DataSourcePlan describes one generated data source.
type DataSourcePlan struct {
	Name    string `json:"name"`
	Derived bool   `json:"derived"`
	Read    OpPlan `json:"read"`
	// List is set when the response is a {data: [...], has_more} page.
	List bool `json:"list,omitempty"`
	// Paginate is set when pages chain through a `cursor` response field
	// and a `starting_after` query parameter.
	Paginate      bool              `json:"paginate,omitempty"`
	WorkspaceAttr string            `json:"workspace_attr,omitempty"`
	Renames       map[string]string `json:"renames,omitempty"`
}

// TFGenConfig is the config file handed to tfplugingen-openapi.
type TFGenConfig struct {
	Provider    ProviderConfig                 `yaml:"provider"`
	Resources   map[string]TFGenResource       `yaml:"resources,omitempty"`
	DataSources map[string]TFGenDataSourceConf `yaml:"data_sources,omitempty"`
}

// TFGenResource is a resource entry in tfplugingen-openapi's format.
type TFGenResource struct {
	Create *Location      `yaml:"create"`
	Read   *Location      `yaml:"read"`
	Update *Location      `yaml:"update,omitempty"`
	Delete *Location      `yaml:"delete,omitempty"`
	Schema *SchemaOptions `yaml:"schema,omitempty"`
}

// TFGenDataSourceConf is a data source entry in tfplugingen-openapi's format.
type TFGenDataSourceConf struct {
	Read   *Location      `yaml:"read"`
	Schema *SchemaOptions `yaml:"schema,omitempty"`
}

// Resolution is the output of Resolve.
type Resolution struct {
	Plan     *Plan
	TFGen    *TFGenConfig
	Coverage string
	Warnings []string
}

type derivedResource struct {
	cfg    ResourceConfig
	prefix string
}

var (
	segRE   = `[a-z0-9_]+`
	paramRE = regexp.MustCompile(`\{([^}]+)\}`)
)

// Resolve derives entries from path conventions, merges them with the
// explicit entries in cfg, validates everything against the spec, and
// returns the plan, the tfplugingen-openapi config and a coverage report.
func Resolve(spec *Spec, cfg *Config) (*Resolution, error) {
	res := &Resolution{}
	var errs []string
	fail := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }
	warn := func(format string, args ...any) { res.Warnings = append(res.Warnings, fmt.Sprintf(format, args...)) }

	derivedRes := map[string]derivedResource{}
	derivedDS := map[string]derivedResource{} // only cfg.Read used
	superseded := map[string]string{}         // "METHOD path" -> note

	for _, prefix := range cfg.Derive.Prefixes {
		collRE := regexp.MustCompile("^" + regexp.QuoteMeta(prefix) + "/(" + segRE + ")$")
		for _, path := range spec.Paths() {
			m := collRE.FindStringSubmatch(path)
			if m == nil {
				continue
			}
			seg := m[1]
			item := findItemPath(spec, path)
			has := func(method, p string) bool { _, ok := spec.Operation(method, p); return p != "" && ok }

			if cfg.Derive.Resources && has("post", path) && item != "" && has("get", item) && has("delete", item) && spec.RequestSchema("post", path) != nil {
				name := singular(seg)
				r := ResourceConfig{
					Create: &Location{Path: path, Method: "POST"},
					Read:   &Location{Path: item, Method: "GET"},
					Delete: &Location{Path: item, Method: "DELETE"},
				}
				switch {
				case has("patch", item):
					r.Update = &Location{Path: item, Method: "PATCH"}
					if has("put", item) {
						warn("resource %s: both PATCH and PUT %s exist; using PATCH", name, item)
					}
				case has("put", item):
					r.Update = &Location{Path: item, Method: "PUT"}
				}
				if prev, ok := derivedRes[name]; ok {
					superseded["POST "+path] = fmt.Sprintf("superseded by %s (resource %s)", prev.cfg.Create.Path, name)
				} else {
					derivedRes[name] = derivedResource{cfg: r, prefix: prefix}
				}
			}
			if cfg.Derive.DataSources {
				if item != "" && has("get", item) {
					name := singular(seg)
					if prev, ok := derivedDS[name]; ok {
						superseded["GET "+item] = fmt.Sprintf("superseded by %s (data source %s)", prev.cfg.Read.Path, name)
					} else {
						derivedDS[name] = derivedResource{cfg: ResourceConfig{Read: &Location{Path: item, Method: "GET"}}, prefix: prefix}
					}
				}
				if has("get", path) && spec.ResponseSchema("get", path) != nil {
					name := seg
					if prev, ok := derivedDS[name]; ok {
						superseded["GET "+path] = fmt.Sprintf("superseded by %s (data source %s)", prev.cfg.Read.Path, name)
					} else {
						derivedDS[name] = derivedResource{cfg: ResourceConfig{Read: &Location{Path: path, Method: "GET"}}, prefix: prefix}
					}
				}
			}
		}
	}

	// Merge explicit resources over derived ones.
	resources := map[string]ResourceConfig{}
	derivedName := map[string]bool{}
	for name, d := range derivedRes {
		if _, skip := cfg.Skip.Resources[name]; skip {
			continue
		}
		resources[name] = d.cfg
		derivedName["r:"+name] = true
	}
	for name := range cfg.Skip.Resources {
		if _, ok := cfg.Resources[name]; ok {
			fail("resource %s is both configured and skipped", name)
		}
		if _, ok := derivedRes[name]; !ok {
			warn("skip.resources.%s does not match any derived resource", name)
		}
	}
	for name, ex := range cfg.Resources {
		base, derived := resources[name]
		if !derived && ex.Create == nil {
			fail("resource %s is not derivable from path conventions, so it needs explicit create/read/delete operations", name)
			continue
		}
		merged := base
		if ex.Create != nil {
			merged.Create = ex.Create
		}
		if ex.Read != nil {
			merged.Read = ex.Read
		}
		if ex.Update != nil {
			merged.Update = ex.Update
		}
		if ex.Delete != nil {
			merged.Delete = ex.Delete
		}
		merged.Schema = ex.Schema
		merged.Spending = ex.Spending
		merged.SpendingNote = ex.SpendingNote
		merged.BeforeDelete = ex.BeforeDelete
		merged.DeleteAcceptStatus = ex.DeleteAcceptStatus
		merged.Stable = ex.Stable
		resources[name] = merged
	}

	dataSources := map[string]DataSourceConfig{}
	for name, d := range derivedDS {
		if _, skip := cfg.Skip.DataSources[name]; skip {
			continue
		}
		dataSources[name] = DataSourceConfig{Read: d.cfg.Read}
		derivedName["d:"+name] = true
	}
	for name := range cfg.Skip.DataSources {
		if _, ok := cfg.DataSources[name]; ok {
			fail("data source %s is both configured and skipped", name)
		}
		if _, ok := derivedDS[name]; !ok {
			warn("skip.data_sources.%s does not match any derived data source", name)
		}
	}
	for name, ex := range cfg.DataSources {
		base, derived := dataSources[name]
		if !derived && ex.Read == nil {
			fail("data source %s is not derivable from path conventions, so it needs an explicit read operation", name)
			continue
		}
		if ex.Read != nil {
			base.Read = ex.Read
		}
		base.Schema = ex.Schema
		dataSources[name] = base
	}

	plan := &Plan{Provider: cfg.Provider.Name}
	tfgen := &TFGenConfig{Provider: cfg.Provider, Resources: map[string]TFGenResource{}, DataSources: map[string]TFGenDataSourceConf{}}
	used := map[string][]string{} // "METHOD path" -> users

	for _, name := range sortedKeys(resources) {
		rc := resources[name]
		rp, ignores, err := planResource(spec, name, rc)
		if err != nil {
			fail("resource %s: %v", name, err)
			continue
		}
		rp.Derived = derivedName["r:"+name]
		plan.Resources = append(plan.Resources, *rp)
		schema := rc.Schema
		if len(ignores) > 0 {
			if schema == nil {
				schema = &SchemaOptions{}
			} else {
				cp := *schema
				schema = &cp
			}
			schema.Ignores = append(append([]string{}, schema.Ignores...), ignores...)
		}
		tfgen.Resources[name] = TFGenResource{Create: rc.Create, Read: rc.Read, Update: rc.Update, Delete: rc.Delete, Schema: schema}
		label := "resource sfcompute_" + name
		used[rc.Create.String()] = append(used[rc.Create.String()], label+" (create)")
		used[rc.Read.String()] = append(used[rc.Read.String()], label+" (read)")
		if rc.Update != nil {
			used[rc.Update.String()] = append(used[rc.Update.String()], label+" (update)")
		}
		used[rc.Delete.String()] = append(used[rc.Delete.String()], label+" (delete)")
		for i := range rc.BeforeDelete {
			l := rc.BeforeDelete[i]
			used[l.String()] = append(used[l.String()], label+" (before delete)")
		}
	}
	for _, name := range sortedKeys(dataSources) {
		dc := dataSources[name]
		dp, err := planDataSource(spec, name, dc)
		if err != nil {
			fail("data source %s: %v", name, err)
			continue
		}
		dp.Derived = derivedName["d:"+name]
		plan.DataSources = append(plan.DataSources, *dp)
		tfgen.DataSources[name] = TFGenDataSourceConf{Read: dc.Read, Schema: dc.Schema}
		used[dc.Read.String()] = append(used[dc.Read.String()], "data source sfcompute_"+name)
	}

	if len(errs) > 0 {
		sort.Strings(errs)
		return nil, fmt.Errorf("%d problem(s) in generator config:\n  %s", len(errs), strings.Join(errs, "\n  "))
	}
	res.Plan = plan
	res.TFGen = tfgen
	res.Coverage = coverage(spec, cfg, used, superseded)
	sort.Strings(res.Warnings)
	return res, nil
}

// findItemPath returns the single "<collection>/{param}" path, if exactly
// one exists.
func findItemPath(spec *Spec, collection string) string {
	re := regexp.MustCompile("^" + regexp.QuoteMeta(collection) + `/\{[^}/]+\}$`)
	var found []string
	for _, p := range spec.Paths() {
		if re.MatchString(p) {
			found = append(found, p)
		}
	}
	if len(found) == 1 {
		return found[0]
	}
	return ""
}

func planOp(spec *Spec, loc *Location, aliases map[string]string, withBody bool) (OpPlan, error) {
	if loc == nil {
		return OpPlan{}, fmt.Errorf("missing operation")
	}
	method := strings.ToLower(loc.Method)
	if _, ok := spec.Operation(method, loc.Path); !ok {
		return OpPlan{}, fmt.Errorf("%s is not in the spec", loc)
	}
	op := OpPlan{Method: strings.ToUpper(method), Path: loc.Path}
	params := spec.Params(method, loc.Path)
	for _, m := range paramRE.FindAllStringSubmatch(loc.Path, -1) {
		name := m[1]
		attr := name
		if a, ok := aliases[name]; ok {
			attr = a
		}
		op.PathParams = append(op.PathParams, ParamPlan{Name: name, Attr: TerraformIdentifier(attr), Required: true})
	}
	for _, p := range params {
		if p.In == "header" && strings.EqualFold(p.Name, "Idempotency-Key") {
			op.IdempotencyHeader = p.Name
		}
	}
	if withBody {
		if rs := spec.RequestSchema(method, loc.Path); rs != nil {
			op.Body = sortedKeys(properties(rs))
		}
	}
	return op, nil
}

func planResource(spec *Spec, name string, rc ResourceConfig) (*ResourcePlan, []string, error) {
	var aliases map[string]string
	if rc.Schema != nil {
		aliases = rc.Schema.Attributes.Aliases
	}
	rp := &ResourcePlan{Name: name}
	var err error
	if rp.Create, err = planOp(spec, rc.Create, aliases, true); err != nil {
		return nil, nil, fmt.Errorf("create: %w", err)
	}
	if spec.RequestSchema(rc.Create.Method, rc.Create.Path) == nil {
		return nil, nil, fmt.Errorf("create: %s has no JSON request body", rc.Create)
	}
	if rp.Read, err = planOp(spec, rc.Read, aliases, false); err != nil {
		return nil, nil, fmt.Errorf("read: %w", err)
	}
	if rc.Update != nil {
		u, err := planOp(spec, rc.Update, aliases, true)
		if err != nil {
			return nil, nil, fmt.Errorf("update: %w", err)
		}
		rp.Update = &u
	}
	if rp.Delete, err = planOp(spec, rc.Delete, aliases, false); err != nil {
		return nil, nil, fmt.Errorf("delete: %w", err)
	}
	for i := range rc.BeforeDelete {
		op, err := planOp(spec, &rc.BeforeDelete[i], aliases, false)
		if err != nil {
			return nil, nil, fmt.Errorf("before_delete: %w", err)
		}
		rp.BeforeDelete = append(rp.BeforeDelete, op)
	}
	for _, st := range rc.DeleteAcceptStatus {
		if st < 400 || st > 499 {
			return nil, nil, fmt.Errorf("delete_accept_status: %d is not a 4xx status", st)
		}
	}
	rp.DeleteAccept = rc.DeleteAcceptStatus
	rp.Spending = rc.Spending == nil || *rc.Spending
	rp.SpendingNote = rc.SpendingNote
	if rp.Spending && rp.SpendingNote == "" {
		rp.SpendingNote = "it has not been classified as free to create (set spending: false in generator_config.yml once verified)"
	}
	rp.Stable = rc.Stable
	if len(rp.Stable) == 0 {
		rp.Stable = []string{"id", "created_at"}
	}

	createReq := spec.RequestSchema(rc.Create.Method, rc.Create.Path)
	readResp := spec.ResponseSchema(rc.Read.Method, rc.Read.Path)
	if readResp == nil {
		return nil, nil, fmt.Errorf("read: %s has no JSON response", rc.Read)
	}
	reqProps, respProps := properties(createReq), properties(readResp)
	for _, f := range rp.Create.Body {
		rq, _ := reqProps[f].(map[string]any)
		rs, _ := respProps[f].(map[string]any)
		if rs == nil || shape(rq) != shape(rs) {
			rp.ShapeMismatch = append(rp.ShapeMismatch, TerraformIdentifier(f))
		}
	}
	if _, ok := reqProps["workspace"]; ok {
		rp.WorkspaceAttr = "workspace"
	}

	// Read query parameters describe how to read, not the resource; keep
	// them out of the resource schema.
	var ignores []string
	for _, p := range spec.Params(rc.Read.Method, rc.Read.Path) {
		if p.In == "query" {
			if _, inBody := reqProps[p.Name]; !inBody {
				ignores = append(ignores, TerraformIdentifier(p.Name))
			}
		}
	}
	sort.Strings(ignores)
	rp.Ignores = ignores

	renames := map[string]string{}
	for _, s := range []map[string]any{createReq, spec.ResponseSchema(rc.Create.Method, rc.Create.Path), readResp} {
		if err := collectRenames(s, renames); err != nil {
			return nil, nil, err
		}
	}
	if rc.Update != nil {
		if err := collectRenames(spec.RequestSchema(rc.Update.Method, rc.Update.Path), renames); err != nil {
			return nil, nil, err
		}
	}
	if len(renames) > 0 {
		rp.Renames = renames
	}
	return rp, ignores, nil
}

func planDataSource(spec *Spec, name string, dc DataSourceConfig) (*DataSourcePlan, error) {
	var aliases map[string]string
	if dc.Schema != nil {
		aliases = dc.Schema.Attributes.Aliases
	}
	op, err := planOp(spec, dc.Read, aliases, false)
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if strings.ToUpper(dc.Read.Method) != "GET" {
		return nil, fmt.Errorf("read: only GET operations can back a data source (got %s)", dc.Read)
	}
	for _, p := range spec.Params(dc.Read.Method, dc.Read.Path) {
		if p.In != "query" {
			continue
		}
		attr := p.Name
		if a, ok := aliases[p.Name]; ok {
			attr = a
		}
		op.Query = append(op.Query, ParamPlan{
			Name:     p.Name,
			Attr:     TerraformIdentifier(attr),
			Array:    p.Schema["type"] == "array",
			Required: p.Required,
		})
	}
	dp := &DataSourcePlan{Name: name, Read: op}
	resp := spec.ResponseSchema(dc.Read.Method, dc.Read.Path)
	if resp == nil {
		return nil, fmt.Errorf("read: %s has no JSON response", dc.Read)
	}
	props := properties(resp)
	if d, ok := props["data"].(map[string]any); ok && d["type"] == "array" {
		if _, ok := props["has_more"]; ok {
			dp.List = true
			_, hasCursor := props["cursor"]
			for _, q := range op.Query {
				if q.Name == "starting_after" && hasCursor {
					dp.Paginate = true
				}
			}
		}
	}
	for _, q := range op.Query {
		if q.Name == "workspace" {
			dp.WorkspaceAttr = q.Attr
		}
	}
	renames := map[string]string{}
	if err := collectRenames(resp, renames); err != nil {
		return nil, err
	}
	for _, q := range op.Query {
		if TerraformIdentifier(q.Name) != q.Name {
			renames[TerraformIdentifier(q.Name)] = q.Name
		}
	}
	if len(renames) > 0 {
		dp.Renames = renames
	}
	return dp, nil
}

func coverage(spec *Spec, cfg *Config, used map[string][]string, superseded map[string]string) string {
	var sb strings.Builder
	sb.WriteString("# Generated by tools/sfcgen from spec/openapi.json and generator_config.yml; DO NOT EDIT.\n")
	sb.WriteString("# Every operation in the spec and what the provider does with it.\n\n")
	width := 0
	type row struct{ op, what string }
	var rows []row
	for _, p := range spec.Paths() {
		for _, m := range spec.Methods(p) {
			key := strings.ToUpper(m) + " " + p
			what := ""
			switch {
			case len(used[key]) > 0:
				what = strings.Join(used[key], "; ")
			case superseded[key] != "":
				what = "not generated: " + superseded[key]
			default:
				what = "not generated: " + skipReason(spec, cfg, m, p)
			}
			rows = append(rows, row{key, what})
			if len(key) > width {
				width = len(key)
			}
		}
	}
	for _, r := range rows {
		fmt.Fprintf(&sb, "%-*s  %s\n", width, r.op, r.what)
	}
	return sb.String()
}

func skipReason(spec *Spec, cfg *Config, method, path string) string {
	for _, prefix := range cfg.Derive.Prefixes {
		re := regexp.MustCompile("^" + regexp.QuoteMeta(prefix) + "/(" + segRE + ")(/\\{[^}]+\\})?$")
		m := re.FindStringSubmatch(path)
		if m == nil {
			continue
		}
		seg := m[1]
		if r, ok := cfg.Skip.Resources[singular(seg)]; ok && (method != "get") {
			return "resource " + singular(seg) + " skipped: " + firstSentence(r)
		}
		if method == "get" {
			n := seg
			if m[2] != "" {
				n = singular(seg)
			}
			if r, ok := cfg.Skip.DataSources[n]; ok {
				return "data source " + n + " skipped: " + firstSentence(r)
			}
		}
		if r, ok := cfg.Skip.Resources[singular(seg)]; ok {
			return "resource " + singular(seg) + " skipped: " + firstSentence(r)
		}
	}
	if method != "get" {
		return "action endpoint (not a CRUD operation of any configured resource)"
	}
	return "no path convention matches; add an explicit data_sources entry to expose it"
}

func firstSentence(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if i := strings.Index(s, ". "); i >= 0 {
		return s[:i+1]
	}
	return s
}
