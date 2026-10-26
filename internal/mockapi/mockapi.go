// Package mockapi is an in-memory imitation of the SF Compute API for
// offline tests. It implements the endpoints the provider uses with the
// request and response shapes documented in spec/openapi.json, and it checks
// itself against that document: every request body and query parameter the
// provider sends, and every response the mock returns, is validated against
// the (prepared) spec, and violations are recorded in Server.Violations.
package mockapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/hofvarpnir-studios/terraform-provider-sfcompute/internal/codegen"
)

// Account is the organization name used in resource paths.
const Account = "acme"

// Recorded is one request the mock received.
type Recorded struct {
	Method string
	Path   string
	Query  string
	Body   map[string]any
}

// Server is a running mock API.
type Server struct {
	*httptest.Server

	Token    string
	PageSize int

	mu         sync.Mutex
	spec       *codegen.Spec
	routes     []route
	objects    map[string]map[string]map[string]any // kind -> id -> object
	order      map[string][]string
	seq        int
	clock      int64
	requests   []Recorded
	violations []string
	// FailNext, if set, makes the next request matching "METHOD path"
	// fail with the given status.
	failNext map[string]int
}

type route struct {
	method  string
	pattern string // spec path, e.g. /v2/pools/{id}
	re      *regexp.Regexp
	handler func(s *Server, params map[string]string, q map[string][]string, body map[string]any) (int, any)
}

// LoadSpec reads and prepares an OpenAPI document (spec/openapi.json).
func LoadSpec(path string) (*codegen.Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	doc, err := codegen.ReadJSON(data)
	if err != nil {
		return nil, err
	}
	p, err := codegen.Prepare(doc)
	if err != nil {
		return nil, err
	}
	return &codegen.Spec{Doc: p.Doc}, nil
}

// New starts a mock server that validates traffic against spec.
func New(spec *codegen.Spec) *Server {
	s := &Server{
		Token:    "sk_test_mock",
		PageSize: 2,
		spec:     spec,
		objects:  map[string]map[string]map[string]any{},
		order:    map[string][]string{},
		clock:    1_700_000_000,
		failNext: map[string]int{},
	}
	s.registerRoutes()
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Requests returns a copy of the requests received so far.
func (s *Server) Requests() []Recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Recorded(nil), s.requests...)
}

// CountRequests counts received requests with the given method and spec path
// pattern (e.g. "POST", "/v2/orders").
func (s *Server) CountRequests(method, pattern string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.requests {
		if r.Method == method && s.matchPattern(r.Method, r.Path) == pattern {
			n++
		}
	}
	return n
}

// Violations returns every spec violation observed.
func (s *Server) Violations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.violations...)
}

// FailNext makes the next request for method+pattern answer with status.
func (s *Server) FailNext(method, pattern string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext[method+" "+pattern] = status
}

// Object returns a stored object by kind and ID (nil if absent).
func (s *Server) Object(kind, id string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objects[kind][id]
}

// Mutate changes a stored object, simulating out-of-band changes (drift).
func (s *Server) Mutate(kind, id string, f func(obj map[string]any)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o := s.objects[kind][id]; o != nil {
		f(o)
	}
}

// Seed stores an object as if it had been created out of band, returning
// its ID. Used to provide fixtures such as images and SKUs.
func (s *Server) Seed(kind string, obj map[string]any) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := obj["id"].(string)
	s.put(kind, id, obj)
	return id
}

func (s *Server) matchPattern(method, path string) string {
	for _, r := range s.routes {
		if r.method == method && r.re.MatchString(path) {
			return r.pattern
		}
	}
	return ""
}

func (s *Server) handle(method, pattern string, h func(s *Server, params map[string]string, q map[string][]string, body map[string]any) (int, any)) {
	re := "^" + regexp.MustCompile(`\\\{[^}]+\\\}`).ReplaceAllString(regexp.QuoteMeta(pattern), `([^/]+)`) + "$"
	s.routes = append(s.routes, route{method: method, pattern: pattern, re: regexp.MustCompile(re), handler: h})
}

func (s *Server) violatef(format string, args ...any) {
	s.violations = append(s.violations, fmt.Sprintf(format, args...))
}

