package registry

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Skeleton builds a minimal example value for a schema: required properties only,
// default / const / first enum value, otherwise the smallest value that satisfies
// minimum / exclusiveMinimum / minLength / minItems. It is best effort: `pattern`
// and `format` are not synthesised, and schemas nested deeper than 8 levels yield null.
func (s *Spec) Skeleton(schema any) any { return s.skeleton(schema, 0) }

func (s *Spec) skeleton(schema any, depth int) any {
	sc, _ := schema.(map[string]any)
	if sc == nil || depth > 8 {
		return nil
	}
	if ref, ok := sc["$ref"].(string); ok {
		return s.skeleton(s.resolve(ref), depth+1)
	}
	if d, ok := sc["default"]; ok {
		return plain(d)
	}
	if c, ok := sc["const"]; ok {
		return plain(c)
	}
	if en, _ := sc["enum"].([]any); len(en) > 0 {
		return plain(en[0])
	}
	for _, kw := range []string{"anyOf", "oneOf"} {
		if alts, _ := sc[kw].([]any); len(alts) > 0 {
			for _, a := range alts { // first non-null branch
				if m, _ := a.(map[string]any); m["type"] != "null" {
					return s.skeleton(a, depth+1)
				}
			}
		}
	}
	if all, _ := sc["allOf"].([]any); len(all) > 0 {
		merged := map[string]any{}
		for _, a := range all {
			if m, ok := s.skeleton(a, depth+1).(map[string]any); ok {
				for k, v := range m {
					merged[k] = v
				}
			}
		}
		// Sibling properties/required next to allOf apply too.
		if own, ok := s.object(sc, depth).(map[string]any); ok {
			for k, v := range own {
				merged[k] = v
			}
		}
		return merged
	}
	switch schemaType(sc) {
	case "object":
		return s.object(sc, depth)
	case "array":
		n := int(number(sc["minItems"], 0))
		out := make([]any, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, s.skeleton(sc["items"], depth+1))
		}
		return out
	case "string":
		v := "string"
		if min := int(number(sc["minLength"], 0)); len(v) < min {
			v += strings.Repeat("x", min-len(v))
		}
		if max, ok := sc["maxLength"]; ok && len(v) > int(number(max, 0)) {
			v = v[:int(number(max, 0))]
		}
		return v
	case "integer":
		return int64(math.Ceil(lowerBound(sc, 1)))
	case "number":
		return lowerBound(sc, 0.5)
	case "boolean":
		return false
	}
	if _, ok := sc["properties"]; ok {
		return s.object(sc, depth)
	}
	return nil
}

// object builds the required properties of an object schema.
func (s *Spec) object(sc map[string]any, depth int) any {
	if _, hasProps := sc["properties"]; !hasProps && sc["required"] == nil && schemaType(sc) != "object" {
		return nil
	}
	out := map[string]any{}
	props, _ := sc["properties"].(map[string]any)
	req, _ := sc["required"].([]any)
	for _, r := range req {
		if name, _ := r.(string); name != "" {
			out[name] = s.skeleton(props[name], depth+1)
		}
	}
	return out
}

func schemaType(sc map[string]any) string {
	if t, ok := sc["type"].(string); ok {
		return t
	}
	if tl, _ := sc["type"].([]any); len(tl) > 0 {
		for _, x := range tl { // prefer a non-null type
			if t, _ := x.(string); t != "null" {
				return t
			}
		}
	}
	return ""
}

// lowerBound is the smallest value allowed by minimum/exclusiveMinimum (step past
// an exclusive bound), or 0 when unbounded below, capped by maximum.
func lowerBound(sc map[string]any, step float64) float64 {
	v := 0.0
	if m, ok := sc["minimum"]; ok {
		v = number(m, 0)
	}
	if m, ok := sc["exclusiveMinimum"]; ok {
		if m == true { // OpenAPI 3.0: boolean modifier on minimum
			v += step
		} else if f := number(m, math.NaN()); !math.IsNaN(f) && v <= f {
			v = f + step
		}
	}
	if m, ok := sc["maximum"]; ok && v > number(m, v) {
		v = number(m, v)
	}
	return v
}

// number reads a schema number (json.Number in a document parsed with UseNumber).
func number(v any, def float64) float64 {
	switch x := v.(type) {
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return f
		}
	case float64:
		return x
	case int:
		return float64(x)
	}
	return def
}

// plain converts json.Number leaves to int64/float64 so they marshal as YAML numbers.
func plain(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = plain(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = plain(e)
		}
		return out
	}
	return v
}

