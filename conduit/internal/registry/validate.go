package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

const specURL = "https://conduit.invalid/openapi.json"

var printer = message.NewPrinter(language.English)

// Issue is one place where a value disagrees with a schema.
type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// Validate checks value against the schema stored at ptr in the OpenAPI document
// (e.g. Operation.RequestPtr), using a full JSON Schema 2020-12 implementation.
func (s *Spec) Validate(ptr string, value any) ([]Issue, error) {
	sch, ok := s.compiled[ptr]
	if !ok {
		var err error
		if sch, err = s.comp.Compile(specURL + ptr); err != nil {
			return nil, fmt.Errorf("compile schema %s: %w", ptr, err)
		}
		s.compiled[ptr] = sch
	}
	b, err := json.Marshal(value) // normalise Go/YAML types to JSON types
	if err != nil {
		return nil, err
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	verr := sch.Validate(v)
	if verr == nil {
		return nil, nil
	}
	ve, ok := verr.(*jsonschema.ValidationError)
	if !ok {
		return nil, verr
	}
	var out []Issue
	collect(ve, &out)
	return out, nil
}

// collect flattens the error tree to its leaves. anyOf/oneOf failures are reported
// once (with the first branch's reason) instead of once per branch.
func collect(e *jsonschema.ValidationError, out *[]Issue) {
	kw := e.ErrorKind.KeywordPath()
	last := ""
	if len(kw) > 0 {
		last = kw[len(kw)-1]
	}
	if len(e.Causes) == 0 || last == "anyOf" || last == "oneOf" {
		msg := e.ErrorKind.LocalizedString(printer)
		if len(e.Causes) > 0 {
			msg += " (e.g. " + leafMessage(e.Causes[0]) + ")"
		}
		*out = append(*out, Issue{Path: instancePath(e.InstanceLocation), Message: msg})
		return
	}
	for _, c := range e.Causes {
		collect(c, out)
	}
}

func leafMessage(e *jsonschema.ValidationError) string {
	for len(e.Causes) > 0 {
		e = e.Causes[0]
	}
	return e.ErrorKind.LocalizedString(printer)
}

// instancePath renders ["tags","1"] as $.tags[1].
func instancePath(loc []string) string {
	var sb strings.Builder
	sb.WriteString("$")
	for _, seg := range loc {
		if _, err := strconv.Atoi(seg); err == nil {
			sb.WriteString("[" + seg + "]")
		} else {
			sb.WriteString("." + seg)
		}
	}
	return sb.String()
}
