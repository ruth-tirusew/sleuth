package flow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func load(t *testing.T, body string) *Flow {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.yaml")
	if err := os.WriteFile(p, []byte("version: 1\nname: t\nnodes:\n"+body), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return f
}

func deps(f *Flow, id string) []string {
	for _, n := range f.Nodes {
		if n.ID == id {
			d := n.Deps()
			sort.Strings(d)
			return d
		}
	}
	return nil
}

// Text that merely contains "nodes.<word>" is not a reference.
func TestDepsIgnoreLiteralText(t *testing.T) {
	f := load(t, `
  - id: a
    kind: request
    request: {url: "https://nodes.cluster.local/api", headers: {X-Note: "see nodes.count"}}
  - id: chk
    kind: assertion
    needs: [a]
    assert:
      - {path: nodes.a.response.status, equals: 200, message: "nodes.count mismatch"}
      - {path: nodes.a.response.body.label, equals: "nodes.cluster"}
`)
	if d := deps(f, "a"); len(d) != 0 {
		t.Errorf("literal text created dependencies: %v", d)
	}
	if d := deps(f, "chk"); !reflect.DeepEqual(d, []string{"a"}) {
		t.Errorf("chk deps %v", d)
	}
}

// Real references, wherever they legitimately occur, still create edges.
func TestDepsFromTemplatesAndPaths(t *testing.T) {
	f := load(t, `
  - {id: a, kind: transform, transform: {set: {x: 1}}}
  - {id: b, kind: transform, transform: {set: {y: 2}}}
  - {id: c, kind: transform, transform: {set: {z: 3}}}
  - {id: d, kind: transform, transform: {set: {z: 3}}}
  - {id: e, kind: transform, transform: {set: {z: 3}}}
  - id: req
    kind: request
    request:
      url: "{{ .env.base }}/x/{{ .nodes.a.out.x }}"
      headers: {Authorization: "Bearer {{ .nodes.b.out.y }}"}
      body: {n: "{{ .nodes.c.out.z }}", list: ["{{ .nodes.d.out.z }}"]}
  - id: wait
    kind: poll
    poll:
      request: {url: "{{ .env.base }}/j"}
      until: [{path: nodes.wait.response.body.status, equals: "{{ .nodes.e.out.z }}"}]
  - id: upstream
    kind: transform
    transform: {set: {k: 1}}
  - id: chk
    kind: assertion
    assert: [{path: nodes.upstream.out.k, equals: 1}]
`)
	if d := deps(f, "req"); !reflect.DeepEqual(d, []string{"a", "b", "c", "d"}) {
		t.Errorf("req deps %v", d)
	}
	if d := deps(f, "wait"); !reflect.DeepEqual(d, []string{"e"}) { // its own id is not a dependency
		t.Errorf("wait deps %v", d)
	}
	if d := deps(f, "chk"); !reflect.DeepEqual(d, []string{"upstream"}) {
		t.Errorf("chk deps %v", d)
	}
}

func TestDepsOnUnknownNodeStillRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.yaml")
	os.WriteFile(p, []byte("version: 1\nname: t\nnodes:\n  - {id: a, kind: transform, transform: {set: {x: \"{{ .nodes.ghost.out.y }}\"}}}\n"), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("a template reference to an undefined node must still fail validation")
	}
}

// Other ways of reading a node are recognised, and a template that reads nodes in a way
// that cannot be traced is refused with a clear message instead of being mis-ordered.
func TestDepsFromIndexFormAndUntraceableTemplates(t *testing.T) {
	f := load(t, `
  - {id: login, kind: transform, transform: {set: {token: t}}}
  - id: viaindex
    kind: request
    request: {url: 'http://x/{{ (index .nodes "login").out.token }}'}
`)
	if d := deps(f, "viaindex"); !reflect.DeepEqual(d, []string{"login"}) {
		t.Errorf("index form: %v", d)
	}

	p := filepath.Join(t.TempDir(), "f.yaml")
	os.WriteFile(p, []byte(`version: 1
name: t
nodes:
  - {id: login, kind: transform, transform: {set: {token: t}}}
  - id: dynamic
    kind: request
    request: {url: 'http://x/{{ with $n := .nodes }}{{ $n.login.out.token }}{{ end }}'}
`), 0o644)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "needs") {
		t.Fatalf("a template that reads .nodes dynamically must be refused with advice to use needs:, got %v", err)
	}
}