func errorBody(typ, msg string) map[string]any {
	return map[string]any{"error": map[string]any{"type": typ, "message": msg}}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("invalid_request_error", "malformed JSON: "+err.Error()))
			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, Recorded{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery, Body: body})

	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		writeJSON(w, http.StatusUnauthorized, errorBody("unauthorized", "missing or invalid bearer token"))
		return
	}
	for _, rt := range s.routes {
		if rt.method != r.Method {
			continue
		}
		m := rt.re.FindStringSubmatch(r.URL.EscapedPath())
		if m == nil {
			continue
		}
		params := map[string]string{}
		names := regexp.MustCompile(`\{([^}]+)\}`).FindAllStringSubmatch(rt.pattern, -1)
		for i, n := range names {
			params[n[1]], _ = urlUnescape(m[i+1])
		}
		key := r.Method + " " + rt.pattern
		if st, ok := s.failNext[key]; ok {
			delete(s.failNext, key)
			writeJSON(w, st, errorBody("injected", "injected failure"))
			return
		}
		s.checkRequest(rt, r, body, len(bytes.TrimSpace(raw)) > 0)
		s.clock++
		status, resp := rt.handler(s, params, r.URL.Query(), body)
		if status >= 200 && status < 300 {
			s.checkResponse(rt, status, resp)
		}
		if resp == nil {
			w.WriteHeader(status)
			return
		}
		writeJSON(w, status, resp)
		return
	}
	writeJSON(w, http.StatusNotFound, errorBody("not_found", "no route for "+r.Method+" "+r.URL.Path))
}

func urlUnescape(s string) (string, error) {
	return strings.NewReplacer("%3A", ":", "%3a", ":").Replace(s), nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) checkRequest(rt route, r *http.Request, body map[string]any, hasBody bool) {
	where := rt.method + " " + rt.pattern
	params := s.spec.Params(rt.method, rt.pattern)
	declared := map[string]codegen.Param{}
	for _, p := range params {
		if p.In == "query" {
			declared[p.Name] = p
		}
	}
	for name, vals := range r.URL.Query() {
		p, ok := declared[name]
		if !ok {
			s.violatef("%s: undeclared query parameter %q", where, name)
			continue
		}
		if p.Schema["type"] != "array" && len(vals) > 1 {
			s.violatef("%s: query parameter %q repeated but not an array", where, name)
		}
	}
	for _, p := range params {
		if p.In == "query" && p.Required && r.URL.Query().Get(p.Name) == "" {
			s.violatef("%s: missing required query parameter %q", where, p.Name)
		}
	}
	reqSchema := s.spec.RequestSchema(rt.method, rt.pattern)
	switch {
	case reqSchema == nil && hasBody:
		s.violatef("%s: sent a body but the operation takes none", where)
	case reqSchema != nil:
		if body == nil {
			body = map[string]any{}
		}
		for _, v := range validate(reqSchema, body, "body", true) {
			s.violatef("%s: request %s", where, v)
		}
	}
}

func (s *Server) checkResponse(rt route, status int, resp any) {
	where := fmt.Sprintf("%s %s -> %d", rt.method, rt.pattern, status)
	schema := s.spec.ResponseSchema(rt.method, rt.pattern)
	if schema == nil {
		if resp != nil {
			s.violatef("%s: mock returned a body but the spec documents none", where)
		}
		return
	}
	// Normalise through JSON so numbers are json.Number like the client sees.
	b, _ := json.Marshal(resp)
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	_ = dec.Decode(&v)
	for _, msg := range validate(schema, v, "response", true) {
		s.violatef("%s: %s", where, msg)
	}
}

