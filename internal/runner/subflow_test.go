package runner

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sleuth/internal/flow"
	"sleuth/internal/report"
)

// runTree writes files under a temp dir, loads top.yaml and runs it.
func runTree(t *testing.T, srvURL string, files map[string]string, opts Options) *report.Run {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f, err := flow.Load(filepath.Join(dir, "top.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	env := &EnvConfig{Vars: map[string]string{"base": srvURL}, Secrets: nil, Contracts: map[string]string{"api": filepath.Join(dir, "spec.json")}}
	r, _, err := Run(f, env, opts)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func child(s report.Step, id string) report.Step {
	for _, c := range s.Children {
		if c.ID == id {
			return c
		}
	}
	return report.Step{}
}

const loginFlow = `version: 1
name: auth
params:
  - username
  - {name: scope, default: read}
nodes:
  - id: login
    kind: request
    request:
      method: POST
      url: "{{ .env.base }}/login"
      body: {user: "{{ .params.username }}", scope: "{{ .params.scope }}"}
  - id: ok
    kind: assertion
    assert: [{path: nodes.login.response.status, equals: 200}]
outputs:
  token: "{{ .nodes.login.response.body.token }}"
`

func loginServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			fmt.Fprintf(w, `{"token":"tok-%v-%v"}`, b["user"], b["scope"])
			return
		}
		fmt.Fprintf(w, `{"path":%q,"auth":%q}`, r.URL.Path, r.Header.Get("Authorization"))
	}))
}

func TestSubflowRunsAndPassesOutputsToTheCaller(t *testing.T) {
	srv := loginServer()
	defer srv.Close()
	for _, workers := range []int{1, 4} {
		t.Run(fmt.Sprint("parallel", workers), func(t *testing.T) {
			r := runTree(t, srv.URL, map[string]string{
				"top.yaml": `version: 1
name: top
nodes:
  - {id: auth, kind: flow, flow: _auth.yaml, params: {username: bob}}
  - id: me
    kind: request
    request: {url: "{{ .env.base }}/me", headers: {Authorization: "Bearer {{ .nodes.auth.out.token }}"}}
`,
				"_auth.yaml": loginFlow,
			}, Options{Parallel: workers})
			if r.Status != report.Passed {
				t.Fatalf("run failed: %+v", r.Steps)
			}
			auth := step(r, "auth")
			if auth.Kind != "flow" || auth.Flow != "_auth.yaml" || len(auth.Children) != 2 {
				t.Fatalf("auth step: %+v", auth)
			}
			if auth.Params["username"] != "bob" || auth.Params["scope"] != "read" {
				t.Errorf("params (given and default) not recorded: %v", auth.Params)
			}
			if auth.Output["token"] != "tok-bob-read" {
				t.Errorf("outputs: %v", auth.Output)
			}
			if got := step(r, "me").Request.Headers["Authorization"]; got != "Bearer tok-bob-read" {
				t.Errorf("the caller did not receive the output: %q", got)
			}
		})
	}
}

func TestSubflowInternalsAreHiddenFromTheCaller(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(dir+"/top.yaml", []byte(`version: 1
name: top
nodes:
  - {id: auth, kind: flow, flow: _auth.yaml, params: {username: bob}}
  - id: peek
    kind: transform
    needs: [auth]
    transform: {set: {t: "{{ .nodes.login.response.status }}"}}
`), 0o644)
	os.WriteFile(dir+"/_auth.yaml", []byte(loginFlow), 0o644)
	// "login" is a node of the subflow, not of the caller: the caller cannot reach it
	if _, err := flow.Load(dir + "/top.yaml"); err == nil || !strings.Contains(err.Error(), `unknown node "login"`) {
		t.Fatalf("want an unknown-node error, got %v", err)
	}
}

func TestSubflowFailureKindsPropagate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	r := runTree(t, srv.URL, map[string]string{
		"top.yaml": `version: 1
name: top
nodes:
  - {id: auth, kind: flow, flow: _auth.yaml, params: {username: bob}}
  - {id: after, kind: transform, needs: [auth], transform: {set: {a: 1}}}
`,
		"_auth.yaml": loginFlow,
	}, Options{})
	auth := step(r, "auth")
	if r.Status != report.Failed || auth.Status != report.Failed || auth.ErrorKind != KindAssertion {
		t.Fatalf("auth: %+v", auth)
	}
	if !strings.Contains(auth.Error, "step ok failed") && !strings.Contains(auth.Error, "step login failed") {
		t.Errorf("error should name the inner step: %s", auth.Error)
	}
	if child(auth, "ok").Status != report.Failed {
		t.Errorf("the inner failure should be in the children: %+v", auth.Children)
	}
	if step(r, "after").Status != report.Skipped {
		t.Errorf("a dependent of a failed subflow must be skipped")
	}
}