// Only the path of a check is a reference, not any map key that happens to be called path.
func TestDepsIgnoreAPathKeyOutsideChecks(t *testing.T) {
	f := load(t, `
  - id: a
    kind: request
    request: {method: POST, url: "http://x", body: {path: "nodes.b.out.x", items: [{path: "nodes.c.y"}]}}
  - {id: b, kind: transform, transform: {set: {x: 1}}}
  - {id: c, kind: transform, transform: {set: {y: 1}}}
`)
	if d := deps(f, "a"); len(d) != 0 {
		t.Fatalf("body fields named path are data: %v", d)
	}
}

// Expected numbers in a check keep their exact text: YAML would otherwise round them
// through a float64 before they are ever compared.
func TestCheckExpectedNumbersKeepExactText(t *testing.T) {
	f := load(t, `
  - id: chk
    kind: assertion
    assert:
      - {path: a, equals: 12345678901234567890.123456789}
      - {path: b, equals: 10.50}
      - {path: c, equals: 9007199254740993}
      - {path: d, contains: [1.0, 2.5, {n: 0.30}]}
      - {path: e, equals: text}
      - {path: f, equals: 0x1F}
`)
	a := f.Nodes[0].Assert
	if a[0].Equals != json.Number("12345678901234567890.123456789") {
		t.Errorf("long decimal: %#v", a[0].Equals)
	}
	if a[1].Equals != json.Number("10.50") || a[2].Equals != json.Number("9007199254740993") {
		t.Errorf("%#v %#v", a[1].Equals, a[2].Equals)
	}
	want := []any{json.Number("1.0"), json.Number("2.5"), map[string]any{"n": json.Number("0.30")}}
	if !reflect.DeepEqual(a[3].Contains, want) {
		t.Errorf("nested: %#v", a[3].Contains)
	}
	if a[4].Equals != "text" {
		t.Errorf("strings are untouched: %#v", a[4].Equals)
	}
	if v, ok := a[5].Equals.(int); !ok || v != 31 {
		t.Errorf("a YAML hex int is not a JSON number literal and keeps the normal decoding: %#v", a[5].Equals)
	}
}

func TestCheckStillRejectsUnknownFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.yaml")
	os.WriteFile(p, []byte("version: 1\nname: t\nnodes:\n  - id: c\n    kind: assertion\n    assert: [{path: a, equal: 1}]\n"), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("a typo such as `equal:` must still be a validation error")
	}
}

// $ is the root of the template data, so $.nodes.a reads node a from inside range / with.
func TestDepsFromRootVariable(t *testing.T) {
	f := load(t, `
  - {id: login, kind: transform, transform: {set: {token: t}}}
  - id: inrange
    kind: request
    request:
      url: 'http://x/{{ range .items }}{{ $.nodes.login.out.token }}{{ end }}'
`)
	if d := deps(f, "inrange"); !reflect.DeepEqual(d, []string{"login"}) {
		t.Errorf("$.nodes form: %v", d)
	}
	p := filepath.Join(t.TempDir(), "f.yaml")
	os.WriteFile(p, []byte("version: 1\nname: t\nnodes:\n  - {id: login, kind: transform, transform: {set: {t: 1}}}\n  - id: bad\n    kind: request\n    request: {url: 'http://x/{{ with $n := $.nodes }}{{ $n.login.out.t }}{{ end }}'}\n"), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("`$n := $.nodes` cannot be traced and must be refused")
	}
}

