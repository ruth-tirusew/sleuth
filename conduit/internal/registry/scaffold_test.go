package registry

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"text/template"

	"gopkg.in/yaml.v3"
)

func TestScaffoldNode(t *testing.T) {
	s := load(t)
	op, _ := s.Find("POST", "/users")
	out := s.ScaffoldNode(op, "api", "create_user")
	for _, want := range []string{"contract: api", "method: POST", `url: "{{ .env.base_url }}/users"`, "name: string", "equals: 200"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	get, _ := s.Find("GET", "/users/1")
	if out := s.ScaffoldNode(get, "api", "g"); !strings.Contains(out, "/users/{{ .env.id }}") && !strings.Contains(out, "/users/{id}") {
		t.Errorf("path param not handled:\n%s", out)
	}
}

func TestNodeID(t *testing.T) {
	for in, want := range map[string]string{
		"GET /health":                    "get_health",
		"GET /loanProcess/customer/{id}": "get_loanProcess_customer_id",
		"POST /product-scoring/mappings": "post_product_scoring_mappings",
		"DELETE /a/{b}/c.d":              "delete_a_b_c_d",
	} {
		if got := NodeID(in); got != want {
			t.Errorf("%s: %q want %q", in, got, want)
		}
	}
}

// Every scaffolded request body must satisfy its own schema. Opt-in against a real
// document: CONDUIT_TEST_OPENAPI=/path/or/url go test ./internal/registry
func TestSkeletonsSatisfyTheirSchemas(t *testing.T) {
	loc := os.Getenv("CONDUIT_TEST_OPENAPI")
	if loc == "" {
		t.Skip("CONDUIT_TEST_OPENAPI not set")
	}
	s, err := Load(loc)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, key := range s.Operations() {
		method, path, _ := strings.Cut(key, " ")
		op, _ := s.Find(method, path)
		if op.RequestSchema == nil {
			continue
		}
		checked++
		issues, err := s.Validate(op.RequestPtr, s.Skeleton(op.RequestSchema))
		if err != nil || len(issues) > 0 {
			t.Errorf("%s: skeleton invalid: %v %v", key, issues, err)
		}
	}
	t.Logf("checked %d request bodies", checked)
}

const reviewDoc = `{
 "openapi": "3.1.0",
 "paths": {
  "/items/{item-id}": {
   "parameters": [
     {"name": "x-access-token", "in": "header", "required": true},
     {"name": "page", "in": "query", "required": true}],
   "post": {
    "summary": "Create item\nRequires a token",
    "parameters": [
      {"name": "x-access-token", "in": "header", "required": true},
      {"name": "page", "in": "query", "required": false},
      {"name": "item-id", "in": "path", "required": true},
      {"name": "page-size", "in": "query", "required": true},
      {"name": "X-Request-Id", "in": "header", "required": true},
      {"name": "session", "in": "cookie", "required": true}],
    "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}},
    "responses": {"201": {}}},
   "trace": {"responses": {"200": {}}}},
  "/v1/items:batchGet": {"get": {"responses": {"200": {}}}}
 },
 "components": {"schemas": {
  "Base": {"type": "object", "required": ["kind"], "properties": {"kind": {"const": "item"}}},
  "Item": {"allOf": [{"$ref": "#/components/schemas/Base"}],
    "required": ["tags", "qty", "price", "code", "short"],
    "properties": {
      "tags": {"type": "array", "minItems": 2, "items": {"type": "string", "minLength": 10}},
      "qty": {"type": "integer", "minimum": 1},
      "price": {"type": "number", "exclusiveMinimum": 0},
      "code": {"type": "string", "minLength": 12},
      "short": {"type": "string", "maxLength": 3}}}
 }}}`

func loadDoc(t *testing.T, doc string) *Spec {
	t.Helper()
	p := t.TempDir() + "/o.json"
	os.WriteFile(p, []byte(doc), 0o644)
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Covers review findings: duplicate params, unsafe template names, multi-line
// summary, cookies, non-auth required headers, skeleton bounds, allOf siblings.
func TestScaffoldReviewFindings(t *testing.T) {
	s := loadDoc(t, reviewDoc)
	op, ok := s.Find("POST", "/items/42")
	if !ok {
		t.Fatal("not found")
	}
	out := s.ScaffoldNode(op, "api", NodeID(op.Key))

	var parsed []map[string]any // the output must be valid YAML as a node list
	if err := yaml.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("scaffold is not valid YAML: %v\n%s", err, out)
	}
	req := parsed[0]["request"].(map[string]any)
	headers := req["headers"].(map[string]any)
	if headers["x-access-token"] != "{{ .secrets.X_ACCESS_TOKEN }}" {
		t.Errorf("auth header: %v", headers)
	}
	if headers["X-Request-Id"] != `{{ index .env "X-Request-Id" }}` {
		t.Errorf("non-auth required header must come from env, got %v", headers["X-Request-Id"])
	}
	if headers["Cookie"] != "session={{ .secrets.COOKIE_SESSION }}" {
		t.Errorf("required cookie: %v", headers["Cookie"])
	}
	query := req["query"].(map[string]any)
	if _, ok := query["page"]; ok {
		t.Error("operation-level page (required:false) must override path-level required:true")
	}
	if query["page-size"] != `{{ index .env "page-size" }}` {
		t.Errorf("unsafe query name: %v", query)
	}
	if req["url"] != `{{ .env.base_url }}/items/{{ index .env "item-id" }}` {
		t.Errorf("unsafe path param: %v", req["url"])
	}
	if !strings.Contains(out, "# Create item Requires a token\n") {
		t.Errorf("summary not collapsed:\n%s", out)
	}
	for _, tpl := range []string{req["url"].(string), headers["X-Request-Id"].(string), query["page-size"].(string)} {
		if _, err := template.New("").Parse(tpl); err != nil {
			t.Errorf("template %q does not parse: %v", tpl, err)
		}
	}

	// Skeleton must satisfy its own contract (minItems, minLength, minimum,
	// exclusiveMinimum, maxLength, allOf + sibling properties).
	issues, err := s.Validate(op.RequestPtr, s.Skeleton(op.RequestSchema))
	if err != nil || len(issues) != 0 {
		t.Fatalf("skeleton violates its schema: %v %v", issues, err)
	}
	body := s.Skeleton(op.RequestSchema).(map[string]any)
	if body["kind"] != "item" || body["qty"] == nil {
		t.Errorf("allOf + siblings not merged: %v", body)
	}
}

func TestNodeIDAlwaysFlowSafe(t *testing.T) {
	re := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	for _, key := range []string{"POST /v1/items:batchGet", "GET /files/~me", "GET /a/@b", "GET /a___b//c", "GET /"} {
		if id := NodeID(key); !re.MatchString(id) || strings.Contains(id, "__") {
			t.Errorf("%s -> %q", key, id)
		}
	}
}

func TestOperationsIncludesTrace(t *testing.T) {
	ops := loadDoc(t, reviewDoc).Operations()
	if !strings.Contains(strings.Join(ops, "|"), "TRACE /items/{item-id}") {
		t.Errorf("%v", ops)
	}
}
