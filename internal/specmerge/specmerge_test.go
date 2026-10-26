package specmerge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func page(source, method, path, yaml string) string {
	return "# Title\n\n> blurb\n\n## OpenAPI\n\n````yaml " + source + " " + method + " " + path + "\n" + yaml + "````\n\nTrailing prose.\n"
}

const ordersSlice = `openapi: 3.1.0
info:
  title: sfc-api
  version: 0.1.0
servers:
  - url: https://api.sfcompute.com
tags:
  - name: Orders
    description: Place orders.
  - name: Orders
    description: Estimate orders.
paths:
  /v2/orders:
    post:
      operationId: create_order
      responses:
        '201':
          description: ok
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Order'
components:
  schemas:
    Order:
      type: object
      properties:
        id: {type: string}
        count: {type: integer, minimum: 1}
        price: {type: number, example: 1.50}
        created: {type: string, example: 2024-01-02}
      required: [id]
`

const getOrderSlice = `openapi: 3.1.0
info:
  title: sfc-api
  version: 0.1.0
servers:
  - url: https://api.sfcompute.com
tags:
  - name: Orders
    description: Place orders.
paths:
  /v2/orders/{id}:
    get:
      operationId: fetch_order
      responses:
        200:
          description: ok
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Order'
components:
  schemas:
    Order:
      type: object
      properties:
        id: {type: string}
        count: {type: integer, minimum: 1}
        price: {type: number, example: 1.50}
        created: {type: string, example: 2024-01-02}
      required: [id]
`

func TestExtractBlocks(t *testing.T) {
	md := "intro\n```yaml\nnot: an openapi block\n```\n" + page("/openapi.json", "POST", "/v2/orders", ordersSlice) +
		page("/preview/openapi.json", "get", "/preview/v2/orders/{id}", "openapi: 3.1.0\n")
	blocks, err := ExtractBlocks([]byte(md))
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want 2", len(blocks))
	}
	if b := blocks[0]; b.Source != "/openapi.json" || b.Method != "post" || b.Path != "/v2/orders" || !strings.HasPrefix(string(b.YAML), "openapi: 3.1.0\n") {
		t.Errorf("unexpected first block: %+v", b)
	}
	if blocks[1].Source != "/preview/openapi.json" || blocks[1].Method != "get" {
		t.Errorf("unexpected second block: %+v", blocks[1])
	}

	if _, err := ExtractBlocks([]byte("````yaml /openapi.json get /x\nopenapi: 3.1.0\n")); err == nil {
		t.Error("expected error for unterminated block")
	}
}

