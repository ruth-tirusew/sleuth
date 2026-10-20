package flow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree writes files (path -> content) under a new directory and returns it.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const authFlow = `version: 1
name: auth
params:
  - username
  - {name: scope, default: read}
nodes:
  - id: login
    kind: request
    request: {method: POST, url: "http://x/login", body: {u: "{{ .params.username }}"}}
outputs:
  token: "{{ .nodes.login.response.body.token }}"
`

func callAuth(params string) string {
	return `version: 1
name: top
nodes:
  - id: auth
    kind: flow
    flow: _auth.flow.yaml
` + params + `
  - id: use
    kind: request
    request: {url: "http://x/{{ .nodes.auth.out.token }}"}
`
}

func TestSubflowLoadsAndIsADependency(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"top.flow.yaml":   callAuth("    params: {username: bob}"),
		"_auth.flow.yaml": authFlow,
	})
	f, err := Load(filepath.Join(dir, "top.flow.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Nodes[0].Sub == nil || f.Nodes[0].Sub.Name != "auth" {
		t.Fatalf("subflow not loaded: %+v", f.Nodes[0].Sub)
	}
	if got := deps(f, "use"); len(got) != 1 || got[0] != "auth" {
		t.Errorf("use should depend on the subflow node, got %v", got)
	}
	if len(f.Files) != 2 || filepath.Base(f.Files[0]) != "top.flow.yaml" || filepath.Base(f.Files[1]) != "_auth.flow.yaml" {
		t.Errorf("Files = %v", f.Files)
	}
}

