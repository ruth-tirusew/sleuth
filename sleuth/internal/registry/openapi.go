// Package registry loads OpenAPI documents and answers "what does the contract
// say about this operation?" so request/response nodes can be validated.
package registry

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
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
	// Content types documented without a JSON schema (xml, text, form...), which cannot be validated.
	RequestOther  []string
	ResponseOther map[string][]string // status -> content types

	method, path string // lower-case method and path template, for Details
}

// httpMethods are the operation keys of an OpenAPI path item.
var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// Param is a documented path/query/header parameter.
type Param struct {
	Name     string
	In       string // path | query | header | cookie
	Required bool
}

// Fetch reads the raw bytes of an OpenAPI document from a file path or http(s) URL.
func Fetch(src string) ([]byte, error) {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		c := http.Client{Timeout: 15 * time.Second}
		resp, err := c.Get(src)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("GET %s: HTTP %d", src, resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	}
	return os.ReadFile(src)
}

// Load reads an OpenAPI 3.x JSON document from a file path or http(s) URL.
func Load(src string) (*Spec, error) {
	b, err := Fetch(src)
	if err != nil {
		return nil, err
	}
	return Parse(src, b)
}

// Parse reads an OpenAPI 3.x JSON document; src only labels it in errors.
func Parse(src string, b []byte) (*Spec, error) {
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
	// A documented server base path (servers: [{url: /api/v1}]) is not part of the path
	// templates, so try the path with each prefix stripped before the bare path.
	for _, pre := range s.serverPrefixes() {
		if rest, ok := stripPrefix(urlPath, pre); ok {
			if op, found := s.find(method, rest); found {
				return op, true
			}
		}
	}
	return s.find(method, urlPath)
}

var serverVarRe = regexp.MustCompile(`\{([^}]+)\}`)

// serverPrefixes is declaredServerPrefixes, longest first, for matching.
func (s *Spec) serverPrefixes() []string {
	out := s.DeclaredServerPrefixes()
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// DeclaredServerPrefixes returns the path component of each `servers[].url` in document
// order, with server variables replaced by their defaults. Root and empty paths are dropped.
func (s *Spec) DeclaredServerPrefixes() []string {
	servers, _ := s.doc["servers"].([]any)
	var out []string
	for _, sv := range servers {
		m, _ := sv.(map[string]any)
		raw, _ := m["url"].(string)
		vars, _ := m["variables"].(map[string]any)
		raw = serverVarRe.ReplaceAllStringFunc(raw, func(v string) string {
			name := v[1 : len(v)-1]
			if def, ok := vars[name].(map[string]any); ok {
				if d, ok := def["default"].(string); ok {
					return d
				}
			}
			return v
		})
		path := raw
		if i := strings.Index(raw, "://"); i >= 0 {
			rest := raw[i+3:]
			if j := strings.Index(rest, "/"); j >= 0 {
				path = rest[j:]
			} else {
				path = ""
			}
		}
		path = "/" + strings.Trim(path, "/")
		if path != "/" {
			out = append(out, path)
		}
	}
	return out
}

// stripPrefix removes prefix from p at a segment boundary.
func stripPrefix(p, prefix string) (string, bool) {
	if p == prefix {
		return "/", true
	}
	if strings.HasPrefix(p, prefix+"/") {
		return p[len(prefix):], true
	}
	return "", false
}

func (s *Spec) find(method, urlPath string) (*Operation, bool) {
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
	o := s.operation(strings.ToUpper(method)+" "+best, "#/paths/"+ptrEsc(best)+"/"+method, op)
	o.method, o.path = method, best
	return o, true
}

// Details returns an operation's summary and parameters. Path-level parameters are
// merged with operation-level ones; per OpenAPI, the operation's copy of a
// (name, in) pair overrides the path's.
func (s *Spec) Details(o *Operation) (summary string, params []Param) {
	item, _ := s.doc["paths"].(map[string]any)[o.path].(map[string]any)
	op, _ := item[o.method].(map[string]any)
	summary, _ = op["summary"].(string)
	index := map[[2]string]int{}
	for _, src := range []any{item["parameters"], op["parameters"]} { // path-level first, then overrides
		list, _ := src.([]any)
		for _, p := range list {
			pm, _ := p.(map[string]any)
			pm, _ = s.deref(pm, "")
			name, _ := pm["name"].(string)
			in, _ := pm["in"].(string)
			req, _ := pm["required"].(bool)
			if name == "" {
				continue
			}
			key := [2]string{name, in}
			if i, seen := index[key]; seen {
				params[i] = Param{name, in, req}
				continue
			}
			index[key] = len(params)
			params = append(params, Param{name, in, req})
		}
	}
	return summary, params
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
		if o.RequestSchema == nil {
			o.RequestOther = otherContentTypes(rb)
		}
	}
	resps, _ := op["responses"].(map[string]any)
	for status, r := range resps {
		rm, _ := r.(map[string]any)
		rm, ptr := s.deref(rm, base+"/responses/"+status)
		o.Responses[status], o.ResponsePtrs[status] = jsonSchema(rm, ptr)
		if o.Responses[status] == nil {
			if other := otherContentTypes(rm); len(other) > 0 {
				if o.ResponseOther == nil {
					o.ResponseOther = map[string][]string{}
				}
				o.ResponseOther[status] = other
			}
		}
	}
	return o
}

// jsonSchema extracts the application/json schema of a request body or response
// object, and its location in the document.
func jsonSchema(obj map[string]any, ptr string) (any, string) {
	content, _ := obj["content"].(map[string]any)
	// Map iteration is random, so pick deterministically: application/json first,
	// then the other +json types in sorted order.
	var mts []string
	for mt := range content {
		if mt == "application/json" || strings.HasSuffix(mt, "+json") {
			mts = append(mts, mt)
		}
	}
	sort.Slice(mts, func(i, j int) bool {
		if (mts[i] == "application/json") != (mts[j] == "application/json") {
			return mts[i] == "application/json"
		}
		return mts[i] < mts[j]
	})
	for _, mt := range mts {
		m, _ := content[mt].(map[string]any)
		if sc, ok := m["schema"]; ok {
			return sc, ptr + "/content/" + ptrEsc(mt) + "/schema"
		}
	}
	return nil, ""
}

// otherContentTypes lists the documented content types of a body (sorted), for bodies
// that have no JSON schema.
func otherContentTypes(obj map[string]any) []string {
	content, _ := obj["content"].(map[string]any)
	var out []string
	for mt := range content {
		out = append(out, mt)
	}
	sort.Strings(out)
	return out
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

// Operations lists every "METHOD /path" in the document, sorted.
func (s *Spec) Operations() []string {
	var out []string
	for tpl, item := range s.doc["paths"].(map[string]any) {
		m, _ := item.(map[string]any)
		for _, method := range httpMethods {
			if _, ok := m[method]; ok {
				out = append(out, strings.ToUpper(method)+" "+tpl)
			}
		}
	}
	sort.Strings(out)
	return out
}
