package runner

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"sleuth/internal/flow"
	"sleuth/internal/report"
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

const fakeSpec = `{"openapi":"3.1.0","paths":{
 "/users/{id}":{"get":{"responses":{"200":{"content":{"application/json":{"schema":
    {"type":"object","required":["id","name"],"properties":{"id":{"type":"integer"},"name":{"type":"string"}}}}}}}}},
 "/users":{"post":{"requestBody":{"required":true,"content":{"application/json":{"schema":
    {"type":"object","required":["name"],"additionalProperties":false,"properties":{"name":{"type":"string"}}}}}},
    "responses":{"201":{}}}},
 "/empty":{"get":{"responses":{"200":{"content":{"application/json":{"schema":{}}}}}}}
}}`

func runContract(t *testing.T, yaml string, handler http.HandlerFunc) *report.Run {
	t.Helper()
	srv := httptest.NewServer(handler)
	defer srv.Close()
	dir := t.TempDir()
	os.WriteFile(dir+"/spec.json", []byte(fakeSpec), 0o644)
	os.WriteFile(dir+"/f.yaml", []byte(yaml), 0o644)
	f, err := flow.Load(dir + "/f.yaml")
	if err != nil {
		t.Fatal(err)
	}
	env := &EnvConfig{Vars: map[string]string{"base": srv.URL}, Contracts: map[string]string{"api": dir + "/spec.json"}}
	r, _, err := Run(f, env, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestContractResponseDriftFailsWithContractKind(t *testing.T) {
	r := runContract(t, `
version: 1
name: t
nodes:
  - {id: a, kind: request, contract: api, request: {url: "{{ .env.base }}/users/7"}}
  - {id: b, kind: transform, needs: [a], transform: {set: {x: 1}}}
`, func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"id":"7"}`)) }) // wrong type + missing name
	a := step(r, "a")
	if a.Status != report.Failed || a.ErrorKind != KindContract {
		t.Fatalf("%+v", a)
	}
	if a.Response == nil || a.Contract.Operation != "GET /users/{id}" || len(a.Contract.Violations) != 2 {
		t.Fatalf("%+v", a.Contract)
	}
	if step(r, "b").Status != report.Skipped {
		t.Fatal("dependent must be skipped")
	}
}

func TestContractWarnModePassesButRecords(t *testing.T) {
	r := runContract(t, `
version: 1
name: t
nodes:
  - {id: a, kind: request, contract: {source: api, on_violation: warn}, request: {url: "{{ .env.base }}/users/7"}}
`, func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"id":"7","name":"x"}`)) })
	a := step(r, "a")
	if a.Status != report.Passed || len(a.Contract.Violations) != 1 {
		t.Fatalf("%+v %+v", a, a.Contract)
	}
}

func TestContractBadRequestIsNotSent(t *testing.T) {
	hits := 0
	r := runContract(t, `
version: 1
name: t
nodes:
  - id: a
    kind: request
    contract: api
    request: {method: POST, url: "{{ .env.base }}/users", body: {name: 5, junk: true}}
`, func(w http.ResponseWriter, _ *http.Request) { hits++ })
	a := step(r, "a")
	if a.ErrorKind != KindContract || hits != 0 || a.Response != nil {
		t.Fatalf("hits=%d %+v", hits, a)
	}
	if len(a.Contract.Violations) != 2 {
		t.Fatalf("%+v", a.Contract.Violations)
	}
}

func TestContractUndocumentedStatusAndOperationAndEmptySchema(t *testing.T) {
	r := runContract(t, `
version: 1
name: t
nodes:
  - {id: undocumented, kind: request, contract: api, request: {url: "{{ .env.base }}/users/7"}}
  - {id: unknown_op, kind: request, contract: api, request: {url: "{{ .env.base }}/nope"}}
  - {id: empty, kind: request, contract: api, request: {url: "{{ .env.base }}/empty"}}
`, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/users") {
			w.WriteHeader(401)
		}
		w.Write([]byte(`{}`))
	})
	if v := step(r, "undocumented").Contract.Violations; len(v) != 1 || !strings.Contains(v[0].Message, "not documented") {
		t.Fatalf("%+v", v)
	}
	if v := step(r, "unknown_op").Contract.Violations; len(v) != 1 || v[0].Where != "operation" {
		t.Fatalf("%+v", v)
	}
	e := step(r, "empty")
	if e.Status != report.Passed || len(e.Contract.Notes) != 1 {
		t.Fatalf("empty schema should pass with a note: %+v %+v", e, e.Contract)
	}
}