func TestSubflowMostSevereKindWins(t *testing.T) {
	// one inner step fails as an assertion, another cannot reach the host: infrastructure must not
	// hide the product failure, and a flow problem outranks both
	if got := childFailure("x", []report.Step{
		{ID: "a", Status: report.Failed, ErrorKind: KindInfra, Error: "down"},
		{ID: "b", Status: report.Failed, ErrorKind: KindAssertion, Error: "wrong"},
	}); kindOf(got) != KindAssertion || !strings.Contains(got.Error(), "step a failed") {
		t.Errorf("assertion should outrank infra, message names the first failure: %v (%s)", got, kindOf(got))
	}
	if got := childFailure("x", []report.Step{
		{ID: "a", Status: report.Failed, ErrorKind: KindAssertion},
		{ID: "b", Status: report.Failed, ErrorKind: KindSpec},
	}); kindOf(got) != KindSpec {
		t.Errorf("spec should outrank assertion: %s", kindOf(got))
	}
	if childFailure("x", []report.Step{{ID: "a", Status: report.Passed}, {ID: "b", Status: report.Skipped}}) != nil {
		t.Error("no failed step means no failure")
	}
}

func TestSubflowRequiredParamWhenRunDirectly(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(dir+"/f.yaml", []byte(loginFlow), 0o644)
	f, err := flow.Load(dir + "/f.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = Run(f, &EnvConfig{Vars: map[string]string{}}, Options{})
	if err == nil || !strings.Contains(err.Error(), "username") {
		t.Fatalf("a flow with a required param cannot run on its own, got %v", err)
	}
}

func TestSubflowOutputErrorIsASpecFailure(t *testing.T) {
	srv := loginServer()
	defer srv.Close()
	r := runTree(t, srv.URL, map[string]string{
		"top.yaml":   "version: 1\nname: top\nnodes:\n  - {id: auth, kind: flow, flow: _auth.yaml, params: {username: bob}}\n",
		"_auth.yaml": strings.Replace(loginFlow, ".response.body.token", ".response.body.nothing", 1),
	}, Options{})
	a := step(r, "auth")
	if a.Status != report.Failed || a.ErrorKind != KindSpec || !strings.Contains(a.Error, `output "token"`) {
		t.Fatalf("%+v", a)
	}
	// login and ok passed; the subflow step itself carries the failure, so it counts as a leaf
	var ids []string
	for _, l := range report.Leaves(r.Steps) {
		ids = append(ids, l.ID)
	}
	if strings.Join(ids, ",") != "auth,auth/login,auth/ok" {
		t.Errorf("leaves: %v", ids)
	}
}

func TestNestedSubflows(t *testing.T) {
	srv := loginServer()
	defer srv.Close()
	r := runTree(t, srv.URL, map[string]string{
		"top.yaml": "version: 1\nname: top\nnodes:\n  - {id: outer, kind: flow, flow: lib/outer.yaml}\n",
		"lib/outer.yaml": `version: 1
name: outer
nodes:
  - {id: inner, kind: flow, flow: inner.yaml, params: {who: amy}}
outputs:
  t: "{{ .nodes.inner.out.t }}"
`,
		"lib/inner.yaml": `version: 1
name: inner
params: [who]
nodes:
  - {id: x, kind: transform, transform: {set: {t: "hi-{{ .params.who }}"}}}
outputs: {t: "{{ .nodes.x.out.t }}"}
`,
	}, Options{})
	if r.Status != report.Passed {
		t.Fatalf("%+v", r.Steps)
	}
	if got := step(r, "outer").Output["t"]; got != "hi-amy" {
		t.Errorf("output through two levels: %v", got)
	}
	var ids []string
	for _, s := range report.Flatten(r.Steps) {
		ids = append(ids, s.ID)
	}
	if strings.Join(ids, ",") != "outer,outer/inner,outer/inner/x" {
		t.Errorf("path ids: %v", ids)
	}
}

func TestSubflowCallerParamTypesSurvive(t *testing.T) {
	r := runTree(t, "", map[string]string{
		"top.yaml": `version: 1
name: top
nodes:
  - {id: n, kind: transform, transform: {set: {id: 42}}}
  - {id: sub, kind: flow, flow: _s.yaml, params: {id: "{{ .nodes.n.out.id }}"}}
`,
		"_s.yaml": `version: 1
name: s
params: [id]
nodes:
  - id: chk
    kind: assertion
    assert: [{path: params.id, equals: 42}]
`,
	}, Options{})
	if r.Status != report.Passed {
		t.Fatalf("a numeric param must stay a number: %+v", child(step(r, "sub"), "chk"))
	}
}

func TestSubflowSecretsAreMaskedInNestedEvidence(t *testing.T) {
	srv := loginServer()
	defer srv.Close()
	t.Setenv("SLEUTH_SUBFLOW_SECRET", "s3cr3t-value")
	dir := t.TempDir()
	os.WriteFile(dir+"/top.yaml", []byte("version: 1\nname: top\nnodes:\n  - {id: auth, kind: flow, flow: _a.yaml, params: {username: \"{{ .secrets.SLEUTH_SUBFLOW_SECRET }}\"}}\n"), 0o644)
	os.WriteFile(dir+"/_a.yaml", []byte(loginFlow), 0o644)
	f, err := flow.Load(dir + "/top.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r, secrets, err := Run(f, &EnvConfig{Vars: map[string]string{"base": srv.URL}, Secrets: []string{"SLEUTH_SUBFLOW_SECRET"}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	out := dir + "/ev.json"
	if err := report.WriteJSON(out, r, secrets); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	if strings.Contains(string(b), "s3cr3t-value") {
		t.Fatal("a secret passed as a param leaked into the nested evidence")
	}
	if !strings.Contains(string(b), "***") {
		t.Fatal("expected the masked value to appear (the secret was used in the request)")
	}
}

func TestSubflowContractDriftAndCoverageCountInsideSubflows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"id":"7"}`)) }))
	defer srv.Close()
	r := runTree(t, srv.URL, map[string]string{
		"top.yaml": "version: 1\nname: top\nnodes:\n  - {id: sub, kind: flow, flow: _s.yaml}\n",
		"_s.yaml": `version: 1
name: s
nodes:
  - {id: get, kind: request, contract: api, request: {url: "{{ .env.base }}/users/7"}}
`,
		"spec.json": fakeSpec,
	}, Options{})
	if r.Status != report.Failed || step(r, "sub").ErrorKind != KindContract {
		t.Fatalf("a contract failure inside a subflow must fail the run as a contract failure: %+v", step(r, "sub"))
	}
	if r.Drift == nil || r.Drift.CallsChecked != 1 || len(r.Drift.Findings) == 0 || r.Drift.Findings[0].Step != "sub/get" {
		t.Fatalf("drift should see the inner call under its path: %+v", r.Drift)
	}
	var covered bool
	for _, s := range r.Drift.Sources {
		covered = covered || (s.Source == "api" && s.Exercised == 1)
	}
	if !covered {
		t.Errorf("coverage should count the operation called inside the subflow: %+v", r.Drift.Sources)
	}
}

func TestSubflowRunsInsideTheParentsWorkerSlot(t *testing.T) {
	// at --parallel 1 and 2, several subflow nodes, each with several steps, must finish
	// (a subflow that asked the shared pool for slots would deadlock)
	srv := loginServer()
	defer srv.Close()
	files := map[string]string{"_a.yaml": loginFlow}
	top := "version: 1\nname: top\nnodes:\n"
	for i := 0; i < 4; i++ {
		top += fmt.Sprintf("  - {id: a%d, kind: flow, flow: _a.yaml, params: {username: u%d}}\n", i, i)
	}
	files["top.yaml"] = top
	for _, w := range []int{1, 2} {
		done := make(chan *report.Run, 1)
		go func() { done <- runTree(t, srv.URL, files, Options{Parallel: w}) }()
		select {
		case r := <-done:
			if r.Status != report.Passed {
				t.Fatalf("parallel %d: %+v", w, r.Steps)
			}
		case <-time.After(20 * time.Second):
			t.Fatalf("parallel %d: deadlock", w)
		}
	}
}

func TestSubflowStopsAtTheRunTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(2 * time.Second) }))
	defer srv.Close()
	start := time.Now()
	r := runTree(t, srv.URL, map[string]string{
		"top.yaml": "version: 1\nname: top\nnodes:\n  - {id: sub, kind: flow, flow: _s.yaml}\n",
		"_s.yaml": `version: 1
name: s
nodes:
  - {id: slow, kind: request, request: {url: "{{ .env.base }}/"}}
  - {id: next, kind: transform, needs: [slow], transform: {set: {a: 1}}}
`,
	}, Options{Timeout: 200 * time.Millisecond})
	if time.Since(start) > 1500*time.Millisecond || !r.TimedOut || step(r, "sub").ErrorKind != KindInfra {
		t.Fatalf("took %s, timed out %v, %+v", time.Since(start), r.TimedOut, step(r, "sub"))
	}
}
