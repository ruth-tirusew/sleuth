package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mock() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"token": "tok-123", "user_id": 42})
	})
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-123" {
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"username": "alice", "balance": 100.5})
	})
	return httptest.NewServer(mux)
}

func setup(t *testing.T, balance string) (flow, env, out string) {
	srv := mock()
	t.Cleanup(srv.Close)
	t.Setenv("APP_PASSWORD", "s3cret")
	dir := t.TempDir()
	flow = filepath.Join(dir, "f.yaml")
	src, _ := os.ReadFile("../../examples/login-and-profile.flow.yaml")
	os.WriteFile(flow, []byte(strings.Replace(string(src), "gt: 0", "gt: "+balance, 1)), 0o644)
	env = filepath.Join(dir, "e.yaml")
	os.WriteFile(env, []byte("vars:\n  base_url: "+srv.URL+"\n  username: alice\nsecrets: [APP_PASSWORD]\n"), 0o644)
	return flow, env, filepath.Join(dir, "out.json")
}

func TestPass(t *testing.T) {
	f, e, out := setup(t, "0")
	if code := run([]string{"run", "-env", e, "-out", out, f}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	b, _ := os.ReadFile(out)
	if strings.Contains(string(b), "s3cret") {
		t.Fatal("secret leaked into evidence")
	}
}

func TestAssertionFailureExit1AndSkips(t *testing.T) {
	f, e, out := setup(t, "1000")
	if code := run([]string{"run", "-env", e, "-out", out, f}); code != 1 {
		t.Fatalf("want exit 1, got %d", code)
	}
}

func TestMissingSecretExit2(t *testing.T) {
	f, e, out := setup(t, "0")
	os.Unsetenv("APP_PASSWORD")
	if code := run([]string{"run", "-env", e, "-out", out, f}); code != 2 {
		t.Fatalf("want exit 2, got %d", code)
	}
}

func TestCycleExit2(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("version: 1\nname: c\nnodes:\n- {id: a, kind: transform, needs: [b], transform: {set: {x: 1}}}\n- {id: b, kind: transform, needs: [a], transform: {set: {x: 1}}}\n"), 0o644)
	if code := run([]string{"validate", p}); code != 2 {
		t.Fatalf("want exit 2, got %d", code)
	}
}

func TestInfraFailureExit3(t *testing.T) {
	srv := httptest.NewServer(nil)
	url := srv.URL
	srv.Close()
	dir := t.TempDir()
	f := filepath.Join(dir, "f.yaml")
	os.WriteFile(f, []byte("version: 1\nname: t\nnodes:\n- {id: a, kind: request, request: {url: \"{{ .env.base }}/\"}}\n"), 0o644)
	e := filepath.Join(dir, "e.yaml")
	os.WriteFile(e, []byte("vars:\n  base: "+url+"\n"), 0o644)
	if code := run([]string{"run", "-env", e, "-out", filepath.Join(dir, "o.json"), f}); code != 3 {
		t.Fatalf("want exit 3, got %d", code)
	}
}

func TestBrokenTemplateExit2(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f.yaml")
	os.WriteFile(f, []byte("version: 1\nname: t\nnodes:\n- {id: a, kind: request, request: {url: \"{{ .env.nope }}/\"}}\n"), 0o644)
	if code := run([]string{"run", "-out", filepath.Join(dir, "o.json"), f}); code != 2 {
		t.Fatalf("want exit 2, got %d", code)
	}
}

func TestServeRefusesNonLoopbackAddress(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", ":0", "192.168.1.9:8787"} {
		if code := run([]string{"serve", "-addr", addr, "-dir", t.TempDir()}); code != 2 {
			t.Fatalf("%s: exit %d, want 2", addr, code)
		}
	}
}

func TestDiffExitCodes(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	a := write("a.json", `{"openapi":"3.1.0","paths":{"/x":{"get":{"responses":{"200":{}}}},"/y":{"get":{"responses":{"200":{}}}}}}`)
	removed := write("removed.json", `{"openapi":"3.1.0","paths":{"/x":{"get":{"responses":{"200":{}}}}}}`)
	added := write("added.json", `{"openapi":"3.1.0","paths":{"/x":{"get":{"responses":{"200":{}}}},"/y":{"get":{"responses":{"200":{}}}},"/z":{"get":{"responses":{"200":{}}}}}}`)
	reviewOld := write("ro.json", `{"openapi":"3.1.0","paths":{"/x":{"get":{"responses":{"200":{"content":{"application/json":{"schema":{"type":"string","format":"date"}}}}}}}}}`)
	reviewNew := write("rn.json", `{"openapi":"3.1.0","paths":{"/x":{"get":{"responses":{"200":{"content":{"application/json":{"schema":{"type":"string","format":"date-time"}}}}}}}}}`)

	for name, tc := range map[string]struct {
		args []string
		want int
	}{
		"identical":                {[]string{"diff", a, a}, 0},
		"compatible addition":      {[]string{"diff", a, added}, 0},
		"removed operation":        {[]string{"diff", a, removed}, 1},
		"removed, fail-on never":   {[]string{"diff", "-fail-on", "never", a, removed}, 0},
		"review passes by default": {[]string{"diff", reviewOld, reviewNew}, 0},
		"review, fail-on review":   {[]string{"diff", "-fail-on", "review", reviewOld, reviewNew}, 1},
		"unreadable old":           {[]string{"diff", filepath.Join(dir, "missing.json"), a}, 2},
		"bad fail-on value":        {[]string{"diff", "-fail-on", "sometimes", a, a}, 2},
		"wrong arg count":          {[]string{"diff", a}, 2},
	} {
		if got := run(tc.args); got != tc.want {
			t.Errorf("%s: exit %d, want %d", name, got, tc.want)
		}
	}
}

