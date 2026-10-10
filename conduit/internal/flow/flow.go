// Package flow defines the flow spec and turns it into an ordered DAG.
package flow

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

const (
	KindRequest   = "request"
	KindAssertion = "assertion"
	KindTransform = "transform"
)

type Flow struct {
	Version     int    `yaml:"version" json:"version"`
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Nodes       []Node `yaml:"nodes" json:"nodes"`
}

type Node struct {
	ID        string     `yaml:"id" json:"id"`
	Kind      string     `yaml:"kind" json:"kind"`
	Needs     []string   `yaml:"needs,omitempty" json:"needs,omitempty"`
	Request   *Request   `yaml:"request,omitempty" json:"request,omitempty"`
	Assert    []Check    `yaml:"assert,omitempty" json:"assert,omitempty"`
	Transform *Transform `yaml:"transform,omitempty" json:"transform,omitempty"`
}

type Request struct {
	Method  string            `yaml:"method" json:"method"`
	URL     string            `yaml:"url" json:"url"`
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	Query   map[string]string `yaml:"query,omitempty" json:"query,omitempty"`
	Body    any               `yaml:"body,omitempty" json:"body,omitempty"`
	Timeout string            `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	// Extract maps output names to paths inside the response
	// (e.g. token: body.token). Available later as nodes.<id>.out.<name>.
	Extract map[string]string `yaml:"extract,omitempty" json:"extract,omitempty"`
}

// Check is one assertion: a path into the run context plus one or more operators.
type Check struct {
	Path     string   `yaml:"path" json:"path"`
	Equals   any      `yaml:"equals,omitempty" json:"equals,omitempty"`
	Exists   *bool    `yaml:"exists,omitempty" json:"exists,omitempty"`
	Contains any      `yaml:"contains,omitempty" json:"contains,omitempty"`
	Matches  string   `yaml:"matches,omitempty" json:"matches,omitempty"`
	Gt       *float64 `yaml:"gt,omitempty" json:"gt,omitempty"`
	Lt       *float64 `yaml:"lt,omitempty" json:"lt,omitempty"`
	Message  string   `yaml:"message,omitempty" json:"message,omitempty"`
}

type Transform struct {
	// Set maps output names to values; strings may contain templates.
	// Outputs are available as nodes.<id>.out.<name>.
	Set map[string]any `yaml:"set" json:"set"`
}

var idRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var refRe = regexp.MustCompile(`\bnodes\.([A-Za-z_][A-Za-z0-9_]*)`)

func Load(path string) (*Flow, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f Flow
	dec := yaml.NewDecoder(bytesReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("invalid flow %s: %w", path, err)
	}
	return &f, nil
}

func (f *Flow) Validate() error {
	if f.Version != 1 {
		return fmt.Errorf("version must be 1")
	}
	if f.Name == "" {
		return fmt.Errorf("name is required")
	}
	if len(f.Nodes) == 0 {
		return fmt.Errorf("flow has no nodes")
	}
	seen := map[string]bool{}
	for _, n := range f.Nodes {
		if !idRe.MatchString(n.ID) {
			return fmt.Errorf("node id %q must match %s", n.ID, idRe)
		}
		if seen[n.ID] {
			return fmt.Errorf("duplicate node id %q", n.ID)
		}
		seen[n.ID] = true
		switch n.Kind {
		case KindRequest:
			if n.Request == nil || n.Request.URL == "" {
				return fmt.Errorf("node %s: request.url is required", n.ID)
			}
		case KindAssertion:
			if len(n.Assert) == 0 {
				return fmt.Errorf("node %s: assert must have at least one check", n.ID)
			}
		case KindTransform:
			if n.Transform == nil || len(n.Transform.Set) == 0 {
				return fmt.Errorf("node %s: transform.set is required", n.ID)
			}
		default:
			return fmt.Errorf("node %s: unknown kind %q", n.ID, n.Kind)
		}
	}
	_, err := f.Order()
	return err
}

// Deps returns explicit `needs` plus dependencies inferred from `nodes.<id>`
// references anywhere in the node.
func (n Node) Deps() []string {
	set := map[string]bool{}
	var out []string
	add := func(id string) {
		if id != n.ID && !set[id] {
			set[id] = true
			out = append(out, id)
		}
	}
	for _, d := range n.Needs {
		add(d)
	}
	b, _ := json.Marshal(n)
	for _, m := range refRe.FindAllSubmatch(b, -1) {
		add(string(m[1]))
	}
	return out
}

// Order returns nodes in topological order (Kahn's algorithm, stable by declaration order).
func (f *Flow) Order() ([]Node, error) {
	byID := map[string]Node{}
	indeg := map[string]int{}
	rev := map[string][]string{}
	for _, n := range f.Nodes {
		byID[n.ID] = n
	}
	for _, n := range f.Nodes {
		for _, d := range n.Deps() {
			if _, ok := byID[d]; !ok {
				return nil, fmt.Errorf("node %s depends on unknown node %q", n.ID, d)
			}
			indeg[n.ID]++
			rev[d] = append(rev[d], n.ID)
		}
	}
	var out []Node
	done := map[string]bool{}
	for len(out) < len(f.Nodes) {
		progressed := false
		for _, n := range f.Nodes {
			if !done[n.ID] && indeg[n.ID] == 0 {
				done[n.ID] = true
				out = append(out, n)
				for _, r := range rev[n.ID] {
					indeg[r]--
				}
				progressed = true
				break
			}
		}
		if !progressed {
			return nil, fmt.Errorf("cycle detected among nodes")
		}
	}
	return out, nil
}
