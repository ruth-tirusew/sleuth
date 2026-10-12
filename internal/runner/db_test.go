package runner

import (
	"os"
	"strings"
	"testing"

	"sleuth/internal/flow"
	"sleuth/internal/report"
)

func runDB(t *testing.T, dsn, yaml string) *report.Run {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(dir+"/f.yaml", []byte(yaml), 0o644)
	f, err := flow.Load(dir + "/f.yaml")
	if err != nil {
		t.Fatal(err)
	}
	env := &EnvConfig{Databases: map[string]string{"main": dsn}}
	r, _, err := Run(f, env, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

const dbFlow = `
version: 1
name: t
nodes:
  - id: q
    kind: db
    db: {connection: main, query: "SELECT 1 AS one"}
    assert: [{path: nodes.q.rows.0.one, equals: 1}]
`

func TestDBUnreachableIsInfrastructure(t *testing.T) {
	r := runDB(t, "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=2", dbFlow)
	if q := step(r, "q"); q.ErrorKind != KindInfra || strings.Contains(q.Error, "p@") {
		t.Fatalf("kind=%q err=%s", q.ErrorKind, q.Error)
	}
}

func TestDBUnknownConnectionAndUnsetVarAreSpecErrors(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(dir+"/f.yaml", []byte(strings.Replace(dbFlow, "main", "nope", 1)), 0o644)
	f, _ := flow.Load(dir + "/f.yaml")
	r, _, _ := Run(f, &EnvConfig{Databases: map[string]string{"main": "x"}}, Options{})
	if q := step(r, "q"); q.ErrorKind != KindSpec || !strings.Contains(q.Error, "not defined") {
		t.Fatalf("%+v", q)
	}
	os.Unsetenv("SLEUTH_TEST_DSN_UNSET")
	r = runDB(t, "${SLEUTH_TEST_DSN_UNSET}", dbFlow)
	if q := step(r, "q"); q.ErrorKind != KindSpec || !strings.Contains(q.Error, "SLEUTH_TEST_DSN_UNSET") {
		t.Fatalf("%+v", q)
	}
}

func TestDBNodeValidation(t *testing.T) {
	for name, body := range map[string]string{
		"no query":      `db: {connection: main}` + "\n    assert: [{path: a, exists: true}]",
		"no connection": `db: {query: "select 1"}` + "\n    assert: [{path: a, exists: true}]",
		"nothing to do": `db: {connection: main, query: "select 1"}`,
		"bad timeout":   `db: {connection: main, query: "select 1", timeout: soon}` + "\n    assert: [{path: a, exists: true}]",
	} {
		dir := t.TempDir()
		os.WriteFile(dir+"/f.yaml", []byte("version: 1\nname: t\nnodes:\n  - id: q\n    kind: db\n    "+body+"\n"), 0o644)
		if _, err := flow.Load(dir + "/f.yaml"); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

// Against a real Postgres: SLEUTH_TEST_PG_DSN=postgres://... go test ./internal/runner -run LivePG
func TestLivePGReadOnlyAndTypes(t *testing.T) {
	dsn := os.Getenv("SLEUTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("SLEUTH_TEST_PG_DSN not set")
	}
	r := runDB(t, dsn, `
version: 1
name: t
nodes:
  - id: types
    kind: db
    db:
      connection: main
      query: "SELECT $1::text AS s, $2::int AS i, now() AS ts, gen_random_uuid() AS id, 1.5::numeric AS n, '{\"a\":[1]}'::jsonb AS j, NULL::text AS nothing"
      args: ["hello", 7]
      extract: {s: rows.0.s}
    assert:
      - {path: nodes.types.rows.0.s, equals: hello}
      - {path: nodes.types.rows.0.i, equals: 7}
      - {path: nodes.types.rows.0.n, equals: 1.5}
      - {path: nodes.types.rows.0.j.a.0, equals: 1}
      - {path: nodes.types.rows.0.id, matches: '^[0-9a-f-]{36}$'}
      - {path: nodes.types.out.s, equals: hello}
  - id: empty
    kind: db
    db: {connection: main, query: "SELECT 1 WHERE false"}
    assert: [{path: nodes.empty.row_count, equals: 0}]
  - id: too_many
    kind: db
    db: {connection: main, query: "SELECT generate_series(1, 5)", max_rows: 3}
    assert: [{path: nodes.too_many.row_count, equals: 3}]
  - id: write
    kind: db
    db: {connection: main, query: "SELECT 1; CREATE TABLE sleuth_should_not_exist(x int)"}
    assert: [{path: nodes.write.row_count, equals: 1}]
  - id: still_no_table
    kind: db
    needs: []
    db: {connection: main, query: "SELECT count(*) AS n FROM information_schema.tables WHERE table_name = 'sleuth_should_not_exist'"}
    assert: [{path: nodes.still_no_table.rows.0.n, equals: 0}]
`)
	if s := step(r, "types"); s.Status != report.Passed {
		t.Fatalf("types: %+v %+v", s, s.Checks)
	}
	if s := step(r, "empty"); s.Status != report.Passed {
		t.Fatalf("empty: %+v", s)
	}
	if s := step(r, "too_many"); s.ErrorKind != KindSpec || !strings.Contains(s.Error, "max_rows") {
		t.Fatalf("too_many: %+v", s)
	}
	if s := step(r, "write"); s.ErrorKind != KindSpec {
		t.Fatalf("a write smuggled behind SELECT must not run: %+v", s)
	}
	if s := step(r, "still_no_table"); s.Status != report.Passed {
		t.Fatalf("table was created! %+v", s)
	}
}

func TestDBAndPollValidationForMongoAndDBPolling(t *testing.T) {
	bad := map[string]string{
		"mongo needs collection":  "kind: db\n    db: {type: mongo, connection: m, database: d}\n    assert: [{path: a, exists: true}]",
		"mongo needs database":    "kind: db\n    db: {type: mongo, connection: m, collection: c}\n    assert: [{path: a, exists: true}]",
		"mongo rejects query":     "kind: db\n    db: {type: mongo, connection: m, database: d, collection: c, query: select 1}\n    assert: [{path: a, exists: true}]",
		"pipeline plus filter":    "kind: db\n    db: {type: mongo, connection: m, database: d, collection: c, pipeline: [], filter: {a: 1}}\n    assert: [{path: a, exists: true}]",
		"sort needs one key":      "kind: db\n    db: {type: mongo, connection: m, database: d, collection: c, sort: [{a: 1, b: 1}]}\n    assert: [{path: a, exists: true}]",
		"postgres rejects filter": "kind: db\n    db: {connection: m, query: select 1, filter: {a: 1}}\n    assert: [{path: a, exists: true}]",
		"unknown type":            "kind: db\n    db: {type: mysql, connection: m, query: select 1}\n    assert: [{path: a, exists: true}]",
		"poll needs a target":     "kind: poll\n    poll: {until: [{path: a, exists: true}]}",
		"poll rejects both":       "kind: poll\n    poll: {request: {url: http://x}, db: {connection: m, query: select 1}, until: [{path: a, exists: true}]}",
		"poll db invalid":         "kind: poll\n    poll: {db: {connection: m}, until: [{path: a, exists: true}]}",
	}
	for name, body := range bad {
		p := t.TempDir() + "/f.yaml"
		os.WriteFile(p, []byte("version: 1\nname: t\nnodes:\n  - id: q\n    "+body+"\n"), 0o644)
		if _, err := flow.Load(p); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
	good := map[string]string{
		"mongo find":      "kind: db\n    db: {type: mongo, connection: m, database: d, collection: c, filter: {a: 1}, sort: [{a: -1}]}\n    assert: [{path: a, exists: true}]",
		"mongo agg":       "kind: db\n    db: {type: mongo, connection: m, database: d, collection: c, pipeline: [{$count: n}]}\n    assert: [{path: a, exists: true}]",
		"poll over db":    "kind: poll\n    poll: {db: {connection: m, query: select 1}, until: [{path: a, exists: true}]}",
		"poll over mongo": "kind: poll\n    poll: {db: {type: mongo, connection: m, database: d, collection: c}, until: [{path: a, exists: true}]}",
	}
	for name, body := range good {
		p := t.TempDir() + "/f.yaml"
		os.WriteFile(p, []byte("version: 1\nname: t\nnodes:\n  - id: q\n    "+body+"\n"), 0o644)
		if _, err := flow.Load(p); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestMongoWriteStagesAreFoundAnywhere(t *testing.T) {
	for stage, pipeline := range map[string]any{
		"$out":    []any{map[string]any{"$match": map[string]any{}}, map[string]any{"$out": "x"}},
		"$merge":  []any{map[string]any{"$merge": map[string]any{"into": "x"}}},
		"$out ":   []any{map[string]any{"$facet": map[string]any{"a": []any{map[string]any{"$out": "x"}}}}},
		"$merge ": []any{map[string]any{"$lookup": map[string]any{"pipeline": []any{map[string]any{"$merge": "x"}}}}},
	} {
		if writeStage(pipeline) == "" {
			t.Errorf("%s not detected in %v", stage, pipeline)
		}
	}
	if writeStage([]any{map[string]any{"$match": map[string]any{"a": 1}}, map[string]any{"$group": map[string]any{"_id": "$a"}}}) != "" {
		t.Error("false positive")
	}
}
