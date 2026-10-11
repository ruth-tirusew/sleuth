// Package flow defines the flow spec and turns it into an ordered DAG.
package flow

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"text/template"
	"text/template/parse"
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
	// DeferResponse is set by the runner, never by a flow: response violations are recorded
	// but left for the caller to judge (a poll judges only the attempt that satisfied it).
	// Request-side violations still stop the request before it is sent.
	DeferResponse bool `yaml:"-" json:"-"`
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

var checkKeys = map[string]bool{"path": true, "equals": true, "exists": true, "contains": true, "matches": true, "gt": true, "lt": true, "message": true}

// UnmarshalYAML reads a check, keeping the exact text of any number in its expected values
// (`equals`, `contains`): YAML would otherwise parse 12345678901234567890.123456789 as a
// float64 and round it before it is ever compared. Unknown fields stay an error. YAML anchors
// and merge keys (`<<: *base`) work as they do everywhere else.
func (c *Check) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: a check must be a mapping", n.Line)
	}
	entries := mappingEntries(n, 0)
	for _, e := range entries {
		if !checkKeys[e.key.Value] {
			return fmt.Errorf("line %d: field %s not found in a check", e.key.Line, e.key.Value)
		}
	}
	type plain Check
	if err := n.Decode((*plain)(c)); err != nil { // the YAML decoder resolves merges itself
		return err
	}
	for _, e := range entries {
		switch e.key.Value {
		case "equals":
			c.Equals = exactValue(e.val)
		case "contains":
			c.Contains = exactValue(e.val)
		}
	}
	return nil
}

type yamlEntry struct{ key, val *yaml.Node }

// mappingEntries lists a mapping's key/value pairs with merge keys (`<<`) expanded and
// aliases followed. An explicit key beats a merged one, and an earlier merge beats a later one.
func mappingEntries(n *yaml.Node, depth int) []yamlEntry {
	if depth > maxYAMLDepth { // a self-referential anchor must be an error, not a stack overflow
		return nil
	}
	if n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	var out []yamlEntry
	seen := map[string]bool{}
	var merges []*yaml.Node
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Tag == "!!merge" { // only the real merge key; a quoted "<<" is ordinary data
			merges = append(merges, v)
			continue
		}
		out = append(out, yamlEntry{k, v})
		seen[k.Value] = true
	}
	for _, m := range merges {
		if m.Kind == yaml.AliasNode && m.Alias != nil {
			m = m.Alias
		}
		var sources []*yaml.Node
		if m.Kind == yaml.SequenceNode {
			sources = m.Content
		} else {
			sources = []*yaml.Node{m}
		}
		for _, src := range sources {
			for _, e := range mappingEntries(src, depth+1) {
				if !seen[e.key.Value] {
					out = append(out, e)
					seen[e.key.Value] = true
				}
			}
		}
	}
	return out
}

var numberLiteral = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// IsNumberLiteral reports whether s is a valid JSON number ("NaN" and "Infinity" are not).
// It is the one definition used when reading flows and when reading results.
func IsNumberLiteral(s string) bool { return numberLiteral.MatchString(s) }

// exactValue decodes a YAML node like any, except decimal number scalars become json.Number
// (their exact text). Other number forms (hex, octal, 1_000) keep YAML's normal decoding.
func exactValue(n *yaml.Node) any { return exactValueAt(n, 0) }

// maxYAMLDepth bounds how far aliases and merges are followed.
const maxYAMLDepth = 64

func exactValueAt(n *yaml.Node, depth int) any {
	if depth > maxYAMLDepth {
		return nil
	}
	if n.Kind == yaml.AliasNode && n.Alias != nil {
		return exactValueAt(n.Alias, depth+1)
	}
	switch n.Kind {
	case yaml.ScalarNode:
		if (n.Tag == "!!int" || n.Tag == "!!float") && IsNumberLiteral(n.Value) {
			return json.Number(n.Value)
		}
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		for _, e := range mappingEntries(n, depth) {
			m[e.key.Value] = exactValueAt(e.val, depth+1)
		}
		return m
	case yaml.SequenceNode:
		out := make([]any, len(n.Content))
		for i, e := range n.Content {
			out[i] = exactValueAt(e, depth+1)
		}
		return out
	}
	var v any
	_ = n.Decode(&v)
	return v
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
	// Backoff optionally grows the delay between attempts. Without it the interval is fixed.
	Backoff *Backoff `yaml:"backoff,omitempty" json:"backoff,omitempty"`
}

