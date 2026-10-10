package runner

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"conduit/internal/flow"
	"conduit/internal/report"
)

func runYAML(t *testing.T, srvURL, yaml string) *report.Run {
	t.Helper()
	dir := t.TempDir()
	p := dir + "/f.yaml"
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := flow.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := Run(f, &EnvConfig{Vars: map[string]string{"base": srvURL}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func step(r *report.Run, id string) report.Step {
	for _, s := range r.Steps {
		if s.ID == id {
			return s
		}
	}
	return report.Step{}
}

func TestTimeoutFailsStepAndSkipsDependents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer srv.Close()
	r := runYAML(t, srv.URL, `
version: 1
name: t
nodes:
  - {id: slow, kind: request, request: {url: "{{ .env.base }}/", timeout: 100ms}}
  - {id: next, kind: transform, needs: [slow], transform: {set: {a: 1}}}
`)
	if step(r, "slow").Status != report.Failed || step(r, "next").Status != report.Skipped {
		t.Fatalf("got %+v", r.Steps)
	}
	if step(r, "slow").Request == nil {
		t.Fatal("request should be kept in evidence on failure")
	}
}

func TestExtractErrorIncludesStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"nope"}`))
	}))
	defer srv.Close()
	r := runYAML(t, srv.URL, `
version: 1
name: t
nodes:
  - id: a
    kind: request
    request: {url: "{{ .env.base }}/", extract: {tok: body.token}}
`)
	e := step(r, "a").Error
	if !strings.Contains(e, "HTTP 401") || !strings.Contains(e, "nope") {
		t.Fatalf("unhelpful error: %s", e)
	}
}

func TestNonJSONBodyAndHeaderLookup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Thing", "abc")
		w.Write([]byte("plain text"))
	}))
	defer srv.Close()
	r := runYAML(t, srv.URL, `
version: 1
name: t
nodes:
  - {id: a, kind: request, request: {url: "{{ .env.base }}/"}}
  - id: chk
    kind: assertion
    assert:
      - {path: nodes.a.response.raw, contains: plain}
      - {path: nodes.a.response.headers.x-thing, equals: abc}
      - {path: nodes.a.response.body, exists: false}
`)
	if r.Status != report.Passed {
		t.Fatalf("%+v", step(r, "chk"))
	}
}

func TestTransformKeepsNativeTypesAndInfersDeps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"n": 5, "items": [{"id": 7}]}`))
	}))
	defer srv.Close()
	r := runYAML(t, srv.URL, `
version: 1
name: t
nodes:
  - id: chk
    kind: assertion
    assert:
      - {path: nodes.tr.out.n, equals: 5}
      - {path: nodes.tr.out.first, equals: 7}
  - id: tr
    kind: transform
    transform: {set: {n: "{{ .nodes.a.response.body.n }}", first: "{{ .nodes.a.response.body.items.0.id }}"}}
  - {id: a, kind: request, request: {url: "{{ .env.base }}/"}}
`)
	if r.Status != report.Passed {
		t.Fatalf("%+v", r.Steps)
	}
}

func TestErrorKinds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"a":1}`))
	}))
	defer srv.Close()
	dead := httptest.NewServer(nil)
	deadURL := dead.URL
	dead.Close()

	r := runYAML(t, srv.URL, `
version: 1
name: t
nodes:
  - {id: ok, kind: request, request: {url: "{{ .env.base }}/"}}
  - {id: wrong, kind: assertion, assert: [{path: nodes.ok.response.body.a, equals: 2}]}
  - {id: missing_field, kind: request, request: {url: "{{ .env.base }}/", extract: {x: body.nope}}}
  - {id: bad_tpl, kind: request, request: {url: "{{ .env.nope }}/"}}
  - {id: bad_re, kind: assertion, assert: [{path: nodes.ok.response.raw, matches: "("}]}
  - {id: bad_timeout, kind: request, request: {url: "{{ .env.base }}/", timeout: soon}}
`)
	want := map[string]string{
		"wrong": KindAssertion, "missing_field": KindAssertion,
		"bad_tpl": KindSpec, "bad_re": KindSpec, "bad_timeout": KindSpec,
	}
	for id, k := range want {
		if got := step(r, id).ErrorKind; got != k {
			t.Errorf("%s: kind %q, want %q (%s)", id, got, k, step(r, id).Error)
		}
	}
	if step(r, "ok").ErrorKind != "" {
		t.Error("passing step must have no error_kind")
	}

	r = runYAML(t, deadURL, `
version: 1
name: t
nodes:
  - {id: down, kind: request, request: {url: "{{ .env.base }}/"}}
  - {id: after, kind: transform, needs: [down], transform: {set: {a: 1}}}
`)
	if step(r, "down").ErrorKind != KindInfra {
		t.Errorf("connection refused: kind %q", step(r, "down").ErrorKind)
	}
	if step(r, "after").ErrorKind != "" {
		t.Error("skipped step must not carry an error_kind")
	}
}