// specFixture: a live service + spec, a saved baseline that has since drifted, and an env file tying them together.
func specFixture(t *testing.T, withBaseline bool) (envPath, flowPath, dir string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{"id":1}`)) }))
	t.Cleanup(srv.Close)
	dir = t.TempDir()
	live := `{"openapi":"3.1.0","paths":{"/u":{"get":{"responses":{"200":{"content":{"application/json":{"schema":{"type":"object","properties":{"id":{"type":"integer"}}}}}}}}}}}`
	old := `{"openapi":"3.1.0","paths":{"/u":{"get":{"responses":{"200":{"content":{"application/json":{"schema":{"type":"object","properties":{"id":{"type":"integer"},"name":{"type":"string"}}}}}}}}},"/gone":{"get":{"responses":{"200":{}}}}}}`
	os.WriteFile(filepath.Join(dir, "live.json"), []byte(live), 0o644)
	env := "vars: {base: " + srv.URL + "}\ncontracts: {api: live.json}\n"
	if withBaseline {
		os.WriteFile(filepath.Join(dir, "old.json"), []byte(old), 0o644)
		env += "baselines: {api: old.json}\n"
	}
	envPath = filepath.Join(dir, "e.yaml")
	os.WriteFile(envPath, []byte(env), 0o644)
	flowPath = filepath.Join(dir, "f.yaml")
	os.WriteFile(flowPath, []byte("version: 1\nname: t\nnodes:\n  - {id: a, kind: request, contract: api, request: {url: \"{{ .env.base }}/u\"}}\n"), 0o644)
	return envPath, flowPath, dir
}

func TestRunSpecDriftFlag(t *testing.T) {
	env, flow, dir := specFixture(t, true)
	out := filepath.Join(dir, "ev.json")
	if code := run([]string{"run", "-env", env, "-out", out, flow}); code != 0 {
		t.Fatalf("report-only drift must not fail the run: exit %d", code)
	}
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), `"spec_drift"`) || !strings.Contains(string(b), `"called_breaking": 1`) {
		t.Fatalf("evidence lacks the drift: %s", b)
	}
	if code := run([]string{"run", "-env", env, "-out", out, "-spec-drift-fail", "breaking", flow}); code != 1 {
		t.Fatalf("exit %d, want 1 (the spec moved under the flow)", code)
	}
	if code := run([]string{"run", "-env", env, "-out", out, "-spec-drift-fail", "whenever", flow}); code != 2 {
		t.Fatalf("bad flag value: exit %d, want 2", code)
	}
}

func TestSnapshotSavesRefusesToOverwriteAndRejectsNonSpecs(t *testing.T) {
	env, _, dir := specFixture(t, false)
	if code := run([]string{"snapshot", "-env", env, "-source", "api"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	saved := filepath.Join(dir, "baselines", "api.openapi.json")
	b, err := os.ReadFile(saved)
	if err != nil || !strings.Contains(string(b), `"/u"`) || !strings.Contains(string(b), "\n  ") { // indented, reviewable
		t.Fatalf("%v %s", err, b)
	}
	if code := run([]string{"snapshot", "-env", env, "-source", "api"}); code != 2 {
		t.Fatalf("overwriting a baseline needs -force: exit %d", code)
	}
	if code := run([]string{"snapshot", "-env", env, "-source", "api", "-force"}); code != 0 {
		t.Fatalf("-force: exit %d", code)
	}
	if code := run([]string{"snapshot", "-env", env, "-source", "nope"}); code != 2 {
		t.Fatalf("unknown source: exit %d", code)
	}

	os.WriteFile(filepath.Join(dir, "live.json"), []byte(`{"not":"openapi"}`), 0o644)
	before, _ := os.ReadFile(saved)
	if code := run([]string{"snapshot", "-env", env, "-source", "api", "-force"}); code != 2 {
		t.Fatalf("a non-OpenAPI document must not be saved: exit %d", code)
	}
	if after, _ := os.ReadFile(saved); string(after) != string(before) {
		t.Fatal("a failed snapshot must leave the existing baseline untouched")
	}
}