// ScaffoldNode renders a starter request node plus a status assertion for one
// operation, ready to paste into a flow. Parameters become references the user
// fills in: path/query/non-auth headers → .env, auth headers and cookies → .secrets.
func (s *Spec) ScaffoldNode(op *Operation, source, id string) string {
	summary, params := s.Details(op)
	method, tpl, _ := strings.Cut(op.Key, " ")
	var b strings.Builder
	if summary = strings.Join(strings.Fields(summary), " "); summary != "" {
		fmt.Fprintf(&b, "  # %s\n", summary)
	}
	fmt.Fprintf(&b, "  - id: %s\n    kind: request\n    contract: %s\n    request:\n", id, source)
	if method != "GET" {
		fmt.Fprintf(&b, "      method: %s\n", method)
	}
	url := tpl
	for _, p := range params {
		if p.In == "path" {
			url = strings.ReplaceAll(url, "{"+p.Name+"}", ref("env", p.Name))
		}
	}
	fmt.Fprintf(&b, "      url: %s\n", yamlString("{{ .env.base_url }}"+url))

	var headers, query, cookies, optional []string
	for _, p := range params {
		switch {
		case p.In == "header" && p.Required && isAuthHeader(p.Name):
			headers = append(headers, fmt.Sprintf("        %s: %s", yamlKey(p.Name), yamlString(ref("secrets", secretName(p.Name)))))
		case p.In == "header" && p.Required:
			headers = append(headers, fmt.Sprintf("        %s: %s", yamlKey(p.Name), yamlString(ref("env", p.Name))))
		case p.In == "header" && isAuthHeader(p.Name): // optional auth header: offer, don't force
			optional = append(optional, fmt.Sprintf("header %s (secret %s)", p.Name, secretName(p.Name)))
		case p.In == "query" && p.Required:
			query = append(query, fmt.Sprintf("        %s: %s", yamlKey(p.Name), yamlString(ref("env", p.Name))))
		case p.In == "query":
			optional = append(optional, "query "+p.Name)
		case p.In == "cookie" && p.Required:
			cookies = append(cookies, p.Name+"="+ref("secrets", secretName("cookie_"+p.Name)))
		case p.In == "cookie":
			optional = append(optional, "cookie "+p.Name)
		}
	}
	if len(cookies) > 0 {
		sort.Strings(cookies)
		headers = append(headers, "        Cookie: "+yamlString(strings.Join(cookies, "; ")))
	}
	if len(headers) > 0 {
		sort.Strings(headers)
		b.WriteString("      headers:\n" + strings.Join(headers, "\n") + "\n")
	}
	if len(query) > 0 {
		sort.Strings(query)
		b.WriteString("      query:\n" + strings.Join(query, "\n") + "\n")
	}
	if len(optional) > 0 {
		sort.Strings(optional)
		fmt.Fprintf(&b, "      # optional (not emitted): %s\n", strings.Join(optional, "; "))
	}
	if op.RequestSchema != nil {
		if sk := s.Skeleton(op.RequestSchema); sk != nil {
			y, _ := yaml.Marshal(sk)
			b.WriteString("      body:\n")
			for _, line := range strings.Split(strings.TrimRight(string(y), "\n"), "\n") {
				b.WriteString("        " + line + "\n")
			}
		}
	}
	fmt.Fprintf(&b, "\n  - id: %s_ok\n    kind: assertion\n    assert:\n      - path: nodes.%s.response.status\n        equals: %d\n", id, id, op.firstSuccess())
	return b.String()
}

var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ref renders a template reference that parses for any key: `{{ .env.page }}`, or
// `{{ index .env "page-size" }}` when the name isn't a Go identifier.
func ref(root, name string) string {
	if identRe.MatchString(name) {
		return "{{ ." + root + "." + name + " }}"
	}
	return fmt.Sprintf("{{ index .%s %q }}", root, name)
}

// yamlString quotes a scalar safely (templates contain quotes once `index` is used).
func yamlString(s string) string {
	b, _ := json.Marshal(s) // a JSON string is a valid YAML double-quoted scalar
	return string(b)
}

func yamlKey(s string) string {
	if identRe.MatchString(strings.ReplaceAll(s, "-", "_")) {
		return s
	}
	return yamlString(s)
}

func (o *Operation) firstSuccess() int {
	best := 0
	for k := range o.Responses {
		var n int
		if _, err := fmt.Sscanf(k, "%d", &n); err == nil && n >= 200 && n < 300 && (best == 0 || n < best) {
			best = n
		}
	}
	if best == 0 {
		return 200
	}
	return best
}

func isAuthHeader(n string) bool {
	n = strings.ToLower(n)
	return n == "authorization" || strings.Contains(n, "token") || strings.Contains(n, "api-key") ||
		strings.Contains(n, "apikey") || strings.Contains(n, "secret")
}

var nonIdent = regexp.MustCompile(`[^A-Za-z0-9_]+`)

func secretName(h string) string {
	return strings.ToUpper(strings.Trim(nonIdent.ReplaceAllString(h, "_"), "_"))
}

var underscores = regexp.MustCompile(`_+`)

// NodeID derives a flow-safe id (matching ^[A-Za-z_][A-Za-z0-9_]*$) from an operation
// key: "GET /loanProcess/{id}" -> get_loanProcess_id.
func NodeID(key string) string {
	method, path, _ := strings.Cut(key, " ")
	id := strings.ToLower(method) + "_" + nonIdent.ReplaceAllString(path, "_")
	id = strings.Trim(underscores.ReplaceAllString(id, "_"), "_")
	if id == "" || !identRe.MatchString(id) {
		id = "op_" + id
	}
	return id
}