func TestMissingContractSourceIsSetupError(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(dir+"/f.yaml", []byte("version: 1\nname: t\nnodes:\n- {id: a, kind: request, contract: nope, request: {url: \"http://x\"}}\n"), 0o644)
	f, err := flow.Load(dir + "/f.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Run(f, &EnvConfig{}, Options{}); err == nil {
		t.Fatal("expected setup error")
	}
}

func TestUnusedContractSourceDoesNotBlockRun(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(dir+"/e.yaml", []byte("vars:\n  base: http://127.0.0.1:1\ncontracts:\n  api: ${SLEUTH_TEST_UNSET_VAR}\n"), 0o644)
	os.Unsetenv("SLEUTH_TEST_UNSET_VAR")
	env, err := LoadEnv(dir + "/e.yaml")
	if err != nil {
		t.Fatalf("an unused contract must not fail env loading: %v", err)
	}
	if _, err := env.ContractLocation("api"); err == nil || !strings.Contains(err.Error(), "SLEUTH_TEST_UNSET_VAR") {
		t.Fatalf("using it must name the unset variable, got %v", err)
	}
}

func TestPartiallyUnsetVariableIsAnError(t *testing.T) {
	dir := t.TempDir()
	os.Unsetenv("SLEUTH_TEST_HOST")
	os.WriteFile(dir+"/e.yaml", []byte("contracts:\n  api: ${SLEUTH_TEST_HOST}/openapi.json\n"), 0o644)
	env, _ := LoadEnv(dir + "/e.yaml")
	if _, err := env.ContractLocation("api"); err == nil || !strings.Contains(err.Error(), "SLEUTH_TEST_HOST") {
		t.Fatalf("want unset-variable error, got %v", err)
	}
	os.WriteFile(dir+"/v.yaml", []byte("vars:\n  base: ${SLEUTH_TEST_HOST}/x\n"), 0o644)
	if _, err := LoadEnv(dir + "/v.yaml"); err == nil {
		t.Fatal("vars with an unset ${VAR} must fail too")
	}
	t.Setenv("SLEUTH_TEST_HOST", "http://h")
	if loc, err := env.ContractLocation("api"); err != nil || loc != "http://h/openapi.json" {
		t.Fatalf("%q %v", loc, err)
	}
}

func TestRelativeContractPathIsRelativeToEnvFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(dir+"/e.yaml", []byte("contracts:\n  api: specs/o.json\n"), 0o644)
	env, _ := LoadEnv(dir + "/e.yaml")
	if loc, _ := env.ContractLocation("api"); loc != dir+"/specs/o.json" {
		t.Fatalf("%q", loc)
	}
}

func TestContractNotesNonJSONBodies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("hello")) }))
	defer srv.Close()
	dir := t.TempDir()
	os.WriteFile(dir+"/spec.json", []byte(`{"openapi":"3.1.0","servers":[{"url":"/api"}],"paths":{"/t":{"get":{"responses":{"200":{"content":{"text/plain":{"schema":{"type":"string"}}}}}}}}}`), 0o644)
	os.WriteFile(dir+"/f.yaml", []byte(`
version: 1
name: t
nodes:
  - {id: a, kind: request, contract: api, request: {url: "{{ .env.base }}/api/t"}}
`), 0o644)
	f, err := flow.Load(dir + "/f.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := Run(f, &EnvConfig{Vars: map[string]string{"base": srv.URL}, Contracts: map[string]string{"api": dir + "/spec.json"}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	a := step(r, "a")
	if a.Status != report.Passed || len(a.Contract.Notes) != 1 || !strings.Contains(a.Contract.Notes[0], "text/plain") || a.Contract.Operation != "GET /t" {
		t.Fatalf("%+v %+v", a, a.Contract)
	}
}

