package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sleuth/internal/mockservice"
)

func TestValidate(t *testing.T) {
	dir := workdir(t)
	run := func(want int, args ...string) *result {
		t.Helper()
		r := invoke(t, dir, nil, args...)
		expectExit(t, r, want)
		return r
	}
	if r := run(0, "validate", "happy.flow.yaml"); !strings.Contains(r.stdout, "OK: happy") || !strings.Contains(r.stdout, "login") {
		t.Errorf("validate should print the execution order:\n%s", r.stdout)
	}
	if r := run(0, "validate", "-parallel", "4", "parallel.flow.yaml"); !strings.Contains(r.stdout, "wave 1: s1, s2, s3, s4") {
		t.Errorf("the four independent requests should share a wave:\n%s", r.stdout)
	}
	if r := run(2, "validate", "cycle.flow.yaml"); !strings.Contains(r.stderr, "cycle") {
		t.Errorf("%s", r.stderr)
	}
	if r := run(2, "validate", "unknown-dep.flow.yaml"); !strings.Contains(r.stderr, "ghost") {
		t.Errorf("the error should name the unknown node:\n%s", r.stderr)
	}
	run(2, "validate", "no-such-file.flow.yaml")
	run(2)                                             // no command at all
	run(2, "run", "-parallel", "0", "happy.flow.yaml") // nonsense flag value
	run(2, "run", "-spec-drift-fail", "sometimes", "happy.flow.yaml")
	run(2, "frobnicate", "happy.flow.yaml")
}

func TestScaffold(t *testing.T) {
	dir := workdir(t)
	env := []string{"BASE_URL=" + startService(t)}
	list := invoke(t, dir, env, "scaffold", "-source", "api", "-env", "env.yaml", "-list")
	expectExit(t, list, 0)
	for _, op := range []string{"GET /health", "POST /login", "GET /users/{id}", "GET /legacy/{id}"} {
		if !strings.Contains(list.stdout, op) {
			t.Errorf("-list should include %q:\n%s", op, list.stdout)
		}
	}

	node := invoke(t, dir, env, "scaffold", "-source", "api", "-env", "env.yaml", "GET /users/{id}")
	expectExit(t, node, 0)
	for _, want := range []string{"contract: api", "{{ .env.id }}", "optional (not emitted)", "equals: 200"} {
		if !strings.Contains(node.stdout, want) {
			t.Errorf("scaffolded node should contain %q:\n%s", want, node.stdout)
		}
	}
	// what scaffold prints is a valid flow fragment
	flowFile := filepath.Join(dir, "scaffolded.flow.yaml")
	os.WriteFile(flowFile, []byte("version: 1\nname: scaffolded\nnodes:\n"+node.stdout), 0o644)
	expectExit(t, invoke(t, dir, nil, "validate", flowFile), 0)

	// a request body skeleton is built from the required properties
	login := invoke(t, dir, env, "scaffold", "-source", "api", "-env", "env.yaml", "POST /login")
	expectExit(t, login, 0)
	if !strings.Contains(login.stdout, "username: string") || !strings.Contains(login.stdout, "password: string") {
		t.Errorf("a body skeleton should list the required fields:\n%s", login.stdout)
	}

	expectExit(t, invoke(t, dir, env, "scaffold", "-source", "api", "-env", "env.yaml", "GET /nope"), 2)
	expectExit(t, invoke(t, dir, env, "scaffold", "-source", "missing", "-env", "env.yaml", "-list"), 2)
}

func TestDiff(t *testing.T) {
	dir := t.TempDir()
	v1, v2 := filepath.Join(dir, "v1.json"), filepath.Join(dir, "v2.json")
	os.WriteFile(v1, mockservice.Spec(1), 0o644)
	os.WriteFile(v2, mockservice.Spec(2), 0o644)

	expectExit(t, invoke(t, dir, nil, "diff", v1, v1), 0)
	r := invoke(t, dir, nil, "diff", v1, v2)
	expectExit(t, r, 1)
	for _, want := range []string{"breaking", "GET /legacy/{id}", "currency"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("the diff should mention %q:\n%s", want, r.stdout)
		}
	}
	expectExit(t, invoke(t, dir, nil, "diff", "-fail-on", "never", v1, v2), 0)

	j := invoke(t, dir, nil, "diff", "-json", "-fail-on", "never", v1, v2)
	expectExit(t, j, 0)
	if !json.Valid([]byte(j.stdout)) {
		t.Errorf("-json must print JSON:\n%s", j.stdout)
	}
	// the reverse direction only adds the removed operation back, which is compatible
	if back := invoke(t, dir, nil, "diff", "-fail-on", "never", v2, v1); back.exit != 0 {
		t.Errorf("exit %d", back.exit)
	}
	expectExit(t, invoke(t, dir, nil, "diff", filepath.Join(dir, "missing.json"), v1), 2)
	expectExit(t, invoke(t, dir, nil, "diff", v1), 2)
}

