package registry

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Severity classifies a change between two versions of a published OpenAPI document,
// from the point of view of a client that was written against the old one.
type Severity string

const (
	// Breaking: a client built on the old document can fail against the new one.
	Breaking Severity = "breaking"
	// Review: the document changed in a way that cannot be classified mechanically
	// (a oneOf was rewritten, a pattern changed); a person should look.
	Review Severity = "review"
	// Info: compatible (something was added, or a restriction was relaxed for you).
	Info Severity = "info"
)

// Change is one difference between two documents.
type Change struct {
	Severity  Severity `json:"severity"`
	Operation string   `json:"operation,omitempty"` // "GET /users/{id}"; empty for document-level changes
	Where     string   `json:"where"`               // operation | parameter | request | response
	Path      string   `json:"path,omitempty"`      // location inside the body or parameter, e.g. "$.items[].name"
	Message   string   `json:"message"`
}

func (s Severity) rank() int {
	switch s {
	case Breaking:
		return 0
	case Review:
		return 1
	}
	return 2
}

// Diff compares two documents and lists what changed, breaking changes first.
//
// Direction matters. A request is what a client produces, so tightening it (a new
// required field, a narrower type, a removed enum value) breaks clients. A response is
// what a client consumes, so loosening it (a removed or now-optional field, a new enum
// value, a new type) breaks clients. The same edit can therefore be breaking in one place
// and harmless in the other, and each rule below is applied accordingly.
func Diff(oldS, newS *Spec) []Change {
	var out []Change
	oldOps, newOps := oldS.operationMap(), newS.operationMap()

	keys := map[string]bool{}
	for k := range oldOps {
		keys[k] = true
	}
	for k := range newOps {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	for _, k := range sorted {
		o, inOld := oldOps[k]
		n, inNew := newOps[k]
		switch {
		case inOld && !inNew:
			out = append(out, Change{Severity: Breaking, Operation: k, Where: "operation", Message: "operation removed"})
		case !inOld && inNew:
			out = append(out, Change{Severity: Info, Operation: k, Where: "operation", Message: "operation added"})
		default:
			c := &differ{o: oldS, n: newS, op: k, out: &out}
			c.operation(o, n)
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Severity.rank() != b.Severity.rank() {
			return a.Severity.rank() < b.Severity.rank()
		}
		if a.Operation != b.Operation {
			return a.Operation < b.Operation
		}
		if a.Where != b.Where {
			return a.Where < b.Where
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Message < b.Message
	})
	return out
}

// opEntry is an operation with the parameters it inherits from its path item.
type opEntry struct {
	op     map[string]any
	params map[string]map[string]any // "in:name" -> resolved parameter
}

func (s *Spec) operationMap() map[string]opEntry {
	out := map[string]opEntry{}
	paths, _ := s.doc["paths"].(map[string]any)
	for tpl, raw := range paths {
		item, _ := raw.(map[string]any)
		for _, method := range httpMethods {
			op, ok := item[method].(map[string]any)
			if !ok {
				continue
			}
			params := map[string]map[string]any{}
			for _, src := range []any{item["parameters"], op["parameters"]} { // path-level first; the operation overrides
				list, _ := src.([]any)
				for _, p := range list {
					pm, _ := p.(map[string]any)
					pm, _ = s.deref(pm, "")
					name, _ := pm["name"].(string)
					in, _ := pm["in"].(string)
					if name != "" {
						params[in+":"+name] = pm
					}
				}
			}
			out[strings.ToUpper(method)+" "+tpl] = opEntry{op: op, params: params}
		}
	}
	return out
}

type differ struct {
	o, n *Spec
	op   string
	out  *[]Change

	// refs being expanded right now, to stop at recursive schemas
	stack map[string]bool
}

func (d *differ) add(sev Severity, where, path, format string, args ...any) {
	*d.out = append(*d.out, Change{Severity: sev, Operation: d.op, Where: where, Path: path, Message: fmt.Sprintf(format, args...)})
}

func (d *differ) operation(o, n opEntry) {
	if dep, _ := n.op["deprecated"].(bool); dep {
		if was, _ := o.op["deprecated"].(bool); !was {
			d.add(Info, "operation", "", "operation is now deprecated")
		}
	}
	if !sameJSON(o.op["security"], n.op["security"]) {
		d.add(Review, "operation", "", "security requirements changed")
	}

	// parameters
	pkeys := map[string]bool{}
	for k := range o.params {
		pkeys[k] = true
	}
	for k := range n.params {
		pkeys[k] = true
	}
	names := make([]string, 0, len(pkeys))
	for k := range pkeys {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		op, inOld := o.params[k]
		np, inNew := n.params[k]
		label := strings.Replace(k, ":", " parameter ", 1)
		oreq, _ := op["required"].(bool)
		nreq, _ := np["required"].(bool)
		switch {
		case inOld && !inNew:
			d.add(Info, "parameter", k, "%s no longer documented", label)
		case !inOld && inNew && nreq:
			d.add(Breaking, "parameter", k, "new required %s", label)
		case !inOld && inNew:
			d.add(Info, "parameter", k, "new optional %s", label)
		default:
			if !oreq && nreq {
				d.add(Breaking, "parameter", k, "%s is now required", label)
			} else if oreq && !nreq {
				d.add(Info, "parameter", k, "%s is no longer required", label)
			}
			d.schema("request", "parameter", k, op["schema"], np["schema"], "$")
		}
	}

	// request body
	ob, oHas := d.body(d.o, o.op["requestBody"])
	nb, nHas := d.body(d.n, n.op["requestBody"])
	switch {
	case !oHas && nHas:
		if nb.required {
			d.add(Breaking, "request", "$", "a request body is now required")
		} else {
			d.add(Info, "request", "$", "an optional request body was added")
		}
	case oHas && !nHas:
		d.add(Review, "request", "$", "request body no longer documented")
	case oHas && nHas:
		if !ob.required && nb.required {
			d.add(Breaking, "request", "$", "the request body is now required")
		}
		switch {
		case ob.schema != nil && nb.schema != nil:
			d.schema("request", "request", "", ob.schema, nb.schema, "$")
		case ob.schema != nil:
			d.add(Review, "request", "$", "request body no longer has a JSON schema (now %s)", orNone(nb.other))
		case nb.schema != nil:
			d.add(Info, "request", "$", "request body now has a JSON schema")
		}
	}

	// responses
	ores, _ := o.op["responses"].(map[string]any)
	nres, _ := n.op["responses"].(map[string]any)
	statuses := map[string]bool{}
	for k := range ores {
		statuses[k] = true
	}
	for k := range nres {
		statuses[k] = true
	}
	codes := make([]string, 0, len(statuses))
	for k := range statuses {
		codes = append(codes, k)
	}
	sort.Strings(codes)
	for _, code := range codes {
		or, inOld := ores[code]
		nr, inNew := nres[code]
		switch {
		case inOld && !inNew:
			if strings.HasPrefix(code, "2") {
				d.add(Breaking, "response", code, "documented success response %s removed", code)
			} else {
				d.add(Info, "response", code, "response %s no longer documented", code)
			}
		case !inOld && inNew:
			d.add(Info, "response", code, "response %s added", code)
		default:
			os, _ := d.o.deref(asMap(or), "")
			ns, _ := d.n.deref(asMap(nr), "")
			osch, _ := jsonSchema(os, "")
			nsch, _ := jsonSchema(ns, "")
			switch {
			case osch != nil && nsch != nil:
				d.schema("response", "response", code, osch, nsch, "$")
			case osch != nil:
				d.add(Review, "response", code, "response %s no longer has a JSON schema", code)
			case nsch != nil:
				d.add(Info, "response", code, "response %s now has a JSON schema", code)
			}
		}
	}
}

type bodyInfo struct {
	required bool
	schema   any
	other    []string
}

func (d *differ) body(s *Spec, raw any) (bodyInfo, bool) {
	m, ok := raw.(map[string]any)
	if !ok {
		return bodyInfo{}, false
	}
	m, _ = s.deref(m, "")
	var b bodyInfo
	b.required, _ = m["required"].(bool)
	b.schema, _ = jsonSchema(m, "")
	if b.schema == nil {
		b.other = otherContentTypes(m)
	}
	return b, true
}

func orNone(v []string) string {
	if len(v) == 0 {
		return "no content"
	}
	return strings.Join(v, ", ")
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// node resolves a schema node (following $ref chains) to a map; nil if it cannot be resolved.
func (d *differ) node(s *Spec, v any) (m map[string]any, ref string) {
	for hops := 0; hops < 16; hops++ {
		mm, ok := v.(map[string]any)
		if !ok {
			return nil, ref
		}
		r, isRef := mm["$ref"].(string)
		if !isRef {
			return mm, ref
		}
		ref = r
		if v = s.resolve(r); v == nil {
			return nil, ref
		}
	}
	return nil, ref
}

// schema compares two schema nodes. dir is "request" (what a client sends) or "response"
// (what a client receives); where/loc name the place for the report; path is the JSON path.
func (d *differ) schema(dir, where, loc string, o, n any, path string) {
	d.compare(dir, where, loc, o, n, path, 0)
}

func (d *differ) compare(dir, where, loc string, o, n any, path string, depth int) {
	if depth > 24 {
		return
	}
	om, oref := d.node(d.o, o)
	nm, nref := d.node(d.n, n)
	if om == nil && nm == nil {
		return
	}
	if om == nil {
		om = map[string]any{}
	}
	if nm == nil {
		nm = map[string]any{}
	}
	if oref != "" || nref != "" { // stop on a recursive schema
		key := dir + "|" + oref + "|" + nref
		if d.stack == nil {
			d.stack = map[string]bool{}
		}
		if d.stack[key] {
			return
		}
		d.stack[key] = true
		defer delete(d.stack, key)
	}
	at := func(sev Severity, format string, args ...any) {
		p := path
		if loc != "" && where == "parameter" {
			p = loc + " " + path
		} else if where == "response" && loc != "" {
			p = loc + " " + path
		}
		d.add(sev, where, strings.TrimSpace(p), format, args...)
	}

	// type
	ot, nt := typeSet(om), typeSet(nm)
	switch {
	case ot == nil && nt == nil:
	case ot == nil:
		if dir == "request" {
			at(Breaking, "now restricted to type %s", setString(nt))
		} else {
			at(Info, "now restricted to type %s", setString(nt))
		}
	case nt == nil:
		if dir == "request" {
			at(Info, "type restriction removed (was %s)", setString(ot))
		} else {
			at(Review, "type restriction removed (was %s); other types may now be returned", setString(ot))
		}
	case !sameSet(ot, nt):
		if dir == "request" {
			if !subset(ot, nt) {
				at(Breaking, "type changed from %s to %s; previously accepted types are rejected", setString(ot), setString(nt))
			} else {
				at(Info, "type widened from %s to %s", setString(ot), setString(nt))
			}
		} else {
			if !subset(nt, ot) {
				at(Breaking, "type changed from %s to %s; clients may receive types they do not expect", setString(ot), setString(nt))
			} else {
				at(Info, "type narrowed from %s to %s", setString(ot), setString(nt))
			}
		}
	}

	// enum
	oe, ne := enumSet(om), enumSet(nm)
	if dir == "request" {
		switch {
		case oe == nil && ne != nil:
			at(Breaking, "enum restriction added (%s)", setString(ne))
		case oe != nil && ne == nil:
			at(Info, "enum restriction removed")
		case oe != nil && ne != nil:
			if rem := minus(oe, ne); len(rem) > 0 {
				at(Breaking, "enum value(s) no longer accepted: %s", setString(rem))
			}
			if add := minus(ne, oe); len(add) > 0 {
				at(Info, "enum value(s) added: %s", setString(add))
			}
		}
	} else if oe != nil && ne != nil {
		if add := minus(ne, oe); len(add) > 0 {
			at(Breaking, "new enum value(s) may be returned: %s", setString(add))
		}
		if rem := minus(oe, ne); len(rem) > 0 {
			at(Info, "enum value(s) no longer returned: %s", setString(rem))
		}
	} else if oe != nil && ne == nil {
		at(Review, "enum restriction removed; any value may now be returned")
	}

	// object properties and required
	op, np := asMap(om["properties"]), asMap(nm["properties"])
	oreq, nreq := stringSet(om["required"]), stringSet(nm["required"])
	names := map[string]bool{}
	for k := range op {
		names[k] = true
	}
	for k := range np {
		names[k] = true
	}
	list := make([]string, 0, len(names))
	for k := range names {
		list = append(list, k)
	}
	sort.Strings(list)
	for _, name := range list {
		child := path + "." + name
		ov, inOld := op[name]
		nv, inNew := np[name]
		sub := func(sev Severity, format string, args ...any) {
			p := child
			if loc != "" && (where == "parameter" || where == "response") {
				p = loc + " " + child
			}
			d.add(sev, where, p, format, args...)
		}
		switch {
		case inOld && !inNew:
			if dir == "response" {
				sub(Breaking, "property removed; clients that read it will find nothing")
			} else if ap, ok := nm["additionalProperties"].(bool); ok && !ap {
				sub(Breaking, "property removed and extra properties are rejected")
			} else {
				sub(Info, "property no longer documented")
			}
		case !inOld && inNew:
			if dir == "request" && nreq[name] {
				sub(Breaking, "new required property")
			} else {
				sub(Info, "property added")
			}
		default:
			switch {
			case dir == "request" && !oreq[name] && nreq[name]:
				sub(Breaking, "property is now required")
			case dir == "request" && oreq[name] && !nreq[name]:
				sub(Info, "property is no longer required")
			case dir == "response" && oreq[name] && !nreq[name]:
				sub(Breaking, "property is no longer guaranteed; it may be absent")
			case dir == "response" && !oreq[name] && nreq[name]:
				sub(Info, "property is now always present")
			}
			d.compare(dir, where, loc, ov, nv, child, depth+1)
		}
	}
	if dir == "request" {
		if ap, ok := nm["additionalProperties"].(bool); ok && !ap {
			if oap, ok := om["additionalProperties"].(bool); !ok || oap {
				at(Breaking, "extra properties are no longer accepted")
			}
		}
	}

	// array items
	if om["items"] != nil || nm["items"] != nil {
		d.compare(dir, where, loc, om["items"], nm["items"], path+"[]", depth+1)
	}

	// bounds on what a client may send
	if dir == "request" {
		for _, k := range []string{"minimum", "exclusiveMinimum", "minLength", "minItems", "minProperties"} {
			ov, oOK := numVal(om[k])
			nv, nOK := numVal(nm[k])
			if nOK && (!oOK || nv > ov) {
				at(Breaking, "%s tightened to %s (was %s)", k, fmtNum(nv), ifSet(oOK, fmtNum(ov)))
			}
		}
		for _, k := range []string{"maximum", "exclusiveMaximum", "maxLength", "maxItems", "maxProperties"} {
			ov, oOK := numVal(om[k])
			nv, nOK := numVal(nm[k])
			if nOK && (!oOK || nv < ov) {
				at(Breaking, "%s tightened to %s (was %s)", k, fmtNum(nv), ifSet(oOK, fmtNum(ov)))
			}
		}
	}

	// things that cannot be classified mechanically
	for _, k := range []string{"pattern", "format"} {
		if ov, nv := om[k], nm[k]; !sameJSON(ov, nv) {
			at(Review, "%s changed from %s to %s", k, jsonText(ov), jsonText(nv))
		}
	}
	for _, k := range []string{"allOf", "oneOf", "anyOf", "not"} {
		if !sameJSON(om[k], nm[k]) {
			at(Review, "%s changed; compare it by hand", k)
		}
	}
}

func typeSet(m map[string]any) map[string]bool {
	switch t := m["type"].(type) {
	case string:
		return map[string]bool{t: true}
	case []any:
		out := map[string]bool{}
		for _, v := range t {
			if s, ok := v.(string); ok {
				out[s] = true
			}
		}
		return out
	}
	return nil
}

func enumSet(m map[string]any) map[string]bool {
	list, ok := m["enum"].([]any)
	if !ok {
		return nil
	}
	out := map[string]bool{}
	for _, v := range list {
		out[jsonText(v)] = true
	}
	return out
}

func stringSet(v any) map[string]bool {
	out := map[string]bool{}
	list, _ := v.([]any)
	for _, x := range list {
		if s, ok := x.(string); ok {
			out[s] = true
		}
	}
	return out
}

// subset reports whether every type in a is allowed by b (an integer is a number).
func subset(a, b map[string]bool) bool {
	for t := range a {
		if !b[t] && !(t == "integer" && b["number"]) {
			return false
		}
	}
	return true
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func minus(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		if !b[k] {
			out[k] = true
		}
	}
	return out
}

func setString(s map[string]bool) string {
	list := make([]string, 0, len(s))
	for k := range s {
		list = append(list, k)
	}
	sort.Strings(list)
	return strings.Join(list, ", ")
}

func numVal(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	case int:
		return float64(x), true
	}
	return 0, false
}

func fmtNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func ifSet(ok bool, s string) string {
	if !ok {
		return "unset"
	}
	return s
}

func sameJSON(a, b any) bool { return jsonText(a) == jsonText(b) }

func jsonText(v any) string {
	if v == nil {
		return "none"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}
