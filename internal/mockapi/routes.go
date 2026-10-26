package mockapi

import (
	"net/http"
	"strings"
)

type params = map[string]string
type query = map[string][]string
type body = map[string]any

func (s *Server) registerRoutes() {
	// Account and limits.
	s.handle("GET", "/preview/v2/me", func(s *Server, _ params, _ query, _ body) (int, any) {
		return http.StatusOK, map[string]any{"account_id": Account, "created_at": int64(1_690_000_000), "entity_type": "business"}
	})
	s.handle("GET", "/preview/v2/limits", func(s *Server, _ params, _ query, _ body) (int, any) {
		return http.StatusOK, map[string]any{"object": "list", "has_more": false, "data": []any{
			map[string]any{"name": "max_nodes", "scope": map[string]any{}, "value": "16", "usage": "2"},
		}}
	})

	// SKUs (read only, seeded).
	s.handle("GET", "/v2/skus", func(s *Server, _ params, q query, _ body) (int, any) {
		return s.list("sku", "skuc", q, nil)
	})
	s.handle("GET", "/v2/skus/{id}", func(s *Server, p params, _ query, _ body) (int, any) {
		if o := s.lookup("sku", p["id"]); o != nil {
			return http.StatusOK, o
		}
		return notFound("SKU")
	})

	// Workspaces.
	s.handle("POST", "/v2/workspaces", func(s *Server, _ params, _ query, b body) (int, any) {
		id := s.nextID("wksp")
		name := str(b["name"])
		if name == "" {
			name = "workspace-" + id[len(id)-3:]
		}
		o := map[string]any{"object": "workspace", "id": id, "name": name, "resource_path": "sfc:workspace:" + Account + ":" + name, "created_at": s.clock}
		s.put("workspace", id, o)
		return http.StatusCreated, o
	})
	s.handle("GET", "/v2/workspaces", func(s *Server, _ params, q query, _ body) (int, any) {
		return s.list("workspace", "wkspc", q, nil)
	})
	s.handle("GET", "/v2/workspaces/{id}", func(s *Server, p params, _ query, _ body) (int, any) {
		if o := s.lookup("workspace", p["id"]); o != nil {
			return http.StatusOK, o
		}
		return notFound("Workspace")
	})
	s.handle("PATCH", "/v2/workspaces/{id}", func(s *Server, p params, _ query, b body) (int, any) {
		o := s.lookup("workspace", p["id"])
		if o == nil {
			return notFound("Workspace")
		}
		if n := str(b["name"]); n != "" {
			o["name"] = n
			o["resource_path"] = "sfc:workspace:" + Account + ":" + n
		}
		return http.StatusOK, o
	})
	s.handle("DELETE", "/v2/workspaces/{id}", func(s *Server, p params, _ query, _ body) (int, any) {
		o := s.lookup("workspace", p["id"])
		if o == nil {
			return notFound("Workspace")
		}
		for _, kind := range []string{"pool", "firewall", "instance_template", "infiniband_partition"} {
			for _, other := range s.objects[kind] {
				if s.workspaceOf(other) == o["id"] {
					return http.StatusUnprocessableEntity, errorBody("unprocessable_entity", "Workspace is not empty.")
				}
			}
		}
		s.del("workspace", str(o["id"]))
		return http.StatusNoContent, nil
	})

	// Pools.
	s.handle("POST", "/v2/pools", func(s *Server, _ params, _ query, b body) (int, any) {
		ws := s.lookup("workspace", str(b["workspace"]))
		if ws == nil {
			return http.StatusNotFound, errorBody("not_found", "Workspace not found")
		}
		id := s.nextID("pool")
		name := str(b["name"])
		if name == "" {
			name = "pool-" + id[len(id)-3:]
		}
		tags, _ := b["tags"].(map[string]any)
		if tags == nil {
			tags = map[string]any{}
		}
		o := map[string]any{
			"object": "pool", "id": id, "name": name,
			"resource_path":       "sfc:pool:" + Account + ":" + str(ws["name"]) + ":" + name,
			"workspace":           summary(ws),
			"tags":                tags,
			"allocation_schedule": map[string]any{"by_sku": map[string]any{}, "total": []any{}},
			"created_at":          s.clock,
		}
		s.put("pool", id, o)
		return http.StatusCreated, o
	})
	s.handle("GET", "/v2/pools", func(s *Server, _ params, q query, _ body) (int, any) {
		ws := s.lookup("workspace", first(q["workspace"]))
		if first(q["workspace"]) != "" && ws == nil {
			return http.StatusNotFound, errorBody("not_found", "Workspace not found")
		}
		return s.list("pool", "poolc", q, func(o map[string]any) bool {
			return ws == nil || s.workspaceOf(o) == ws["id"]
		})
	})
	s.handle("GET", "/v2/pools/{id}", func(s *Server, p params, _ query, _ body) (int, any) {
		if o := s.lookup("pool", p["id"]); o != nil {
			return http.StatusOK, o
		}
		return notFound("Pool")
	})
	s.handle("PATCH", "/v2/pools/{id}", func(s *Server, p params, _ query, b body) (int, any) {
		o := s.lookup("pool", p["id"])
		if o == nil {
			return notFound("Pool")
		}
		if n := str(b["name"]); n != "" {
			o["name"] = n
			ws := o["workspace"].(map[string]any)
			o["resource_path"] = "sfc:pool:" + Account + ":" + str(ws["name"]) + ":" + n
		}
		if t, ok := b["tags"].(map[string]any); ok {
			o["tags"] = t
		}
		return http.StatusOK, o
	})
	s.handle("DELETE", "/v2/pools/{id}", func(s *Server, p params, _ query, _ body) (int, any) {
		o := s.lookup("pool", p["id"])
		if o == nil {
			return notFound("Pool")
		}
		for _, inst := range s.objects["instance"] {
			if inst["pool"].(map[string]any)["id"] == o["id"] && inst["state"] != "terminated" {
				return http.StatusUnprocessableEntity, errorBody("unprocessable_entity", "Pool has active dependencies.")
			}
		}
		s.del("pool", str(o["id"]))
		return http.StatusNoContent, nil
	})

	// Firewalls (preview routes in the stable document).
	s.handle("POST", "/preview/v2/firewalls", func(s *Server, _ params, _ query, b body) (int, any) {
		ws := s.lookup("workspace", str(b["workspace"]))
		if ws == nil {
			return http.StatusNotFound, errorBody("not_found", "Workspace not found")
		}
		id := s.nextID("frwl")
		name := str(b["name"])
		o := map[string]any{
			"object": "firewall", "id": id, "name": name, "owner": Account,
			"workspace": ws["name"], "workspace_id": ws["id"],
			"resource_path": "sfc:firewall:" + Account + ":" + str(ws["name"]) + ":" + name,
			"rules":         normalizeRules(b["rules"]),
			"created_at":    s.clock, "updated_at": s.clock,
		}
		s.put("firewall", id, o)
		return http.StatusCreated, o
	})
	s.handle("GET", "/preview/v2/firewalls", func(s *Server, _ params, q query, _ body) (int, any) {
		ws := s.lookup("workspace", first(q["workspace"]))
		var data []any
		for _, id := range s.order["firewall"] {
			if o := s.objects["firewall"][id]; ws == nil || o["workspace_id"] == ws["id"] {
				data = append(data, o)
			}
		}
		return http.StatusOK, map[string]any{"object": "list", "data": nonNil(data)}
	})
	s.handle("GET", "/preview/v2/firewalls/{id}", func(s *Server, p params, _ query, _ body) (int, any) {
		if o := s.lookup("firewall", p["id"]); o != nil {
			return http.StatusOK, o
		}
		return notFound("Firewall")
	})
	s.handle("PUT", "/preview/v2/firewalls/{id}", func(s *Server, p params, _ query, b body) (int, any) {
		o := s.lookup("firewall", p["id"])
		if o == nil {
			return notFound("Firewall")
		}
		o["rules"] = normalizeRules(b["rules"])
		o["updated_at"] = s.clock
		return http.StatusOK, o
	})
	s.handle("DELETE", "/preview/v2/firewalls/{id}", func(s *Server, p params, _ query, _ body) (int, any) {
		o := s.lookup("firewall", p["id"])
		if o == nil {
			return notFound("Firewall")
		}
		s.del("firewall", str(o["id"]))
		return http.StatusNoContent, nil
	})

	// Instance templates.
	s.handle("POST", "/preview/v2/instance_templates", func(s *Server, _ params, _ query, b body) (int, any) {
		ws := s.lookup("workspace", str(b["workspace"]))
		if ws == nil {
			return http.StatusNotFound, errorBody("not_found", "Workspace not found")
		}
		img := s.lookup("image", str(b["image"]))
		if img == nil {
			return http.StatusNotFound, errorBody("not_found", "Image not found")
		}
		id := s.nextID("itmpl")
		name := str(b["name"])
		if name == "" {
			name = "template-" + id[len(id)-3:]
		}
		enable, _ := b["enable_public_ipv4"].(bool)
		tags, _ := b["tags"].(map[string]any)
		o := map[string]any{
			"object": "instance_template", "id": id, "name": name, "owner": Account,
			"workspace": ws["name"], "workspace_id": ws["id"],
			"resource_path":             "sfc:instance_template:" + Account + ":" + str(ws["name"]) + ":" + name,
			"image":                     summary(img),
			"enable_public_ipv4":        enable,
			"cloud_init_user_data_used": str(b["cloud_init_user_data"]) != "",
			"created_at":                s.clock,
		}
		if tags != nil {
			o["tags"] = tags
		}
		if fw := str(b["firewall"]); fw != "" {
			f := s.lookup("firewall", fw)
			if f == nil {
				return http.StatusNotFound, errorBody("not_found", "Firewall not found")
			}
			o["firewall"] = f["id"]
		}
		s.put("instance_template", id, o)
		return http.StatusCreated, o
	})
	s.handle("GET", "/preview/v2/instance_templates/{id}", func(s *Server, p params, _ query, _ body) (int, any) {
		if o := s.lookup("instance_template", p["id"]); o != nil {
			return http.StatusOK, o
		}
		return notFound("Instance template")
	})
	s.handle("DELETE", "/preview/v2/instance_templates/{id}", func(s *Server, p params, _ query, _ body) (int, any) {
		o := s.lookup("instance_template", p["id"])
		if o == nil {
			return notFound("Instance template")
		}
		s.del("instance_template", str(o["id"]))
		return http.StatusNoContent, nil
	})

	// Images (read only, seeded).
	s.handle("GET", "/v2/images/{id}", func(s *Server, p params, _ query, _ body) (int, any) {
		if o := s.lookup("image", p["id"]); o != nil {
			return http.StatusOK, o
		}
		return notFound("Image")
	})

	// Orders.
	s.handle("POST", "/v2/orders", func(s *Server, _ params, _ query, b body) (int, any) {
		pool := s.lookup("pool", str(b["pool"]))
		if pool == nil {
			return http.StatusNotFound, errorBody("not_found", "Pool not found")
		}
		sku := s.lookup("sku", str(b["sku"]))
		if sku == nil {
			return http.StatusNotFound, errorBody("not_found", "SKU not found")
		}
		id := s.nextID("ordr")
		standing, _ := b["allow_standing"].(bool)
		state := "cancelled" // immediate-or-cancel with no match
		if standing {
			state = "standing"
		}
		o := map[string]any{
			"object": "order", "id": id, "side": b["side"],
			"pool":      map[string]any{"object": "pool_summary", "id": pool["id"], "name": pool["name"]},
			"workspace": pool["workspace"], "sku": map[string]any{"id": sku["id"], "alias": sku["alias"]},
			"allow_standing": standing, "allow_partial": b["allow_partial"],
			"allocation_schedule_delta":         b["allocation_schedule_delta"],
			"filled_allocation_schedule_delta":  []any{},
			"fills":                             []any{},
			"limit_price_dollars_per_node_hour": b["limit_price_dollars_per_node_hour"],
			"state":                             state, "created_at": s.clock,
		}
		s.put("order", id, o)
		return http.StatusCreated, o
	})
	s.handle("GET", "/v2/orders", func(s *Server, _ params, q query, _ body) (int, any) {
		return s.list("order", "ordrc", q, nil)
	})
	s.handle("GET", "/v2/orders/{id}", func(s *Server, p params, _ query, _ body) (int, any) {
		if o := s.lookup("order", p["id"]); o != nil {
			return http.StatusOK, o
		}
		return notFound("Order")
	})
	s.handle("POST", "/v2/orders/{id}/cancel", func(s *Server, p params, _ query, _ body) (int, any) {
		o := s.lookup("order", p["id"])
		if o == nil {
			return notFound("Order")
		}
		if o["state"] != "standing" && o["state"] != "pending" {
			return http.StatusUnprocessableEntity, errorBody("unprocessable_entity", "Order cannot be cancelled.")
		}
		o["state"] = "cancelled"
		o["cancelled_at"] = s.clock
		return http.StatusNoContent, nil
	})

	// Instances.
	s.handle("POST", "/v2/instances", func(s *Server, _ params, _ query, b body) (int, any) {
		pool := s.lookup("pool", str(b["pool"]))
		if pool == nil {
			return http.StatusNotFound, errorBody("not_found", "Pool not found")
		}
		img := s.lookup("image", str(b["image"]))
		sku := s.lookup("sku", str(b["sku"]))
		if img == nil || sku == nil {
			return http.StatusNotFound, errorBody("not_found", "Image or SKU not found")
		}
		id := s.nextID("inst")
		name := str(b["name"])
		if name == "" {
			name = "instance-" + id[len(id)-3:]
		}
		ws := pool["workspace"].(map[string]any)
		prio := str(b["priority_level"])
		if prio == "" {
			prio = "normal"
		}
		tags, _ := b["tags"].(map[string]any)
		if tags == nil {
			tags = map[string]any{}
		}
		o := map[string]any{
			"object": "instance", "id": id, "name": name,
			"resource_path":             "sfc:instance:" + Account + ":" + str(ws["name"]) + ":" + name,
			"workspace":                 ws,
			"pool":                      summary(pool),
			"image":                     summary(img),
			"sku":                       map[string]any{"id": sku["id"], "alias": sku["alias"]},
			"state":                     "awaiting_allocation",
			"priority_level":            prio,
			"tags":                      tags,
			"cloud_init_user_data_used": str(b["cloud_init_user_data"]) != "",
			"created_at":                s.clock, "_preview_updated_at": s.clock,
		}
		s.put("instance", id, o)
		return http.StatusCreated, o
	})
	s.handle("GET", "/v2/instances", func(s *Server, _ params, q query, _ body) (int, any) {
		return s.list("instance", "instc", q, nil)
	})
	s.handle("GET", "/v2/instances/{id}", func(s *Server, p params, _ query, _ body) (int, any) {
		if o := s.lookup("instance", p["id"]); o != nil {
			return http.StatusOK, o
		}
		return notFound("Instance")
	})
	s.handle("PATCH", "/v2/instances/{id}", func(s *Server, p params, _ query, b body) (int, any) {
		o := s.lookup("instance", p["id"])
		if o == nil {
			return notFound("Instance")
		}
		for _, k := range []string{"name", "priority_level"} {
			if v := str(b[k]); v != "" {
				o[k] = v
			}
		}
		if t, ok := b["tags"].(map[string]any); ok {
			o["tags"] = t
		}
		o["_preview_updated_at"] = s.clock
		return http.StatusOK, o
	})
	s.handle("POST", "/v2/instances/{id}/terminate", func(s *Server, p params, _ query, _ body) (int, any) {
		o := s.lookup("instance", p["id"])
		if o == nil || o["state"] == "terminated" {
			return notFound("Instance")
		}
		o["state"] = "terminated"
		o["_preview_updated_at"] = s.clock
		return http.StatusOK, o
	})
	s.handle("DELETE", "/v2/instances/{id}", func(s *Server, p params, _ query, _ body) (int, any) {
		o := s.lookup("instance", p["id"])
		if o == nil {
			return notFound("Instance")
		}
		if o["state"] != "terminated" {
			return http.StatusConflict, errorBody("conflict", "Instance must be terminated before deletion.")
		}
		s.del("instance", str(o["id"]))
		return http.StatusNoContent, nil
	})
}

