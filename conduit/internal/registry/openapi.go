// Package registry loads OpenAPI documents and answers "what does the contract
// say about this operation?" so request/response nodes can be validated.
package registry

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

type Spec struct {
	Source string
	doc    map[string]any
	// compiled JSON Schemas, by pointer into the document ("#/paths/...").
	comp     *jsonschema.Compiler
	compiled map[string]*jsonschema.Schema
}

// Operation is the contract for one method+path.
type Operation struct {
	Key             string // "GET /customer/{id}"
	RequestSchema   any    // nil when the operation documents no JSON request body
	RequestPtr      string // location of RequestSchema in the document (for Validate)
	RequestRequired bool
	Responses       map[string]any    // status ("200", "2XX", "default") -> schema (may be nil)
	ResponsePtrs    map[string]string // same keys -> location in the document
}

// Load reads an OpenAPI 3.x JSON document from a file path or http(s) URL.
func Load(src string) (*Spec, error) {
	var b []byte
	var err error
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		c := http.Client{Timeout: 15 * time.Second}
		resp, e := c.Get(src)
		if e != nil {
			return nil, e
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("GET %s: HTTP %d", src, resp.StatusCode)
		}
		b, err = io.ReadAll(resp.Body)
	} else {
		b, err = os.ReadFile(src)
	}
	if err != nil {
		return nil, err
	}
	s := &Spec{Source: src, compiled: map[string]*jsonschema.Schema{}}
	// UnmarshalJSON keeps numbers exact (json.Number), which the schema compiler expects.
	raw, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("parse OpenAPI %s: %w", src, err)
	}
	s.doc, _ = raw.(map[string]any)
	if s.doc == nil {
		return nil, fmt.Errorf("%s: not a JSON object", src)
	}
	if v, _ := s.doc["openapi"].(string); !strings.HasPrefix(v, "3.") {
		return nil, fmt.Errorf("%s: not an OpenAPI 3.x document", src)
	}
	if _, ok := s.doc["paths"].(map[string]any); !ok {
		return nil, fmt.Errorf("%s: no paths", src)
	}
	if strings.HasPrefix(s.doc["openapi"].(string), "3.0") {
		upgradeNullable(s.doc) // 3.0's `nullable` is not JSON Schema 2020-12
	}
	s.comp = jsonschema.NewCompiler()
	s.comp.DefaultDraft(jsonschema.Draft2020)
	if err := s.comp.AddResource(specURL, s.doc); err != nil {
		return nil, err
	}
	return s, nil
}

// upgradeNullable rewrites OpenAPI 3.0 `nullable: true` into `type: [T, "null"]`.
func upgradeNullable(v any) {
	switch x := v.(type) {
	case map[string]any:
		if x["nullable"] == true {
			if t, ok := x["type"].(string); ok {
				x["type"] = []any{t, "null"}
			}
		}
		for _, c := range x {
			upgradeNullable(c)
		}
	case []any:
		for _, c := range x {
			upgradeNullable(c)
		}
	}
}

// Find matches a concrete request path against the spec's path templates.
// Literal paths beat templated ones (/users/me over /users/{id}).
func (s *Spec) Find(method, urlPath string) (*Operation, bool) {
	method = strings.ToLower(method)
	paths := s.doc["paths"].(map[string]any)
	keys := make([]string, 0, len(paths))
	for k := range paths {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	best, bestParams := "", 1<<30
	for _, tpl := range keys {
		item, _ := paths[tpl].(map[string]any)
		if _, ok := item[method]; !ok {
			continue
		}
		if params, ok := matchTemplate(tpl, urlPath); ok && params < bestParams {
			best, bestParams = tpl, params
		}
	}
	if best == "" {
		return nil, false
	}
	op, _ := paths[best].(map[string]any)[method].(map[string]any)
	return s.operation(strings.ToUpper(method)+" "+best, "#/paths/"+ptrEsc(best)+"/"+method, op), true
}

func matchTemplate(tpl, path string) (params int, ok bool) {
	a, b := strings.Split(strings.Trim(tpl, "/"), "/"), strings.Split(strings.Trim(path, "/"), "/")
	if len(a) != len(b) {
		return 0, false
	}
	for i := range a {
		switch {
		case strings.HasPrefix(a[i], "{") && strings.HasSuffix(a[i], "}"):
			params++
		case a[i] != b[i]:
			return 0, false
		}
	}
	return params, true
}

func (s *Spec) operation(key, base string, op map[string]any) *Operation {
	o := &Operation{Key: key, Responses: map[string]any{}, ResponsePtrs: map[string]string{}}
	if rb, ok := op["requestBody"].(map[string]any); ok {
		rb, ptr := s.deref(rb, base+"/requestBody")
		o.RequestRequired, _ = rb["required"].(bool)
		o.RequestSchema, o.RequestPtr = jsonSchema(rb, ptr)
	}
	resps, _ := op["responses"].(map[string]any)
	for status, r := range resps {
		rm, _ := r.(map[string]any)
		rm, ptr := s.deref(rm, base+"/responses/"+status)
		o.Responses[status], o.ResponsePtrs[status] = jsonSchema(rm, ptr)
	}
	return o
}

// jsonSchema extracts the application/json schema of a request body or response
// object, and its location in the document.
func jsonSchema(obj map[string]any, ptr string) (any, string) {
	content, _ := obj["content"].(map[string]any)
	for mt, v := range content {
		if mt == "application/json" || strings.HasSuffix(mt, "+json") {
			m, _ := v.(map[string]any)
			if sc, ok := m["schema"]; ok {
				return sc, ptr + "/content/" + ptrEsc(mt) + "/schema"
			}
		}
	}
	return nil, ""
}

func ptrEsc(s string) string { return strings.NewReplacer("~", "~0", "/", "~1").Replace(s) }

// ResponseFor picks the documented response for a status: exact, then "2XX", then "default".
func (o *Operation) ResponseFor(status int) (schema any, documented bool) {
	s := strconv.Itoa(status)
	for _, k := range []string{s, s[:1] + "XX", "default"} {
		if sc, ok := o.Responses[k]; ok {
			return sc, true
		}
	}
	return nil, false
}

// ResponsePtrFor is ResponseFor's location in the document, for Validate.
func (o *Operation) ResponsePtrFor(status int) string {
	s := strconv.Itoa(status)
	for _, k := range []string{s, s[:1] + "XX", "default"} {
		if _, ok := o.Responses[k]; ok {
			return o.ResponsePtrs[k]
		}
	}
	return ""
}

// deref follows a local "$ref" object once, returning the target and its location.
func (s *Spec) deref(m map[string]any, ptr string) (map[string]any, string) {
	if ref, ok := m["$ref"].(string); ok {
		if t, ok := s.resolve(ref).(map[string]any); ok {
			return t, ref
		}
	}
	return m, ptr
}

func (s *Spec) resolve(ref string) any {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	var cur any = s.doc
	for _, part := range strings.Split(ref[2:], "/") {
		part = strings.NewReplacer("~1", "/", "~0", "~").Replace(part)
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}