func TestYAMLToJSONPreservesScalars(t *testing.T) {
	v, err := YAMLToJSON([]byte("a: 1.50\nb: 007\nc: 2024-01-02\nd: '200'\n200: x\ne: [true, null, 1e3]\nf: &x {k: v}\ng: *x\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(v)
	want := `{"200":"x","a":1.50,"b":7,"c":"2024-01-02","d":"200","e":[true,null,1e3],"f":{"k":"v"},"g":{"k":"v"}}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if _, err := YAMLToJSON([]byte("a: 1\na: 2\n")); err == nil {
		t.Error("expected duplicate key error")
	}
}

func mustDoc(t *testing.T, y string) map[string]any {
	t.Helper()
	v, err := YAMLToJSON([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return v.(map[string]any)
}

func TestMergeCombinesPathsAndComponents(t *testing.T) {
	m := NewMerger()
	if err := m.Add("a", mustDoc(t, ordersSlice)); err != nil {
		t.Fatal(err)
	}
	if err := m.Add("b", mustDoc(t, getOrderSlice)); err != nil {
		t.Fatal(err)
	}
	doc, err := m.Result()
	if err != nil {
		t.Fatal(err)
	}
	paths := doc["paths"].(map[string]any)
	if len(paths) != 2 || paths["/v2/orders"] == nil || paths["/v2/orders/{id}"] == nil {
		t.Errorf("paths not merged: %v", paths)
	}
	tags := doc["tags"].([]any)
	if len(tags) != 2 {
		t.Errorf("want 2 distinct tags (same name, different description), got %d: %v", len(tags), tags)
	}
	out, err := Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(out), "}\n") || !strings.Contains(string(out), `"price": {`) {
		t.Errorf("unexpected rendering:\n%s", out)
	}
}

func TestMergeIsOrderIndependent(t *testing.T) {
	render := func(order ...string) string {
		m := NewMerger()
		for _, s := range order {
			if err := m.Add(s[:10], mustDoc(t, s)); err != nil {
				t.Fatal(err)
			}
		}
		doc, err := m.Result()
		if err != nil {
			t.Fatal(err)
		}
		b, err := Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if a, b := render(ordersSlice, getOrderSlice), render(getOrderSlice, ordersSlice); a != b {
		t.Errorf("output depends on input order:\n%s\n---\n%s", a, b)
	}
}

func TestMergeSamePathDifferentMethods(t *testing.T) {
	m := NewMerger()
	_ = m.Add("a", mustDoc(t, "openapi: 3.1.0\npaths:\n  /x:\n    get: {operationId: g}\n"))
	_ = m.Add("b", mustDoc(t, "openapi: 3.1.0\npaths:\n  /x:\n    post: {operationId: p}\n"))
	doc, err := m.Result()
	if err != nil {
		t.Fatal(err)
	}
	item := doc["paths"].(map[string]any)["/x"].(map[string]any)
	if item["get"] == nil || item["post"] == nil {
		t.Errorf("methods not merged: %v", item)
	}
}

func TestMergeConflictFailsLoudly(t *testing.T) {
	m := NewMerger()
	_ = m.Add("page-a", mustDoc(t, ordersSlice))
	conflicting := strings.Replace(getOrderSlice, "minimum: 1", "minimum: 2", 1)
	_ = m.Add("page-b", mustDoc(t, conflicting))
	_ = m.Add("page-c", mustDoc(t, conflicting))
	_, err := m.Result()
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("expected ConflictError, got %v", err)
	}
	if len(ce.Conflicts) != 1 {
		t.Fatalf("expected the identical disagreement to be reported once, got %d", len(ce.Conflicts))
	}
	c := ce.Conflicts[0]
	if c.Pointer != "/components/schemas/Order" || c.First != "page-a" || c.Second != "page-b" || c.More != 1 {
		t.Errorf("unexpected conflict: %+v", c)
	}
	if !strings.Contains(err.Error(), "page-a and page-b (and 1 more) disagree") {
		t.Errorf("unhelpful error message: %v", err)
	}
}

func TestMergeConflictingInfo(t *testing.T) {
	m := NewMerger()
	_ = m.Add("a", mustDoc(t, "openapi: 3.1.0\ninfo: {title: one}\n"))
	_ = m.Add("b", mustDoc(t, "openapi: 3.1.0\ninfo: {title: two}\n"))
	if _, err := m.Result(); err == nil {
		t.Error("expected conflict on /info")
	}
}

func TestReferencePages(t *testing.T) {
	index := []byte(`# Docs
- [Create order](https://docs.sfcompute.com/api-reference/orders/create-order.md): Place an order.
- [Create order](https://docs.sfcompute.com/api-reference/orders/create-order.md): duplicate
- [Preview](/preview/api-reference/orders/get-order.md): relative link
- [Guide](https://docs.sfcompute.com/preview/orders.md): not a reference page
- [Elsewhere](https://evil.example.com/api-reference/x.md): other host
- [openapi](/openapi.json)
`)
	pages, err := ReferencePages(index, "https://docs.sfcompute.com/llms.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://docs.sfcompute.com/api-reference/orders/create-order.md",
		"https://docs.sfcompute.com/preview/api-reference/orders/get-order.md",
	}
	if fmt.Sprint(pages) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v", pages, want)
	}
}

func TestBuildAgainstFakeDocsSite(t *testing.T) {
	mux := http.NewServeMux()
	var srvURL string
	mux.HandleFunc("/llms.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "- [a](%s/api-reference/orders/create-order.md)\n- [b](/api-reference/orders/get-order.md)\n- [c](/preview/api-reference/orders/get-order.md)\n", srvURL)
	})
	mux.HandleFunc("/api-reference/orders/create-order.md", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, page("/openapi.json", "post", "/v2/orders", ordersSlice))
	})
	mux.HandleFunc("/api-reference/orders/get-order.md", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, page("/openapi.json", "get", "/v2/orders/{id}", getOrderSlice))
	})
	mux.HandleFunc("/preview/api-reference/orders/get-order.md", func(w http.ResponseWriter, r *http.Request) {
		// The preview document legitimately defines Order differently;
		// that must not conflict with the stable document.
		fmt.Fprint(w, page("/preview/openapi.json", "get", "/v2/orders/{id}", strings.Replace(getOrderSlice, "minimum: 1", "minimum: 5", 1)))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	docs, stats, err := Build(context.Background(), &Fetcher{Client: srv.Client()}, srv.URL+"/llms.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || docs["/openapi.json"] == nil || docs["/preview/openapi.json"] == nil {
		t.Fatalf("expected one document per source, got %v", docs)
	}
	if st := stats["/openapi.json"]; st.Pages != 2 || st.Operations != 2 || st.Schemas != 1 {
		t.Errorf("unexpected stats %+v", st)
	}
}

func TestBuildFailsOnPageWithoutSpec(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/llms.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "- [a](/api-reference/x.md)\n")
	})
	mux.HandleFunc("/api-reference/x.md", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "# A page whose format changed\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	_, _, err := Build(context.Background(), &Fetcher{Client: srv.Client()}, srv.URL+"/llms.txt")
	if err == nil || !strings.Contains(err.Error(), "no embedded OpenAPI document") {
		t.Fatalf("expected loud failure, got %v", err)
	}
}

func TestBuildFailsWhenFenceDisagreesWithDocument(t *testing.T) {
	m := NewMultiMerger()
	err := m.AddPage("p", []byte(page("/openapi.json", "delete", "/v2/orders", ordersSlice)))
	if err == nil || !strings.Contains(err.Error(), "does not define it") {
		t.Fatalf("expected mismatch error, got %v", err)
	}
	err = m.AddPage("p", []byte(page("/../../etc/openapi.json", "post", "/v2/orders", ordersSlice)))
	if err == nil || !strings.Contains(err.Error(), "unexpected OpenAPI source") {
		t.Fatalf("expected source validation error, got %v", err)
	}
}

func TestFetcherDoesNotRetryClientErrors(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.NotFound(w, r)
	}))
	defer srv.Close()
	f := &Fetcher{Client: srv.Client(), Retries: 3}
	if _, err := f.Get(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("404 was retried %d times", calls-1)
	}
}