// validate checks v against a prepared schema (type, properties, required,
// items, additionalProperties, enum). Nullability was flattened away by
// prepare, so null is accepted anywhere.
func validate(schema map[string]any, v any, path string, strict bool) []string {
	if v == nil || schema == nil {
		return nil
	}
	var out []string
	switch schema["type"] {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s: expected object, got %T", path, v)}
		}
		props, _ := schema["properties"].(map[string]any)
		for _, r := range anyStrings(schema["required"]) {
			if _, ok := obj[r]; !ok {
				out = append(out, fmt.Sprintf("%s: missing required property %q", path, r))
			}
		}
		ap, hasAP := schema["additionalProperties"].(map[string]any)
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if ps, ok := props[k].(map[string]any); ok {
				out = append(out, validate(ps, obj[k], path+"."+k, strict)...)
			} else if hasAP {
				out = append(out, validate(ap, obj[k], path+"."+k, strict)...)
			} else if strict && props != nil && k != "object" {
				// "object" is the constant type discriminator that
				// codegen.Prepare drops from the schema.
				out = append(out, fmt.Sprintf("%s: unknown property %q", path, k))
			}
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return []string{fmt.Sprintf("%s: expected array, got %T", path, v)}
		}
		items, _ := schema["items"].(map[string]any)
		for i, e := range arr {
			out = append(out, validate(items, e, fmt.Sprintf("%s[%d]", path, i), strict)...)
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			return []string{fmt.Sprintf("%s: expected string, got %T", path, v)}
		}
		if enum, ok := schema["enum"].([]any); ok && !containsAny(enum, str) {
			out = append(out, fmt.Sprintf("%s: %q not in enum %v", path, str, enum))
		}
	case "integer":
		n, ok := v.(json.Number)
		if _, err := n.Int64(); !ok || err != nil {
			out = append(out, fmt.Sprintf("%s: expected integer, got %v", path, v))
		}
	case "number":
		if _, ok := v.(json.Number); !ok {
			out = append(out, fmt.Sprintf("%s: expected number, got %T", path, v))
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			out = append(out, fmt.Sprintf("%s: expected boolean, got %T", path, v))
		}
	}
	return out
}

func anyStrings(v any) []string {
	l, _ := v.([]any)
	var out []string
	for _, x := range l {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func containsAny(l []any, v any) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

// ---- storage helpers (callers hold s.mu) ----

func (s *Server) nextID(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s_%05d", prefix, s.seq)
}

func (s *Server) put(kind, id string, obj map[string]any) {
	if s.objects[kind] == nil {
		s.objects[kind] = map[string]map[string]any{}
	}
	if _, exists := s.objects[kind][id]; !exists {
		s.order[kind] = append(s.order[kind], id)
	}
	s.objects[kind][id] = obj
}

func (s *Server) del(kind, id string) {
	delete(s.objects[kind], id)
	ids := s.order[kind][:0]
	for _, x := range s.order[kind] {
		if x != id {
			ids = append(ids, x)
		}
	}
	s.order[kind] = ids
}

// lookup finds an object by ID, resource path or (within kinds that have
// one) name.
func (s *Server) lookup(kind, ref string) map[string]any {
	if o := s.objects[kind][ref]; o != nil {
		return o
	}
	for _, id := range s.order[kind] {
		o := s.objects[kind][id]
		if o["resource_path"] == ref || o["name"] == ref {
			return o
		}
	}
	return nil
}

func (s *Server) list(kind, cursorPrefix string, q map[string][]string, keep func(map[string]any) bool) (int, any) {
	limit := s.PageSize
	if l := first(q["limit"]); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 {
			return http.StatusBadRequest, errorBody("invalid_request_error", "invalid limit")
		}
		limit = n
	}
	after := strings.TrimPrefix(first(q["starting_after"]), cursorPrefix+"_")
	var all []any
	started := after == ""
	more := false
	var last string
	for _, id := range s.order[kind] {
		o := s.objects[kind][id]
		if keep != nil && !keep(o) {
			continue
		}
		if !started {
			if id == after {
				started = true
			}
			continue
		}
		if len(all) == limit {
			more = true
			break
		}
		all = append(all, o)
		last = id
	}
	resp := map[string]any{"object": "list", "data": nonNil(all), "has_more": more}
	if more {
		resp["cursor"] = cursorPrefix + "_" + last
	}
	return http.StatusOK, resp
}

func nonNil(l []any) []any {
	if l == nil {
		return []any{}
	}
	return l
}

func first(l []string) string {
	if len(l) == 0 {
		return ""
	}
	return l[0]
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func notFound(what string) (int, any) {
	return http.StatusNotFound, errorBody("not_found", what+" not found")
}

func summary(o map[string]any) map[string]any {
	return map[string]any{"id": o["id"], "name": o["name"]}
}