func TestSubflowParamTemplatesCreateDependencies(t *testing.T) {
	top := `version: 1
name: top
nodes:
  - id: who
    kind: transform
    transform: {set: {name: bob}}
  - id: auth
    kind: flow
    flow: _auth.flow.yaml
    params: {username: "{{ .nodes.who.out.name }}"}
`
	dir := writeTree(t, map[string]string{"top.yaml": top, "_auth.flow.yaml": authFlow})
	f, err := Load(filepath.Join(dir, "top.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := deps(f, "auth"); len(got) != 1 || got[0] != "who" {
		t.Errorf("auth should wait for who, got %v", got)
	}
}

func TestSubflowErrors(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"missing required param", map[string]string{"top.yaml": callAuth(""), "_auth.flow.yaml": authFlow}, "missing required param(s) username"},
		{"unknown param", map[string]string{"top.yaml": callAuth("    params: {username: a, nope: 1}"), "_auth.flow.yaml": authFlow}, "unknown param(s) nope"},
		{"file missing", map[string]string{"top.yaml": callAuth("    params: {username: a}")}, "_auth.flow.yaml"},
		{"escapes the directory", map[string]string{
			"sub/top.yaml":    strings.Replace(callAuth("    params: {username: a}"), "_auth.flow.yaml", "../_auth.flow.yaml", 1),
			"_auth.flow.yaml": authFlow,
		}, "outside the directory"},
		{"absolute path", map[string]string{"top.yaml": strings.Replace(callAuth("    params: {username: a}"), "_auth.flow.yaml", "/etc/passwd", 1)}, "must be relative"},
		{"templated path", map[string]string{"top.yaml": strings.Replace(callAuth("    params: {username: a}"), "_auth.flow.yaml", "\"{{ .env.x }}.yaml\"", 1)}, "plain path"},
		{"direct cycle", map[string]string{"top.yaml": `version: 1
name: top
nodes:
  - {id: again, kind: flow, flow: top.yaml}
`}, "include themselves: top.yaml -> top.yaml"},
		{"indirect cycle", map[string]string{
			"a.yaml": "version: 1\nname: a\nnodes:\n  - {id: n, kind: flow, flow: b.yaml}\n",
			"b.yaml": "version: 1\nname: b\nnodes:\n  - {id: n, kind: flow, flow: a.yaml}\n",
		}, "a.yaml -> b.yaml -> a.yaml"},
		{"output reads unknown node", map[string]string{"top.yaml": callAuth("    params: {username: a}"), "_auth.flow.yaml": strings.Replace(authFlow, ".nodes.login.", ".nodes.ghost.", 1)}, `output token reads node "ghost"`},
		{"flow field on a request", map[string]string{"top.yaml": "version: 1\nname: t\nnodes:\n  - {id: a, kind: request, flow: x.yaml, request: {url: http://x}}\n"}, "only valid on flow nodes"},
		{"flow node without a path", map[string]string{"top.yaml": "version: 1\nname: t\nnodes:\n  - {id: a, kind: flow}\n"}, "flow is required"},
		{"duplicate param", map[string]string{"top.yaml": callAuth("    params: {username: a}"), "_auth.flow.yaml": strings.Replace(authFlow, "  - {name: scope, default: read}", "  - username", 1)}, `duplicate param "username"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := writeTree(t, c.files)
			top := "top.yaml"
			if _, ok := c.files["sub/top.yaml"]; ok {
				top = "sub/top.yaml"
			}
			if _, ok := c.files["a.yaml"]; ok {
				top = "a.yaml"
			}
			_, err := Load(filepath.Join(dir, top))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestSubflowDepthLimit(t *testing.T) {
	files := map[string]string{}
	for i := 0; i <= MaxSubflowDepth+1; i++ {
		body := "version: 1\nname: f\nnodes:\n  - {id: n, kind: flow, flow: f" + string(rune('a'+i+1)) + ".yaml}\n"
		if i == MaxSubflowDepth+1 {
			body = "version: 1\nname: leaf\nnodes:\n  - {id: n, kind: transform, transform: {set: {a: b}}}\n"
		}
		files["f"+string(rune('a'+i))+".yaml"] = body
	}
	dir := writeTree(t, files)
	if _, err := Load(filepath.Join(dir, "fa.yaml")); err == nil || !strings.Contains(err.Error(), "nested more than") {
		t.Fatalf("want a depth error, got %v", err)
	}
}

func TestSubflowCanLiveInASubdirectoryAndIncludeSiblings(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"top.yaml":       "version: 1\nname: top\nnodes:\n  - {id: a, kind: flow, flow: lib/outer.yaml}\n",
		"lib/outer.yaml": "version: 1\nname: outer\nnodes:\n  - {id: b, kind: flow, flow: inner.yaml}\n",
		"lib/inner.yaml": "version: 1\nname: inner\nnodes:\n  - {id: c, kind: transform, transform: {set: {a: b}}}\n",
	})
	f, err := Load(filepath.Join(dir, "top.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Nodes[0].Sub.Nodes[0].Sub == nil {
		t.Fatal("the nested subflow was not resolved relative to its includer")
	}
	if len(f.Files) != 3 {
		t.Errorf("Files = %v", f.Files)
	}
}

func TestSymlinkCannotEscapeTheDirectory(t *testing.T) {
	outside := writeTree(t, map[string]string{"evil.yaml": authFlow})
	dir := writeTree(t, map[string]string{"top.yaml": "version: 1\nname: top\nnodes:\n  - {id: a, kind: flow, flow: link.yaml, params: {username: x}}\n"})
	if err := os.Symlink(filepath.Join(outside, "evil.yaml"), filepath.Join(dir, "link.yaml")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := Load(filepath.Join(dir, "top.yaml")); err == nil || !strings.Contains(err.Error(), "outside the directory") {
		t.Fatalf("a symlink out of the tree must be refused, got %v", err)
	}
}

// `sleuth run flow.yaml` passes a relative path: siblings and the files list must still resolve.
func TestSubflowWithARelativePath(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"top.yaml":        callAuth("    params: {username: bob}"),
		"_auth.flow.yaml": authFlow,
	})
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	f, err := Load("top.yaml")
	if err != nil {
		t.Fatalf("a relative path must work: %v", err)
	}
	for _, p := range f.Files {
		if !filepath.IsAbs(p) {
			t.Errorf("Files should hold absolute paths, got %q", p)
		}
	}
	if len(f.Files) != 2 {
		t.Errorf("Files = %v", f.Files)
	}
}

func TestFinallyValidation(t *testing.T) {
	const head = "version: 1\nname: t\nnodes:\n  - {id: make, kind: request, request: {url: 'http://x/make'}}\n"
	cases := map[string]struct{ yaml, want string }{
		"ok": {head + `finally:
  - {id: drop, kind: request, request: {method: DELETE, url: "http://x/{{ .nodes.make.response.body.id }}"}}
  - {id: drop2, kind: request, needs: [drop], request: {method: DELETE, url: "http://x/2"}}
`, ""},
		"db refused": {head + `finally:
  - {id: q, kind: db, db: {connection: c, query: "select 1"}, assert: [{path: nodes.q.row_count, equals: 1}]}
`, "read-only"},
		"subflow refused": {head + "finally:\n  - {id: s, kind: flow, flow: _x.yaml}\n", "cannot be a subflow"},
		"duplicate id":    {head + "finally:\n  - {id: make, kind: request, request: {url: 'http://x'}}\n", "duplicate node id"},
		"main reads cleanup": {"version: 1\nname: t\nnodes:\n  - {id: a, kind: request, request: {url: 'http://x/{{ .nodes.drop.response.status }}'}}\n" +
			"finally:\n  - {id: drop, kind: request, request: {url: 'http://x'}}\n", "unknown node"},
		"cleanup cycle": {head + "finally:\n  - {id: a, kind: request, needs: [b], request: {url: 'http://x'}}\n  - {id: b, kind: request, needs: [a], request: {url: 'http://x'}}\n", "cycle"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writeTree(t, map[string]string{"f.yaml": c.yaml, "_x.yaml": authFlow})
			_, err := Load(filepath.Join(dir, "f.yaml"))
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}
