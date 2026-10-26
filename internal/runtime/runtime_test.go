package runtime_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/hofvarpnir-studios/terraform-provider-sfcompute/internal/runtime"
)

func TestWorkspaceLifecycle(t *testing.T) {
	h := newHarness(t)
	ws := h.resource("workspace")

	ws.apply(map[string]any{"name": "research"})
	id := ws.get("id").(string)
	if !strings.HasPrefix(id, "wksp_") || ws.get("resource_path") != "sfc:workspace:acme:research" || ws.get("created_at") == nil {
		t.Fatalf("unexpected state after create: id=%v path=%v", id, ws.get("resource_path"))
	}

	// Rename in place: PATCH, and the computed resource path follows.
	pr, _ := ws.plan(map[string]any{"name": "research-2"})
	noErrors(t, "plan", pr.Diagnostics)
	if len(pr.RequiresReplace) != 0 {
		t.Fatalf("rename should update in place, got replace %v", pr.RequiresReplace)
	}
	ws.apply(map[string]any{"name": "research-2"})
	if ws.get("id") != id || ws.get("resource_path") != "sfc:workspace:acme:research-2" {
		t.Fatalf("unexpected state after update: %v %v", ws.get("id"), ws.get("resource_path"))
	}
	if h.srv.CountRequests("PATCH", "/v2/workspaces/{id}") != 1 {
		t.Error("expected exactly one PATCH")
	}

	// Drift on an updatable attribute is detected on refresh.
	h.srv.Mutate("workspace", id, func(o map[string]any) { o["name"] = "renamed-in-console" })
	ws.refresh()
	if ws.get("name") != "renamed-in-console" {
		t.Errorf("drift not detected: name=%v", ws.get("name"))
	}

	noErrors(t, "delete", ws.destroy())
	if h.srv.Object("workspace", id) != nil {
		t.Error("workspace not deleted")
	}
}

func TestReadRemovesMissingResource(t *testing.T) {
	h := newHarness(t)
	ws := h.resource("workspace")
	ws.apply(map[string]any{"name": "gone"})
	id := ws.get("id").(string)
	h.srv.Mutate("workspace", id, func(map[string]any) {})
	// Delete out of band.
	saved := ws.state
	noErrors(t, "delete", ws.destroy())
	ws.state = saved
	if ws.refresh() {
		t.Error("a resource that returns 404 must be removed from state")
	}
}

func TestPoolDefaultsWorkspaceFromProvider(t *testing.T) {
	h := newHarness(t)
	ws := h.resource("workspace")
	ws.apply(map[string]any{"name": "prod"})
	h.client.DefaultWorkspace = "sfc:workspace:acme:prod"

	pool := h.resource("pool")
	pr, planned := pool.plan(map[string]any{"name": "train", "tags": map[string]any{"team": "ml"}})
	noErrors(t, "plan", pr.Diagnostics)
	pm := goValueMap(t, planned)
	if pm["workspace"] != "sfc:workspace:acme:prod" {
		t.Fatalf("workspace not defaulted in plan: %v", pm["workspace"])
	}
	pool.applyPlanned(map[string]any{"name": "train", "tags": map[string]any{"team": "ml"}}, planned)
	if pool.get("workspace") != "sfc:workspace:acme:prod" {
		t.Errorf("state must keep the planned workspace, got %v", pool.get("workspace"))
	}
	// The API echoes workspace as {id, name}; refresh must not turn that
	// into drift (workspace is create-only).
	pool.refresh()
	if pool.get("workspace") != "sfc:workspace:acme:prod" {
		t.Errorf("refresh replaced the configured workspace with %v", pool.get("workspace"))
	}
	if sched, ok := pool.get("allocation_schedule").(map[string]any); !ok || sched["total"] == nil {
		t.Errorf("nested computed object not populated: %v", pool.get("allocation_schedule"))
	}

	// Unset workspace on update keeps the prior value; changing tags is an
	// in-place PATCH; changing the provider default does not move it.
	h.client.DefaultWorkspace = "sfc:workspace:acme:other"
	pr, _ = pool.plan(map[string]any{"name": "train", "tags": map[string]any{"team": "infra"}})
	noErrors(t, "plan", pr.Diagnostics)
	if len(pr.RequiresReplace) != 0 {
		t.Fatalf("unexpected replace: %v", pr.RequiresReplace)
	}
	pool.apply(map[string]any{"name": "train", "tags": map[string]any{"team": "infra"}})
	if tags := pool.get("tags").(map[string]any); tags["team"] != "infra" {
		t.Errorf("tags not updated: %v", tags)
	}

	// Changing the workspace explicitly forces replacement.
	pr, _ = pool.plan(map[string]any{"name": "train", "workspace": "sfc:workspace:acme:other"})
	if !reflect.DeepEqual(pr.RequiresReplace, path.Paths{path.Root("workspace")}) {
		t.Errorf("RequiresReplace = %v, want [workspace]", pr.RequiresReplace)
	}
}

