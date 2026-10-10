// Package flow defines the flow spec and turns it into an ordered DAG.
package flow

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	KindRequest   = "request"
	KindAssertion = "assertion"
	KindTransform = "transform"
	KindPoll      = "poll"
	KindDB        = "db"
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
	Poll      *Poll      `yaml:"poll,omitempty" json:"poll,omitempty"`
	DB        *DB        `yaml:"db,omitempty" json:"db,omitempty"`
	// Contract (request nodes only) validates the call against a published OpenAPI document.
	Contract *Contract `yaml:"contract,omitempty" json:"contract,omitempty"`
}

// Contract binds a request node to a named source from the env file's `contracts:` map.
// Shorthand: `contract: api`.
type Contract struct {
	Source string `yaml:"source" json:"source"`
	// OnViolation is "fail" (default) or "warn" (record evidence, don't fail the step).
	OnViolation string `yaml:"on_violation,omitempty" json:"on_violation,omitempty"`
}

func (c *Contract) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		c.Source = n.Value
		return nil
	}
	type plain Contract
	return n.Decode((*plain)(c))
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

// Poll repeats a request until every `until` check passes or the timeout elapses.
// `until` paths read the latest response, e.g. nodes.<id>.response.body.status.
type Poll struct {
	// Exactly one of Request or DB is polled.
	Request *Request `yaml:"request,omitempty" json:"request,omitempty"`
	DB      *DB      `yaml:"db,omitempty" json:"db,omitempty"`
	Until   []Check  `yaml:"until" json:"until"`
	// Interval between attempts (default 1s) and Timeout for the whole poll (default 30s).
	Interval string `yaml:"interval,omitempty" json:"interval,omitempty"`
	Timeout  string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// DB runs a read-only query against a database named under `databases:` in the env file.
// Results are available as nodes.<id>.rows (list of objects), nodes.<id>.row_count and
// nodes.<id>.out.<name>; `assert` checks run against them.
//
// Type "postgres" (default) uses Query/Args. Type "mongo" uses Database, Collection and
// either Filter (+Sort/Projection) for a find, or Pipeline for an aggregation.
type DB struct {
	Type       string `yaml:"type,omitempty" json:"type,omitempty"` // postgres (default) | mongo
	Connection string `yaml:"connection" json:"connection"`

	Query string `yaml:"query,omitempty" json:"query,omitempty"`
	Args  []any  `yaml:"args,omitempty" json:"args,omitempty"`

	Database   string           `yaml:"database,omitempty" json:"database,omitempty"`
	Collection string           `yaml:"collection,omitempty" json:"collection,omitempty"`
	Filter     map[string]any   `yaml:"filter,omitempty" json:"filter,omitempty"`
	Sort       []map[string]int `yaml:"sort,omitempty" json:"sort,omitempty"` // ordered: [{created_at: -1}, {_id: 1}]
	Projection map[string]any   `yaml:"projection,omitempty" json:"projection,omitempty"`
	Pipeline   []any            `yaml:"pipeline,omitempty" json:"pipeline,omitempty"`

	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`   // default 10s
	MaxRows int    `yaml:"max_rows,omitempty" json:"max_rows,omitempty"` // default 1000
	// Extract maps output names to paths in {rows, row_count}, e.g. id: rows.0.id.
	Extract map[string]string `yaml:"extract,omitempty" json:"extract,omitempty"`
}

const (
	DBPostgres = "postgres"
	DBMongo    = "mongo"
)

// Kind returns the database type with the default applied.
func (d *DB) Kind() string {
	if d.Type == "" {
		return DBPostgres
	}
	return d.Type
}

func (d *DB) validate(id string) error {
	if d == nil || d.Connection == "" {
		return fmt.Errorf("node %s: db.connection is required", id)
	}
	switch d.Kind() {
	case DBPostgres:
		if strings.TrimSpace(d.Query) == "" {
			return fmt.Errorf("node %s: db.query is required for postgres", id)
		}
		if d.Database != "" || d.Collection != "" || d.Filter != nil || d.Pipeline != nil || d.Sort != nil || d.Projection != nil {
			return fmt.Errorf("node %s: database/collection/filter/pipeline/sort/projection are mongo-only fields", id)
		}
	case DBMongo:
		if d.Database == "" || d.Collection == "" {
			return fmt.Errorf("node %s: db.database and db.collection are required for mongo", id)
		}
		if d.Query != "" || d.Args != nil {
			return fmt.Errorf("node %s: query/args are postgres-only; mongo uses filter or pipeline", id)
		}
		if d.Pipeline != nil && (d.Filter != nil || d.Sort != nil || d.Projection != nil) {
			return fmt.Errorf("node %s: use either pipeline or filter/sort/projection, not both", id)
		}
		for _, s := range d.Sort {
			if len(s) != 1 {
				return fmt.Errorf("node %s: each sort entry must have exactly one field", id)
			}
		}
	default:
		return fmt.Errorf("node %s: db.type must be postgres or mongo", id)
	}
	if d.Timeout != "" {
		if _, err := time.ParseDuration(d.Timeout); err != nil {
			return fmt.Errorf("node %s: bad db.timeout %q", id, d.Timeout)
		}
	}
	if d.MaxRows < 0 {
		return fmt.Errorf("node %s: db.max_rows must be positive", id)
	}
	return nil
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
		if c := n.Contract; c != nil {
			if n.Kind != KindRequest && n.Kind != KindPoll {
				return fmt.Errorf("node %s: contract is only valid on request nodes", n.ID)
			}
			if c.Source == "" {
				return fmt.Errorf("node %s: contract.source is required", n.ID)
			}
			if c.OnViolation != "" && c.OnViolation != "fail" && c.OnViolation != "warn" {
				return fmt.Errorf("node %s: contract.on_violation must be fail or warn", n.ID)
			}
		}
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
		case KindPoll:
			if n.Poll == nil || (n.Poll.Request == nil) == (n.Poll.DB == nil) {
				return fmt.Errorf("node %s: poll needs exactly one of poll.request or poll.db", n.ID)
			}
			if n.Poll.Request != nil && n.Poll.Request.URL == "" {
				return fmt.Errorf("node %s: poll.request.url is required", n.ID)
			}
			if n.Poll.DB != nil {
				if err := n.Poll.DB.validate(n.ID); err != nil {
					return err
				}
			}
			if len(n.Poll.Until) == 0 {
				return fmt.Errorf("node %s: poll.until must have at least one check", n.ID)
			}
			for _, d := range []string{n.Poll.Interval, n.Poll.Timeout} {
				if d == "" {
					continue
				}
				if _, err := time.ParseDuration(d); err != nil {
					return fmt.Errorf("node %s: bad poll duration %q", n.ID, d)
				}
			}
			if n.Contract != nil {
				return fmt.Errorf("node %s: contract is not supported on poll nodes", n.ID)
			}
		case KindDB:
			if err := n.DB.validate(n.ID); err != nil {
				return err
			}
			if len(n.Assert) == 0 && len(n.DB.Extract) == 0 {
				return fmt.Errorf("node %s: a db node needs assert or db.extract", n.ID)
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
