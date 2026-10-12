// Package shape records the structure of JSON responses, without their values, and compares
// a response against a recorded structure from the point of view of a client written against
// the recording. See docs/decisions/0007-behavioural-snapshots.md.
package shape

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"sort"
)

// Shape is the structure of a JSON value: the set of types it was seen as, an object's fields,
// an array's item shape. It holds no values.
type Shape struct {
	Type   []string         `json:"type"`
	Fields map[string]Field `json:"fields,omitempty"`
	Items  *Shape           `json:"items,omitempty"`
}

// Field is an object member. Optional means it was missing in some observation of the object.
type Field struct {
	Shape
	Optional bool `json:"optional,omitempty"`
}

// JSON type names, as in JSON Schema.
const (
	TString  = "string"
	TInteger = "integer"
	TNumber  = "number"
	TBoolean = "boolean"
	TNull    = "null"
	TObject  = "object"
	TArray   = "array"
)

const (
	maxDepth = 32  // deeper structure is recorded as an opaque object
	maxItems = 200 // array elements sampled per array
)

var intLiteral = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// Infer returns the shape of a decoded JSON value (map[string]any, []any, string, bool, nil,
// json.Number or float64). Numbers written without a fraction or exponent are integers.
func Infer(v any) Shape { return infer(v, 0) }

func infer(v any, depth int) Shape {
	if depth > maxDepth {
		return Shape{Type: []string{TObject}}
	}
	switch x := v.(type) {
	case nil:
		return Shape{Type: []string{TNull}}
	case bool:
		return Shape{Type: []string{TBoolean}}
	case string:
		return Shape{Type: []string{TString}}
	case json.Number:
		if intLiteral.MatchString(string(x)) {
			return Shape{Type: []string{TInteger}}
		}
		return Shape{Type: []string{TNumber}}
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) {
			return Shape{Type: []string{TInteger}}
		}
		return Shape{Type: []string{TNumber}}
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return Shape{Type: []string{TInteger}}
	case float32:
		return infer(float64(x), depth)
	case map[string]any:
		s := Shape{Type: []string{TObject}, Fields: make(map[string]Field, len(x))}
		for k, e := range x {
			s.Fields[k] = Field{Shape: infer(e, depth+1)}
		}
		return s
	case []any:
		s := Shape{Type: []string{TArray}}
		for i, e := range x {
			if i == maxItems {
				break
			}
			item := infer(e, depth+1)
			if s.Items == nil {
				s.Items = &item
			} else {
				m := Merge(*s.Items, item)
				s.Items = &m
			}
		}
		return s
	}
	// any other Go type (a typed slice or map): go through JSON
	b, err := json.Marshal(v)
	if err != nil {
		return Shape{Type: []string{TObject}}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var generic any
	if dec.Decode(&generic) != nil {
		return Shape{Type: []string{TObject}}
	}
	return infer(generic, depth+1)
}

// Merge combines two observations into one shape that describes both: types are unioned, and a
// field missing from either observation of an object becomes optional.
func Merge(a, b Shape) Shape {
	if len(a.Type) == 0 {
		return b
	}
	if len(b.Type) == 0 {
		return a
	}
	out := Shape{Type: unionTypes(a.Type, b.Type)}
	aObj, bObj := has(a.Type, TObject), has(b.Type, TObject)
	if aObj || bObj {
		out.Fields = map[string]Field{}
		for k, fa := range a.Fields {
			if fb, ok := b.Fields[k]; ok {
				out.Fields[k] = Field{Shape: Merge(fa.Shape, fb.Shape), Optional: fa.Optional || fb.Optional}
			} else {
				// missing from the other side: optional only if the other side is an object at all
				fa.Optional = fa.Optional || bObj
				out.Fields[k] = fa
			}
		}
		for k, fb := range b.Fields {
			if _, ok := a.Fields[k]; !ok {
				fb.Optional = fb.Optional || aObj
				out.Fields[k] = fb
			}
		}
	}
	switch {
	case a.Items != nil && b.Items != nil:
		m := Merge(*a.Items, *b.Items)
		out.Items = &m
	case a.Items != nil:
		out.Items = a.Items
	case b.Items != nil:
		out.Items = b.Items
	}
	return out
}

func has(types []string, t string) bool {
	for _, x := range types {
		if x == t {
			return true
		}
	}
	return false
}

func unionTypes(a, b []string) []string {
	set := map[string]bool{}
	for _, t := range a {
		set[t] = true
	}
	for _, t := range b {
		set[t] = true
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