func goValueMap(t *testing.T, v tftypes.Value) map[string]any {
	return goValue(t, v).(map[string]any)
}

func TestMissingWorkspaceIsAPlanError(t *testing.T) {
	h := newHarness(t)
	pool := h.resource("pool")
	pr, _ := pool.plan(map[string]any{"name": "x"})
	if !hasError(pr.Diagnostics, "needs a workspace") {
		t.Errorf("expected a missing-workspace error, got %v", pr.Diagnostics)
	}
}

func TestFirewallNestedComputedAndPut(t *testing.T) {
	h := newHarness(t)
	h.resource("workspace").apply(map[string]any{"name": "net"})
	fw := h.resource("firewall")
	rule := func(port string) map[string]any {
		return map[string]any{"direction": "ingress", "protocol": "tcp", "port": port, "source": unknown}
	}
	cfgRule := func(port string) map[string]any {
		return map[string]any{"direction": "ingress", "protocol": "tcp", "port": port}
	}
	cfg := map[string]any{"name": "web", "workspace": "sfc:workspace:acme:net", "rules": []any{cfgRule("443")}}
	proposed := fw.proposed(cfg)
	// Terraform marks the unset optional+computed nested attribute unknown.
	proposed = tfv(t, fw.typ, mergeMaps(goValueMap(t, proposed), map[string]any{"rules": []any{rule("443")}}))
	pr, planned := fw.planWith(cfg, proposed)
	noErrors(t, "plan", pr.Diagnostics)
	noErrors(t, "create", fw.applyPlanned(cfg, planned))
	rules := fw.get("rules").([]any)
	if src := rules[0].(map[string]any)["source"]; src != "0.0.0.0/0" {
		t.Errorf("nested computed source not filled from response: %v", src)
	}
	// API returns workspace as its *name*; state must keep the resource path.
	fw.refresh()
	if fw.get("workspace") != "sfc:workspace:acme:net" {
		t.Errorf("workspace overwritten by API representation: %v", fw.get("workspace"))
	}

	cfg2 := map[string]any{"name": "web", "workspace": "sfc:workspace:acme:net", "rules": []any{
		map[string]any{"direction": "ingress", "protocol": "tcp", "port": "443", "source": "0.0.0.0/0"},
		map[string]any{"direction": "ingress", "protocol": "udp", "port": "51820", "source": "203.0.113.0/24"},
	}}
	pr, _ = fw.plan(cfg2)
	noErrors(t, "plan", pr.Diagnostics)
	if len(pr.RequiresReplace) != 0 {
		t.Fatalf("rules are updatable via PUT, got replace %v", pr.RequiresReplace)
	}
	fw.apply(cfg2)
	if n := len(fw.get("rules").([]any)); n != 2 {
		t.Errorf("want 2 rules after PUT, got %d", n)
	}
	last := h.srv.Requests()[len(h.srv.Requests())-1]
	if last.Method != "PUT" || len(last.Body) != 1 || last.Body["rules"] == nil {
		t.Errorf("PUT should send only the updatable body fields, got %s %v", last.Method, last.Body)
	}
	// name is create-only (not in the PUT body).
	pr, _ = fw.plan(map[string]any{"name": "web2", "workspace": "sfc:workspace:acme:net", "rules": cfg2["rules"]})
	if !reflect.DeepEqual(pr.RequiresReplace, path.Paths{path.Root("name")}) {
		t.Errorf("RequiresReplace = %v, want [name]", pr.RequiresReplace)
	}
}