// YAML anchors, aliases and merge keys keep working inside checks.
func TestChecksSupportYAMLAnchorsAliasesAndMergeKeys(t *testing.T) {
	f := load(t, `
  - {id: x, kind: transform, transform: {set: {s: 1}}}
  - {id: y, kind: transform, transform: {set: {s: 1}}}
  - id: a
    kind: assertion
    assert:
      - &ok {path: nodes.x.out.s, equals: 200, message: shared}
      - *ok
      - <<: *ok
        path: nodes.y.out.s
        equals: 404.50
  - id: b
    kind: assertion
    assert:
      - <<: [*ok, {contains: [1.0]}]
`)
	a := f.Nodes[2].Assert
	if len(a) != 3 || a[1].Path != "nodes.x.out.s" || a[1].Equals != json.Number("200") {
		t.Fatalf("alias: %+v", a)
	}
	if a[2].Path != "nodes.y.out.s" || a[2].Equals != json.Number("404.50") || a[2].Message != "shared" {
		t.Fatalf("merge key (explicit keys win, the rest is merged, numbers stay exact): %+v", a[2])
	}
	b := f.Nodes[3].Assert[0]
	if b.Path != "nodes.x.out.s" || b.Equals != json.Number("200") || !reflect.DeepEqual(b.Contains, []any{json.Number("1.0")}) {
		t.Fatalf("merge of a sequence: %+v", b)
	}
	if d := deps(f, "a"); !reflect.DeepEqual(d, []string{"x", "y"}) {
		t.Errorf("deps through anchors: %v", d)
	}
}

// A merged-in unknown field is still rejected.
func TestChecksRejectUnknownFieldsEvenWhenMerged(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.yaml")
	os.WriteFile(p, []byte("version: 1\nname: t\nnodes:\n  - id: c\n    kind: assertion\n    assert:\n      - <<: {path: a, equal: 1}\n"), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("an unknown field must be an error even when it arrives through a merge")
	}
}

// Reading all nodes at once (the whole root) cannot be ordered, so it needs explicit needs:
// but `.` inside range / with is just the loop item.
func TestRootReadsAreUntraceableButScopedDotIsNot(t *testing.T) {
	for name, tpl := range map[string]string{
		"printf of dot":    `{{ printf "%v" . }}`,
		"bare $":           `{{ $ }}`,
		"range over $":     `{{ range $k, $v := $ }}{{ $k }}{{ end }}`,
		"variable of $":    `{{ $r := $ }}{{ $r.x }}`,
		"$.nodes as whole": `{{ $n := $.nodes }}{{ $n.a }}`,
	} {
		p := filepath.Join(t.TempDir(), "f.yaml")
		os.WriteFile(p, []byte("version: 1\nname: t\nnodes:\n  - {id: a, kind: transform, transform: {set: {x: 1}}}\n  - id: b\n    kind: request\n    request: {url: 'http://x/"+strings.ReplaceAll(tpl, "'", "''")+"'}\n"), 0o644)
		if _, err := Load(p); err == nil {
			t.Errorf("%s: must be refused (it reads every node)", name)
		}
	}
	f := load(t, `
  - {id: a, kind: transform, transform: {set: {x: 1}}}
  - id: ok
    kind: request
    request: {url: 'http://x/{{ range .items }}{{ . }}{{ end }}{{ with .y }}{{ . }}{{ end }}{{ .nodes.a.out.x }}'}
`)
	if d := deps(f, "ok"); !reflect.DeepEqual(d, []string{"a"}) {
		t.Errorf("a scoped dot is the item, not the root: %v", d)
	}
}

// A self-referential anchor must be an error, never a stack overflow; a quoted "<<" is data.
func TestYAMLCyclesAndQuotedMergeKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.yaml")
	os.WriteFile(p, []byte("version: 1\nname: t\nnodes:\n  - id: c\n    kind: assertion\n    assert:\n      - &a {path: x, <<: *a}\n"), 0o644)
	_, err := Load(p) // must return (any result), not crash
	_ = err

	f := load(t, `
  - id: q
    kind: assertion
    assert:
      - {path: nodes.x.out.v, equals: {"<<": 7, plain: 1.50}}
  - {id: x, kind: transform, transform: {set: {v: 1}}}
`)
	got, _ := f.Nodes[0].Assert[0].Equals.(map[string]any)
	if got["<<"] == nil || got["plain"] != json.Number("1.50") {
		t.Fatalf("a quoted << key is ordinary data: %#v", got)
	}
}