// workspaceOf returns the workspace ID an object belongs to.
func (s *Server) workspaceOf(o map[string]any) any {
	if id, ok := o["workspace_id"]; ok {
		return id
	}
	if ws, ok := o["workspace"].(map[string]any); ok {
		return ws["id"]
	}
	return nil
}

// normalizeRules applies the documented default (source 0.0.0.0/0).
func normalizeRules(v any) []any {
	rules, _ := v.([]any)
	out := make([]any, 0, len(rules))
	for _, r := range rules {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		c := map[string]any{}
		for k, v := range m {
			c[k] = v
		}
		if strings.TrimSpace(str(c["source"])) == "" {
			c["source"] = "0.0.0.0/0"
		}
		out = append(out, c)
	}
	return out
}

// SeedFixtures adds the read-only objects tests reference: a public image
// and two SKUs.
func (s *Server) SeedFixtures() {
	s.Seed("image", map[string]any{"object": "image", "id": "image_ubuntu2404", "name": "ubuntu-24.04-cuda-13.2",
		"resource_path": "sfc:image:sfcompute:public:ubuntu-24.04-cuda-13.2", "upload_status": "completed", "created_at": int64(1_690_000_000),
		"is_public": true, "workspace": map[string]any{"id": "wksp_sfcpublic0001", "name": "public"}})
	for i, alias := range []string{"h100-sea", "h200-iad", "h100-iad"} {
		s.Seed("sku", map[string]any{"object": "sku", "id": "sku_mock" + alias[:4] + string(rune('a'+i)), "alias": alias,
			"accelerator": map[string]any{"value": strings.ToUpper(alias[:4]), "display_value": strings.ToUpper(alias[:4])}})
	}
}