func mergeMaps(a, b map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func TestSpendingGuardBlocksOrders(t *testing.T) {
	h := newHarness(t)
	h.resource("workspace").apply(map[string]any{"name": "buy"})
	pool := h.resource("pool")
	pool.apply(map[string]any{"name": "p", "workspace": "sfc:workspace:acme:buy"})

	cfg := map[string]any{
		"pool": pool.get("id"), "side": "buy", "sku": "sku_mockh100a",
		"allow_standing": true, "allow_partial": false,
		"limit_price_dollars_per_node_hour": "12.50",
		"allocation_schedule_delta":         []any{map[string]any{"start_at": 1_700_003_600, "end_at": 1_700_007_200, "node_count": 1}},
	}
	order := h.resource("order")
	pr, planned := order.plan(cfg)
	if !hasError(pr.Diagnostics, "allow_spending") {
		t.Fatalf("plan must fail without allow_spending, got %v", pr.Diagnostics)
	}
	// Even if a plan slipped through, apply refuses too.
	if d := order.applyPlanned(cfg, planned); !hasError(d, "allow_spending") {
		t.Fatalf("create must fail without allow_spending, got %v", d)
	}
	if n := h.srv.CountRequests("POST", "/v2/orders"); n != 0 {
		t.Fatalf("the API received %d order request(s) despite the guard", n)
	}

	// Instances are guarded too.
	ipr, _ := h.resource("instance").plan(map[string]any{"pool": pool.get("id"), "image": "image_ubuntu2404", "sku": "sku_mockh100a"})
	if !hasError(ipr.Diagnostics, "allow_spending") {
		t.Errorf("instance plan must fail without allow_spending")
	}
	// Free resources are not.
	wpr, _ := h.resource("workspace").plan(map[string]any{"name": "free"})
	noErrors(t, "workspace plan", wpr.Diagnostics)

	h.client.AllowSpending = true
	order.apply(cfg)
	if order.get("state") != "standing" || !strings.HasPrefix(order.get("id").(string), "ordr_") {
		t.Fatalf("unexpected order state: %v %v", order.get("state"), order.get("id"))
	}
	// Orders cannot be updated: any change is a replacement, which is
	// itself guarded.
	h.client.AllowSpending = false
	pr, _ = order.plan(mergeMaps(cfg, map[string]any{"limit_price_dollars_per_node_hour": "13.00"}))
	if len(pr.RequiresReplace) != 1 || !hasError(pr.Diagnostics, "forces a replacement") {
		t.Errorf("price change should require a guarded replacement: %v / %v", pr.RequiresReplace, pr.Diagnostics)
	}
	// Destroying (cancelling) is always allowed.
	noErrors(t, "cancel", order.destroy())
	if n := h.srv.CountRequests("POST", "/v2/orders/{id}/cancel"); n != 1 {
		t.Errorf("want one cancel, got %d", n)
	}
}

func TestOrderAlreadyFilledIsRemovedWithWarning(t *testing.T) {
	h := newHarness(t)
	h.client.AllowSpending = true
	h.resource("workspace").apply(map[string]any{"name": "buy"})
	pool := h.resource("pool")
	pool.apply(map[string]any{"workspace": "sfc:workspace:acme:buy"})
	order := h.resource("order")
	order.apply(map[string]any{
		"pool": pool.get("id"), "side": "buy", "sku": "sku_mockh100a",
		"allow_standing": false, "allow_partial": false, "limit_price_dollars_per_node_hour": "1.00",
		"allocation_schedule_delta": []any{map[string]any{"start_at": 1, "end_at": 2, "node_count": 1}},
	})
	d := order.destroy() // immediate-or-cancel order is already cancelled: 422
	noErrors(t, "destroy", d)
	if d.WarningsCount() != 1 {
		t.Errorf("expected a warning about the 422, got %v", d)
	}
}

func TestInstanceTerminatesBeforeDelete(t *testing.T) {
	h := newHarness(t)
	h.client.AllowSpending = true
	h.resource("workspace").apply(map[string]any{"name": "gpu"})
	pool := h.resource("pool")
	pool.apply(map[string]any{"workspace": "sfc:workspace:acme:gpu"})
	inst := h.resource("instance")
	inst.apply(map[string]any{"pool": pool.get("id"), "image": "sfc:image:sfcompute:public:ubuntu-24.04-cuda-13.2", "sku": "sku_mockh100a", "name": "trainer", "cloud_init_user_data": "I2Nsb3VkLWNvbmZpZwo="})
	if inst.get("state") != "awaiting_allocation" {
		t.Errorf("state = %v", inst.get("state"))
	}
	inst.refresh()
	if inst.get("cloud_init_user_data") != "I2Nsb3VkLWNvbmZpZwo=" {
		t.Error("write-only cloud_init_user_data lost on refresh")
	}
	if inst.get("image") != "sfc:image:sfcompute:public:ubuntu-24.04-cuda-13.2" {
		t.Errorf("image overwritten by API summary object: %v", inst.get("image"))
	}
	// priority_level is updatable.
	inst.apply(map[string]any{"pool": pool.get("id"), "image": "sfc:image:sfcompute:public:ubuntu-24.04-cuda-13.2", "sku": "sku_mockh100a", "name": "trainer", "cloud_init_user_data": "I2Nsb3VkLWNvbmZpZwo=", "priority_level": "critical"})
	if inst.get("priority_level") != "critical" {
		t.Errorf("priority not updated")
	}
	id := inst.get("id").(string)
	noErrors(t, "delete", inst.destroy())
	if h.srv.Object("instance", id) != nil {
		t.Error("instance not deleted")
	}
	reqs := h.srv.Requests()
	var seq []string
	for _, r := range reqs[len(reqs)-2:] {
		seq = append(seq, r.Method+" "+r.Path)
	}
	if want := []string{"POST /v2/instances/" + id + "/terminate", "DELETE /v2/instances/" + id}; !reflect.DeepEqual(seq, want) {
		t.Errorf("delete sequence = %v, want %v", seq, want)
	}
}

func TestDeleteRetriesConflicts(t *testing.T) {
	h := newHarness(t)
	ws := h.resource("workspace")
	ws.apply(map[string]any{"name": "busy"})
	h.srv.FailNext("DELETE", "/v2/workspaces/{id}", http.StatusConflict)
	noErrors(t, "delete", ws.destroy())
	if n := h.srv.CountRequests("DELETE", "/v2/workspaces/{id}"); n != 2 {
		t.Errorf("want a retry after 409, got %d DELETEs", n)
	}
}

func TestImportReadsEverything(t *testing.T) {
	h := newHarness(t)
	h.resource("workspace").apply(map[string]any{"name": "imp"})
	src := h.resource("pool")
	src.apply(map[string]any{"name": "imported", "workspace": "sfc:workspace:acme:imp", "tags": map[string]any{"a": "b"}})

	imp := h.resource("pool")
	imp.state = imp.config(map[string]any{"id": src.get("id")})
	imp.refresh()
	if imp.get("name") != "imported" || imp.get("resource_path") != "sfc:pool:acme:imp:imported" {
		t.Errorf("import did not populate state: %v %v", imp.get("name"), imp.get("resource_path"))
	}
	if ws, _ := imp.get("workspace").(string); !strings.HasPrefix(ws, "wksp_") {
		t.Errorf("imported workspace should fall back to the ID from the API's summary object, got %v", imp.get("workspace"))
	}
}

func TestListDataSourcePaginates(t *testing.T) {
	h := newHarness(t)
	h.resource("workspace").apply(map[string]any{"name": "many"})
	h.resource("workspace").apply(map[string]any{"name": "other"})
	for _, n := range []string{"a", "b", "c"} {
		h.resource("pool").apply(map[string]any{"name": n, "workspace": "sfc:workspace:acme:many"})
	}
	h.resource("pool").apply(map[string]any{"name": "elsewhere", "workspace": "sfc:workspace:acme:other"})

	h.client.DefaultWorkspace = "sfc:workspace:acme:many"
	before := h.srv.CountRequests("GET", "/v2/pools")
	got, d := h.readDataSource("pools", map[string]any{})
	noErrors(t, "read", d)
	data := got["data"].([]any)
	if len(data) != 3 {
		t.Fatalf("want 3 pools across pages, got %d", len(data))
	}
	if got["workspace"] != "sfc:workspace:acme:many" {
		t.Errorf("workspace = %v, want provider default", got["workspace"])
	}
	if calls := h.srv.CountRequests("GET", "/v2/pools") - before; calls != 2 {
		t.Errorf("want 2 page requests (page size 2), got %d", calls)
	}
	// An explicit limit returns one page.
	got, d = h.readDataSource("pools", map[string]any{"limit": 1})
	noErrors(t, "read", d)
	if len(got["data"].([]any)) != 1 || got["has_more"] != true {
		t.Errorf("explicit limit should return a single page: %v", got)
	}
	// Explicit filter by IDs (array query parameter).
	first := data[0].(map[string]any)["id"].(string)
	got, d = h.readDataSource("pools", map[string]any{"id": []any{first}})
	noErrors(t, "read", d)
	if q := h.srv.Requests()[len(h.srv.Requests())-1].Query; !strings.Contains(q, "id="+first) {
		t.Errorf("array query parameter not sent: %q", q)
	}
	_ = got
}

func TestSingletonAndItemDataSources(t *testing.T) {
	h := newHarness(t)
	acct, d := h.readDataSource("account", map[string]any{})
	noErrors(t, "account", d)
	if acct["account_id"] != "acme" || acct["entity_type"] != "business" {
		t.Errorf("unexpected account: %v", acct)
	}
	sku, d := h.readDataSource("sku", map[string]any{"id": "sku_mockh100a"})
	noErrors(t, "sku", d)
	if sku["alias"] != "h100-sea" {
		t.Errorf("unexpected sku: %v", sku)
	}
	if acc := sku["accelerator"].(map[string]any); acc["value"] != "H100" {
		t.Errorf("nested object not decoded: %v", acc)
	}
	limits, d := h.readDataSource("limits", map[string]any{})
	noErrors(t, "limits", d)
	if len(limits["data"].([]any)) != 1 {
		t.Errorf("unexpected limits: %v", limits)
	}
	_, d = h.readDataSource("sku", map[string]any{"id": "sku_nope"})
	if !hasError(d, "HTTP 404") {
		t.Errorf("expected a 404 error, got %v", d)
	}
}

func TestClientRetriesAndErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer token")
		}
		switch {
		case r.URL.Path == "/flaky" && n < 3:
			w.WriteHeader(http.StatusServiceUnavailable)
		case r.URL.Path == "/flaky":
			_, _ = w.Write([]byte(`{"ok": true, "n": 12345678901234567890}`))
		case r.URL.Path == "/post":
			if r.Header.Get("Idempotency-Key") == "" {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Retry-After", "0")
			if n%2 == 1 {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": "x"}`))
		case r.URL.Path == "/bad":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"error": {"type": "unprocessable_entity", "message": "Pool has active dependencies.", "details": [{"code": "busy", "field": "pool", "message": "has orders"}]}}`))
		case strings.HasPrefix(r.URL.EscapedPath(), "/v2/pools/"):
			_, _ = w.Write([]byte(`{"path": "` + r.URL.EscapedPath() + `"}`))
		}
	}))
	defer srv.Close()
	c, err := runtime.NewClient(srv.URL, "tok", "ua")
	if err != nil {
		t.Fatal(err)
	}
	c.RetryWaitMin, c.RetryWaitMax = time.Millisecond, time.Millisecond
	ctx := context.Background()

	out, err := c.Do(ctx, runtime.Request{Method: "GET", Path: "/flaky"})
	if err != nil || out.(map[string]any)["ok"] != true || calls.Load() != 3 {
		t.Fatalf("GET should be retried on 503: out=%v err=%v calls=%d", out, err, calls.Load())
	}
	if n := out.(map[string]any)["n"]; n.(interface{ String() string }).String() != "12345678901234567890" {
		t.Errorf("large numbers must not lose precision: %v", n)
	}

	calls.Store(0)
	if _, err := c.Do(ctx, runtime.Request{Method: "POST", Path: "/post", Body: map[string]any{}}); runtime.StatusOf(err) != 503 || calls.Load() != 1 {
		t.Errorf("POST without idempotency key must not be retried: err=%v calls=%d", err, calls.Load())
	}
	calls.Store(0)
	if out, err := c.Do(ctx, runtime.Request{Method: "POST", Path: "/post", Body: map[string]any{}, IdempotencyHeader: "Idempotency-Key"}); err != nil || out.(map[string]any)["id"] != "x" {
		t.Errorf("POST with idempotency key should be retried on 429: %v %v", out, err)
	}

	_, err = c.Do(ctx, runtime.Request{Method: "DELETE", Path: "/bad"})
	if runtime.StatusOf(err) != 422 || !strings.Contains(err.Error(), "Pool has active dependencies.") || !strings.Contains(err.Error(), "pool: has orders [busy]") {
		t.Errorf("error not parsed: %v", err)
	}

	p, err := runtime.ExpandPath("/v2/pools/{id}", map[string]string{"id": "sfc:pool:acme:prod:a b/c"})
	if err != nil {
		t.Fatal(err)
	}
	out, err = c.Do(ctx, runtime.Request{Method: "GET", Path: p})
	if err != nil || out.(map[string]any)["path"] != "/v2/pools/sfc:pool:acme:prod:a%20b%2Fc" {
		t.Errorf("path parameter not escaped as one segment: %v %v", out, err)
	}

	noToken, _ := runtime.NewClient(srv.URL, "", "ua")
	if _, err := noToken.Do(ctx, runtime.Request{Method: "GET", Path: "/flaky"}); err == nil || !strings.Contains(err.Error(), "SFC_TOKEN") {
		t.Errorf("missing token should be explained: %v", err)
	}
	if _, err := runtime.NewClient("not a url", "t", "ua"); err == nil {
		t.Error("invalid base URL accepted")
	}
}