// Backoff grows a poll's delay: attempt k waits interval * factor^(k-1), capped at Max,
// then spread by +/- Jitter (a fraction, 0.2 = 20%) so parallel pollers don't synchronise.
type Backoff struct {
	Factor float64 `yaml:"factor,omitempty" json:"factor,omitempty"` // >= 1; default 2
	Max    string  `yaml:"max,omitempty" json:"max,omitempty"`       // delay cap; default none
	Jitter float64 `yaml:"jitter,omitempty" json:"jitter,omitempty"` // 0..1; default 0
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

// A node reference is only real in two places: inside a {{ ... }} template expression
// (".nodes.<id>"), or as the `path:` of a check ("nodes.<id>.response.status"). Anywhere
// else the text is just text: a URL host such as nodes.cluster.local, or a message
// mentioning "nodes.count", must not become a dependency.
var (
	tplRe     = regexp.MustCompile(`(?s)\{\{(.*?)\}\}`)
	tplRefRe  = regexp.MustCompile(`\.nodes\.([A-Za-z_][A-Za-z0-9_]*)`)
	pathRefRe = regexp.MustCompile(`^\.?nodes\.([A-Za-z_][A-Za-z0-9_]*)`)
)

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
		if _, untraceable := n.refs(); untraceable && len(n.Needs) == 0 {
			return fmt.Errorf("node %s: a template reads .nodes in a way that cannot be traced to specific nodes (for example through a variable); list the nodes it uses under needs:", n.ID)
		}
		if c := n.Contract; c != nil {
			if n.Kind != KindRequest && n.Kind != KindPoll {
				return fmt.Errorf("node %s: contract is only valid on request and poll nodes", n.ID)
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
			if b := n.Poll.Backoff; b != nil {
				if b.Factor != 0 && b.Factor < 1 {
					return fmt.Errorf("node %s: poll.backoff.factor must be >= 1", n.ID)
				}
				if b.Jitter < 0 || b.Jitter > 1 {
					return fmt.Errorf("node %s: poll.backoff.jitter must be between 0 and 1", n.ID)
				}
				if b.Max != "" {
					if d, err := time.ParseDuration(b.Max); err != nil || d <= 0 {
						return fmt.Errorf("node %s: bad poll.backoff.max %q", n.ID, b.Max)
					}
				}
			}
			if n.Contract != nil && n.Poll.Request == nil {
				return fmt.Errorf("node %s: contract on a poll node needs poll.request (db polls have no contract)", n.ID)
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

// Deps returns explicit `needs` plus dependencies inferred from `nodes.<id>` references
// in template expressions and in the paths of checks (see refs).
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
	ids, _ := n.refs()
	for _, id := range ids {
		add(id)
	}
	return out
}

// refs returns the node ids this node reads, and whether it also reads .nodes in a way that
// cannot be traced to specific nodes (for example `{{ with $n := .nodes }}`).
func (n Node) refs() (ids []string, untraceable bool) {
	add := func(id string) { ids = append(ids, id) }
	// A check's path reads the run context directly ("nodes.<id>.response.status"). These are
	// taken from the typed checks, not guessed from key names, so a request body field that
	// happens to be called "path" is data and never a reference.
	checks := n.Assert
	if n.Poll != nil {
		checks = append(append([]Check(nil), checks...), n.Poll.Until...)
	}
	for _, c := range checks {
		if m := pathRefRe.FindStringSubmatch(c.Path); m != nil {
			add(m[1])
		}
	}
	// Everything else is searched for templates.
	var generic any
	b, err := json.Marshal(n)
	if err != nil || json.Unmarshal(b, &generic) != nil {
		return ids, untraceable
	}
	scanTemplates(generic, add, &untraceable)
	return ids, untraceable
}

// scanTemplates walks a node (as generic JSON) and reports the node references inside every
// template string. Map keys are not scanned: keys are never rendered, so a template in a
// key is just text.
func scanTemplates(v any, add func(string), untraceable *bool) {
	switch x := v.(type) {
	case map[string]any:
		for _, e := range x {
			scanTemplates(e, add, untraceable)
		}
	case []any:
		for _, e := range x {
			scanTemplates(e, add, untraceable)
		}
	case string:
		if strings.Contains(x, "{{") {
			templateRefs(x, add, untraceable)
		}
	}
}

// templateRefs finds the nodes a template reads by parsing it, so `.nodes.a.out.x` and
// `(index .nodes "a").out.x` both count and text inside string literals does not.
func templateRefs(text string, add func(string), untraceable *bool) {
	t, err := template.New("").Parse(text)
	if err != nil || t.Tree == nil {
		// not a template Go can parse: fall back to a plain scan of the {{ }} bodies
		for _, body := range tplRe.FindAllStringSubmatch(text, -1) {
			for _, m := range tplRefRe.FindAllStringSubmatch(body[1], -1) {
				add(m[1])
			}
		}
		return
	}
	walkTemplate(t.Tree.Root, false, add, untraceable)
}

// scoped is true inside range / with, where `.` is no longer the root data.
func walkTemplate(n parse.Node, scoped bool, add func(string), untraceable *bool) {
	switch x := n.(type) {
	case nil:
	case *parse.ListNode:
		if x == nil {
			return
		}
		for _, c := range x.Nodes {
			walkTemplate(c, scoped, add, untraceable)
		}
	case *parse.ActionNode:
		walkTemplate(x.Pipe, scoped, add, untraceable)
	case *parse.PipeNode:
		if x == nil {
			return
		}
		for _, d := range x.Decl {
			walkTemplate(d, scoped, add, untraceable)
		}
		for _, c := range x.Cmds {
			walkTemplate(c, scoped, add, untraceable)
		}
	case *parse.CommandNode:
		// (index .nodes "id"): a lookup by literal id is traceable
		if len(x.Args) >= 3 {
			if id, ok := x.Args[0].(*parse.IdentifierNode); ok && id.Ident == "index" {
				if f, ok := x.Args[1].(*parse.FieldNode); ok && len(f.Ident) == 1 && f.Ident[0] == "nodes" {
					if lit, ok := x.Args[2].(*parse.StringNode); ok {
						add(lit.Text)
						for _, a := range x.Args[3:] {
							walkTemplate(a, scoped, add, untraceable)
						}
						return
					}
				}
			}
		}
		for _, a := range x.Args {
			walkTemplate(a, scoped, add, untraceable)
		}
	case *parse.FieldNode:
		if len(x.Ident) > 0 && x.Ident[0] == "nodes" {
			if len(x.Ident) >= 2 {
				add(x.Ident[1])
			} else {
				*untraceable = true
			}
		}
	case *parse.VariableNode:
		// $ is the root data, so $.nodes.a.out.x reads node a just like .nodes.a.out.x. Using
		// $ itself (or $.nodes as a whole) reads every node, which cannot be ordered.
		if len(x.Ident) >= 1 && x.Ident[0] == "$" {
			switch {
			case len(x.Ident) == 1: // bare $
				*untraceable = true
			case x.Ident[1] == "nodes" && len(x.Ident) >= 3:
				add(x.Ident[2])
			case x.Ident[1] == "nodes":
				*untraceable = true
			}
		}
	case *parse.DotNode:
		if !scoped { // . at the top level is the whole root data
			*untraceable = true
		}
	case *parse.ChainNode:
		walkTemplate(x.Node, scoped, add, untraceable)
	case *parse.IfNode:
		walkTemplate(x.Pipe, scoped, add, untraceable)
		walkTemplate(x.List, scoped, add, untraceable)
		walkTemplate(x.ElseList, scoped, add, untraceable)
	case *parse.RangeNode:
		walkTemplate(x.Pipe, scoped, add, untraceable)
		walkTemplate(x.List, true, add, untraceable)
		walkTemplate(x.ElseList, scoped, add, untraceable)
	case *parse.WithNode:
		walkTemplate(x.Pipe, scoped, add, untraceable)
		walkTemplate(x.List, true, add, untraceable)
		walkTemplate(x.ElseList, scoped, add, untraceable)
	case *parse.TemplateNode:
		walkTemplate(x.Pipe, scoped, add, untraceable)
	}
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

// Waves groups nodes by dependency depth: every node in a wave depends only on earlier
// waves, so the nodes of one wave are independent of each other and may run together.
func (f *Flow) Waves() ([][]string, error) {
	order, err := f.Order()
	if err != nil {
		return nil, err
	}
	level := map[string]int{}
	var waves [][]string
	for _, n := range order {
		l := 0
		for _, d := range n.Deps() {
			if level[d]+1 > l {
				l = level[d] + 1
			}
		}
		level[n.ID] = l
		for len(waves) <= l {
			waves = append(waves, nil)
		}
		waves[l] = append(waves[l], n.ID)
	}
	return waves, nil
}
