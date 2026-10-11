package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const doc = `{
 "openapi": "3.1.0",
 "paths": {
  "/users/{id}": {"get": {"responses": {
     "200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/User"}}}},
     "422": {"content": {"application/json": {"schema": {}}}}}}},
  "/users/me": {"get": {"responses": {"200": {}}}},
  "/users": {"post": {"requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/NewUser"}}}},
     "responses": {"2XX": {}, "default": {}}}}
 },
 "components": {"schemas": {
  "User": {"type": "object", "required": ["id", "name"], "properties": {
     "id": {"type": "integer"}, "name": {"type": "string", "minLength": 1},
     "role": {"enum": ["admin", "user"]},
     "nick": {"anyOf": [{"type": "string"}, {"type": "null"}]},
     "tags": {"type": "array", "items": {"type": "string"}}}},
  "NewUser": {"type": "object", "required": ["name"], "additionalProperties": false,
     "properties": {"name": {"type": "string"}}}
 }}}`

func load(t *testing.T) *Spec {
	p := filepath.Join(t.TempDir(), "o.json")
	os.WriteFile(p, []byte(doc), 0o644)
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestFindPrefersLiteralOverTemplate(t *testing.T) {
	s := load(t)
	op, ok := s.Find("GET", "/users/me")
	if !ok || op.Key != "GET /users/me" {
		t.Fatalf("%+v", op)
	}
	op, ok = s.Find("get", "/users/42")
	if !ok || op.Key != "GET /users/{id}" {
		t.Fatalf("%+v", op)
	}
	if _, ok := s.Find("DELETE", "/users/42"); ok {
		t.Fatal("method must matter")
	}
	if _, ok := s.Find("GET", "/users/42/extra"); ok {
		t.Fatal("segment count must matter")
	}
}

func TestResponseFor(t *testing.T) {
	op, _ := load(t).Find("POST", "/users")
	if _, ok := op.ResponseFor(201); !ok {
		t.Fatal("2XX should cover 201")
	}
	if _, ok := op.ResponseFor(500); !ok {
		t.Fatal("default should cover 500")
	}
	g, _ := load(t).Find("GET", "/users/1")
	if _, ok := g.ResponseFor(401); ok {
		t.Fatal("401 is undocumented for GET /users/{id}")
	}
}

func TestValidate(t *testing.T) {
	s := load(t)
	op, _ := s.Find("GET", "/users/1")
	ptr := op.ResponsePtrFor(200)
	cases := []struct {
		name string
		val  any
		want string // substring of first issue, "" = valid
	}{
		{"valid", map[string]any{"id": 1, "name": "a", "role": "admin", "nick": nil, "tags": []any{"x"}}, ""},
		{"missing required", map[string]any{"id": 1}, "missing property"},
		{"wrong type", map[string]any{"id": "1", "name": "a"}, "want integer"},
		{"non-integer", map[string]any{"id": 1.5, "name": "a"}, "want integer"},
		{"bad enum", map[string]any{"id": 1, "name": "a", "role": "root"}, "must be"},
		{"minLength", map[string]any{"id": 1, "name": ""}, "minLength"},
		{"anyOf none", map[string]any{"id": 1, "name": "a", "nick": 5}, "anyOf"},
		{"array item", map[string]any{"id": 1, "name": "a", "tags": []any{"x", 2}}, "want string"},
	}
	for _, c := range cases {
		issues, err := s.Validate(ptr, c.val)
		if err != nil {
			t.Fatal(err)
		}
		if c.want == "" && len(issues) != 0 {
			t.Errorf("%s: unexpected %v", c.name, issues)
		}
		if c.want != "" && (len(issues) == 0 || !strings.Contains(issues[0].Message, c.want)) {
			t.Errorf("%s: want %q, got %v", c.name, c.want, issues)
		}
	}
	// additionalProperties:false and path reporting
	post, _ := s.Find("POST", "/users")
	is, _ := s.Validate(post.RequestPtr, map[string]any{"name": "a", "extra": 1})
	if len(is) != 1 || !strings.Contains(is[0].Message, "extra") {
		t.Fatalf("%v", is)
	}
	is, _ = s.Validate(ptr, map[string]any{"id": 1, "name": "a", "tags": []any{"x", 2}})
	if is[0].Path != "$.tags[1]" {
		t.Fatalf("path %q", is[0].Path)
	}
}

func TestOpenAPI30NullableIsUpgraded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.json")
	os.WriteFile(p, []byte(`{"openapi":"3.0.3","paths":{"/x":{"get":{"responses":{"200":{"content":{"application/json":{"schema":
	  {"type":"object","properties":{"n":{"type":"string","nullable":true}}}}}}}}}}}`), 0o644)
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	op, _ := s.Find("GET", "/x")
	if is, _ := s.Validate(op.ResponsePtrFor(200), map[string]any{"n": nil}); len(is) != 0 {
		t.Fatalf("nullable null rejected: %v", is)
	}
	if is, _ := s.Validate(op.ResponsePtrFor(200), map[string]any{"n": 3}); len(is) == 0 {
		t.Fatal("non-string must still fail")
	}
}

func TestJSONSchemaPickIsDeterministic(t *testing.T) {
	obj := map[string]any{"content": map[string]any{
		"application/problem+json": map[string]any{"schema": map[string]any{"title": "problem"}},
		"application/json":         map[string]any{"schema": map[string]any{"title": "plain"}},
		"application/a+json":       map[string]any{"schema": map[string]any{"title": "a"}},
	}}
	for i := 0; i < 50; i++ {
		sc, ptr := jsonSchema(obj, "#/r")
		if sc.(map[string]any)["title"] != "plain" || ptr != "#/r/content/application~1json/schema" {
			t.Fatalf("run %d: %v %s", i, sc, ptr)
		}
	}
	delete(obj["content"].(map[string]any), "application/json")
	if sc, _ := jsonSchema(obj, "#/r"); sc.(map[string]any)["title"] != "a" {
		t.Fatalf("want sorted +json fallback, got %v", sc)
	}
}
