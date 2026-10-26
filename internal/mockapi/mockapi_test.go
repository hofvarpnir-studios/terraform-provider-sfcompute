package mockapi

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func TestMockFlagsTrafficThatViolatesTheSpec(t *testing.T) {
	spec, err := LoadSpec("../../spec/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	s := New(spec)
	defer s.Close()
	do := func(method, path, body string) int {
		req, _ := http.NewRequest(method, s.URL+path, bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+s.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if st := do("POST", "/v2/workspaces", `{"name": "ok"}`); st != 201 {
		t.Fatalf("create workspace: %d", st)
	}
	if v := s.Violations(); len(v) != 0 {
		t.Fatalf("valid traffic flagged: %v", v)
	}
	do("POST", "/v2/workspaces", `{"name": "x", "colour": "red"}`)
	do("GET", "/v2/workspaces?bogus=1", "")
	do("POST", "/v2/pools", `{"name": "no-workspace"}`)
	v := strings.Join(s.Violations(), "\n")
	for _, want := range []string{`unknown property "colour"`, `undeclared query parameter "bogus"`, `missing required property "workspace"`} {
		if !strings.Contains(v, want) {
			t.Errorf("violation %q not detected; got:\n%s", want, v)
		}
	}
	req, _ := http.NewRequest("GET", s.URL+"/v2/workspaces", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated request got %d", resp.StatusCode)
	}
}