func TestSnapshotThenRunDetectsDrift(t *testing.T) {
	dir := workdir(t)
	env := []string{"BASE_URL=" + startService(t), "APP_PASSWORD=" + password}

	// snapshot the live (v1) contract as the baseline for a source
	r := invoke(t, dir, env, "snapshot", "-env", "env.yaml", "-source", "api")
	expectExit(t, r, 0)
	baseline := filepath.Join(dir, "baselines", "api.openapi.json")
	b, err := os.ReadFile(baseline)
	if err != nil {
		t.Fatalf("snapshot should write %s: %v\n%s", baseline, err, r.stdout)
	}
	var doc map[string]any
	if json.Unmarshal(b, &doc) != nil || doc["paths"] == nil {
		t.Fatalf("the baseline should be an OpenAPI document: %s", b)
	}
	// it refuses to overwrite what drift is measured against, unless told to
	expectExit(t, invoke(t, dir, env, "snapshot", "-env", "env.yaml", "-source", "api"), 2)
	expectExit(t, invoke(t, dir, env, "snapshot", "-env", "env.yaml", "-source", "api", "-force"), 0)

	// an env that points the contract at the later version, with that baseline, shows the drift
	envFile := filepath.Join(dir, "env-snap.yaml")
	os.WriteFile(envFile, []byte("vars:\n  base_url: ${BASE_URL}\ncontracts:\n  api: ${BASE_URL}/openapi.v2.json\nbaselines:\n  api: baselines/api.openapi.json\n"), 0o644)
	out := filepath.Join(dir, "e.json")
	run := invoke(t, dir, env, "run", "-env", envFile, "-out", out, "-spec-drift-fail", "breaking", "contract-health.flow.yaml")
	expectExit(t, run, 1)
	if !strings.Contains(run.stdout, "Spec drift") {
		t.Errorf("%s", run.stdout)
	}
}

// The database nodes need a real Postgres. Point SLEUTH_TEST_PG_DSN at a scratch database:
//
//	SLEUTH_TEST_PG_DSN=postgres://user:pw@localhost:5432/scratch?sslmode=disable go test ./e2e
func TestDatabaseNodes(t *testing.T) {
	dsn := os.Getenv("SLEUTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("SLEUTH_TEST_PG_DSN not set")
	}
	cases := []struct {
		name, flow, dsn string
		exit            int
		kind            string
	}{
		{"a query passes", "db-pg", dsn, 0, ""},
		{"the database rejecting the SQL is a spec error", "db-pg-bad", dsn, 2, "spec"},
		{"a write smuggled behind a SELECT never runs", "db-pg-write", dsn, 2, "spec"},
		{"an unreachable database is infrastructure", "db-pg", "postgres://x:y@127.0.0.1:1/z?sslmode=disable&connect_timeout=2", 3, "infrastructure"},
	}
	for _, c := range cases {
		for _, p := range []int{1, 4} {
			c, p := c, p
			t.Run(fmt.Sprintf("%s/parallel=%d", c.name, p), func(t *testing.T) {
				r := runFlow(t, c.flow, p, runOptions{env: "env-pg.yaml", extraEnv: []string{"PG_DSN=" + c.dsn}})
				expectExit(t, r, c.exit)
				id := map[string]string{"db-pg": "one", "db-pg-bad": "bad", "db-pg-write": "w"}[c.flow]
				if c.kind != "" {
					expectSteps(t, r, map[string]string{id: c.kind})
				}
				if strings.Contains(r.rawEvidence(t), "127.0.0.1:1/z") || strings.Contains(r.rawEvidence(t), "://") && strings.Contains(r.rawEvidence(t), dsn) {
					t.Error("the connection string must never appear in the evidence")
				}
			})
		}
	}
}
