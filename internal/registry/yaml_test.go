package registry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestToJSONReadsOpenAPIYAML(t *testing.T) {
	src := `
openapi: 3.0.3
info: {title: t, version: "1"}
x-common: &errors
  "500": {description: server error}
paths:
  /items/{id}:
    get:
      responses:
        200:
          description: ok
          content:
            application/json:
              schema:
                type: object
                properties:
                  price: {type: number, example: 10.50}
                  big: {type: integer, example: 9007199254740993}
                  created: {type: string, format: date, example: 2024-01-01}
        <<: *errors
`
	b, err := ToJSON([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"200":`, `"500":`, `"example":10.50`, `"example":9007199254740993`, `"example":"2024-01-01"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
	if !json.Valid(b) {
		t.Fatal("not valid JSON")
	}
	same, _ := ToJSON([]byte(`{"openapi":"3.1.0"}`))
	if string(same) != `{"openapi":"3.1.0"}` {
		t.Error("JSON passes through unchanged")
	}
	if _, err := ToJSON([]byte("a: [unclosed")); err == nil {
		t.Error("broken YAML is an error")
	}
}

func TestLoadYAMLFileAndValidate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "openapi.yaml")
	os.WriteFile(p, []byte(`openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /x:
    get:
      responses:
        200:
          description: ok
          content:
            application/json:
              schema: {type: object, required: [n], properties: {n: {type: integer, nullable: true}}}
`), 0o644)
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	op, ok := s.Find("GET", "/x")
	if !ok {
		t.Fatal("operation not found")
	}
	if issues, err := s.Validate(op.ResponsePtrFor(200), map[string]any{"n": nil}); err != nil || len(issues) != 0 {
		t.Errorf("valid body: %v %v", issues, err)
	}
	if issues, _ := s.Validate(op.ResponsePtrFor(200), map[string]any{"n": "x"}); len(issues) == 0 {
		t.Error("an invalid body must be reported")
	}
}

// Every schema of a real document must compile, or every call to it would be a false
// violation. SLEUTH_TEST_OPENAPI=path/to/openapi.yaml go test ./internal/registry -run EverySchema
func TestEverySchemaCompiles(t *testing.T) {
	loc := os.Getenv("SLEUTH_TEST_OPENAPI")
	if loc == "" {
		t.Skip("SLEUTH_TEST_OPENAPI not set")
	}
	s, err := Load(loc)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, key := range s.Operations() {
		method, path, _ := strings.Cut(key, " ")
		op, _ := s.Find(method, path)
		ptrs := []string{op.RequestPtr}
		for _, p := range op.ResponsePtrs {
			ptrs = append(ptrs, p)
		}
		for _, p := range ptrs {
			if p == "" {
				continue
			}
			n++
			if _, err := s.schema(p); err != nil {
				t.Errorf("%s: %v", key, err)
			}
		}
	}
	t.Logf("%d operations, %d schemas compiled", len(s.Operations()), n)
}