func TestRunReportsRegistryCoverage(t *testing.T) {
	r := runContract(t, `
version: 1
name: t
nodes:
  - {id: a, kind: request, contract: api, request: {url: "{{ .env.base }}/users/7"}}
  - {id: after, kind: transform, needs: [a], transform: {set: {x: 1}}}
`, func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"id":7,"name":"x"}`)) })
	if r.Drift == nil || len(r.Drift.Sources) != 1 {
		t.Fatalf("%+v", r.Drift)
	}
	sc := r.Drift.Sources[0]
	if sc.Source != "api" || sc.Total != 3 || sc.Exercised != 1 || strings.Join(sc.Untested, "|") != "GET /empty|POST /users" {
		t.Fatalf("%+v", sc)
	}
}

// baselineSpec is fakeSpec as it was when the flow was written: it still has GET /gone, and the
// user response still carried a "nick" property the live spec has since dropped.
const baselineSpec = `{"openapi":"3.1.0","paths":{
 "/users/{id}":{"get":{"responses":{"200":{"content":{"application/json":{"schema":
    {"type":"object","required":["id","name"],"properties":{"id":{"type":"integer"},"name":{"type":"string"},"nick":{"type":"string"}}}}}}}}},
 "/users":{"post":{"requestBody":{"required":true,"content":{"application/json":{"schema":
    {"type":"object","required":["name"],"additionalProperties":false,"properties":{"name":{"type":"string"}}}}}},
    "responses":{"201":{}}}},
 "/empty":{"get":{"responses":{"200":{"content":{"application/json":{"schema":{}}}}}}},
 "/gone":{"get":{"responses":{"200":{}}}}
}}`

func runWithBaseline(t *testing.T, baseline, failOn string) (*report.Run, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"id":7,"name":"x"}`)) }))
	defer srv.Close()
	dir := t.TempDir()
	os.WriteFile(dir+"/spec.json", []byte(fakeSpec), 0o644)
	os.WriteFile(dir+"/base.json", []byte(baselineSpec), 0o644)
	os.WriteFile(dir+"/f.yaml", []byte(`
version: 1
name: t
nodes:
  - {id: a, kind: request, contract: api, request: {url: "{{ .env.base }}/users/7"}}
`), 0o644)
	f, err := flow.Load(dir + "/f.yaml")
	if err != nil {
		t.Fatal(err)
	}
	env := &EnvConfig{Vars: map[string]string{"base": srv.URL}, Contracts: map[string]string{"api": dir + "/spec.json"}, path: dir + "/env.yaml"}
	if baseline != "" {
		env.Baselines = map[string]string{"api": baseline} // relative: resolved against the env file's directory
	}
	r, _, err := Run(f, env, Options{FailOnSpecDrift: failOn})
	return r, err
}

func TestRunComparesLiveSpecWithSavedBaseline(t *testing.T) {
	r, err := runWithBaseline(t, "base.json", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != report.Passed || r.Drift == nil || len(r.Drift.SpecDrift) != 1 {
		t.Fatalf("report-only drift must not fail the run: %s %+v", r.Status, r.Drift)
	}
	sd := r.Drift.SpecDrift[0]
	if sd.Source != "api" || sd.Breaking != 2 || sd.CalledBreaking != 1 || sd.Failing {
		t.Fatalf("%+v", sd)
	}
	var sawProp, sawGone bool
	for _, c := range sd.Changes {
		if c.Operation == "GET /users/{id}" && strings.Contains(c.Message, "property removed") && c.Called {
			sawProp = true
		}
		if c.Operation == "GET /gone" && c.Message == "operation removed" && !c.Called {
			sawGone = true
		}
	}
	if !sawProp || !sawGone {
		t.Fatalf("%+v", sd.Changes)
	}
}

func TestRunFailsOnSpecDriftWhenAsked(t *testing.T) {
	r, err := runWithBaseline(t, "base.json", "breaking")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != report.Failed || !r.Drift.SpecDrift[0].Failing || step(r, "a").Status != report.Passed {
		t.Fatalf("the step passed but the run must fail on the drifted spec: %s %+v", r.Status, r.Drift.SpecDrift[0])
	}
}

func TestRunWithoutBaselineHasNoSpecDrift(t *testing.T) {
	r, err := runWithBaseline(t, "", "breaking")
	if err != nil || r.Drift == nil || len(r.Drift.SpecDrift) != 0 || r.Status != report.Passed {
		t.Fatalf("%v %+v", err, r)
	}
}

func TestRunWithUnreadableBaselineIsASetupError(t *testing.T) {
	if _, err := runWithBaseline(t, "missing-baseline.json", ""); err == nil || !strings.Contains(err.Error(), `baseline for contract source "api"`) {
		t.Fatalf("a drift check that silently did not run must be an error, got %v", err)
	}
}
