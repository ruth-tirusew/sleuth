package runner

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"sleuth/internal/flow"
	"sleuth/internal/report"
)

func runDBs(t *testing.T, dbs map[string]string, yaml string) *report.Run {
	t.Helper()
	p := t.TempDir() + "/f.yaml"
	os.WriteFile(p, []byte(yaml), 0o644)
	f, err := flow.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := Run(f, &EnvConfig{Databases: dbs}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Connection reuse + polling a query, against a scratch Postgres database:
// SLEUTH_TEST_PG_DSN=postgres://.../scratch go test ./internal/runner -run LivePGPoll
func TestLivePGPollAndConnectionReuse(t *testing.T) {
	dsn := os.Getenv("SLEUTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("SLEUTH_TEST_PG_DSN not set")
	}
	bg := context.Background()
	admin, err := pgx.Connect(bg, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(bg)
	admin.Exec(bg, "DROP TABLE IF EXISTS sleuth_poll_probe")
	if _, err := admin.Exec(bg, "CREATE TABLE sleuth_poll_probe(id int, state text)"); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(bg, "DROP TABLE IF EXISTS sleuth_poll_probe")
	inserted := make(chan struct{})
	go func() { // the "async work": a row appears a moment after the poll starts
		defer close(inserted)
		time.Sleep(450 * time.Millisecond)
		admin.Exec(bg, "INSERT INTO sleuth_poll_probe VALUES (7, 'SETTLED')")
	}()
	defer func() { <-inserted }() // the connection is not safe to share with the deferred cleanup above

	r := runDBs(t, map[string]string{"main": dsn}, `
version: 1
name: t
nodes:
  - id: pid1
    kind: db
    db: {connection: main, query: "SELECT pg_backend_pid() AS pid", extract: {pid: rows.0.pid}}
    assert: [{path: nodes.pid1.row_count, equals: 1}]
  - id: settled
    kind: poll
    needs: [pid1]
    poll:
      interval: 100ms
      timeout: 10s
      db:
        connection: main
        query: "SELECT id, state FROM sleuth_poll_probe WHERE state = $1"
        args: [SETTLED]
        extract: {id: rows.0.id}
      until:
        - {path: nodes.settled.row_count, equals: 1}
  - id: pid2
    kind: db
    needs: [settled]
    db: {connection: main, query: "SELECT pg_backend_pid() AS pid"}
    assert:
      - {path: nodes.pid2.rows.0.pid, equals: "{{ .nodes.pid1.out.pid }}", message: same connection reused across nodes}
      - {path: nodes.settled.out.id, equals: 7}
`)
	s := step(r, "settled")
	if s.Status != report.Passed || s.Poll == nil || s.Poll.Attempts < 2 || s.Poll.History[0].Passed || s.Poll.History[0].RowCount != 0 {
		t.Fatalf("poll: %+v %+v", s, s.Poll)
	}
	if p := step(r, "pid2"); p.Status != report.Passed {
		t.Fatalf("connection not reused: %+v %+v", p, p.Checks)
	}
}

func TestLivePGPollTimeoutAndSpecError(t *testing.T) {
	dsn := os.Getenv("SLEUTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("SLEUTH_TEST_PG_DSN not set")
	}
	r := runDBs(t, map[string]string{"main": dsn}, `
version: 1
name: t
nodes:
  - id: never
    kind: poll
    poll:
      interval: 100ms
      timeout: 400ms
      db: {connection: main, query: "SELECT 1 WHERE false"}
      until: [{path: nodes.never.row_count, gt: 0}]
  - id: broken
    kind: poll
    poll:
      interval: 100ms
      timeout: 5s
      db: {connection: main, query: "SELECT * FROM no_such_table"}
      until: [{path: nodes.broken.row_count, gt: 0}]
`)
	if s := step(r, "never"); s.ErrorKind != KindTimeout || s.Poll.Attempts < 2 {
		t.Fatalf("%+v %+v", s, s.Poll)
	}
	if s := step(r, "broken"); s.ErrorKind != KindSpec || s.Poll.Attempts != 1 {
		t.Fatalf("bad SQL must fail fast: %+v %+v", s, s.Poll)
	}
}

// Mongo, against a scratch database that the test creates and drops:
// SLEUTH_TEST_MONGO_DSN=mongodb://127.0.0.1:27017/?replicaSet=rs0 go test ./internal/runner -run LiveMongo
func TestLiveMongo(t *testing.T) {
	dsn := os.Getenv("SLEUTH_TEST_MONGO_DSN")
	if dsn == "" {
		t.Skip("SLEUTH_TEST_MONGO_DSN not set")
	}
	bg := context.Background()
	cl, err := mongo.Connect(options.Client().ApplyURI(dsn))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Disconnect(bg)
	const dbName = "sleuth_scratch"
	db := cl.Database(dbName)
	defer db.Drop(bg)
	id1 := bson.NewObjectID()
	_, err = db.Collection("loans").InsertMany(bg, []any{
		bson.M{"_id": id1, "ref": "L-1", "status": "ACTIVE", "amount": int64(100), "created": time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "meta": bson.M{"tags": bson.A{"a", "b"}}},
		bson.M{"ref": "L-2", "status": "ACTIVE", "amount": int64(250)},
		bson.M{"ref": "L-3", "status": "CLOSED", "amount": int64(40)},
	})
	if err != nil {
		t.Fatal(err)
	}
	price, _ := bson.ParseDecimal128("10.50")
	if _, err := db.Collection("precise").InsertOne(bg, bson.M{"ref": "P-1", "big": int64(9007199254740993), "price": price}); err != nil {
		t.Fatal(err)
	}
	go func() { // async work for the poll
		time.Sleep(450 * time.Millisecond)
		db.Collection("settle").InsertOne(bg, bson.M{"loan": "L-1", "state": "SETTLED"})
	}()

	r := runDBs(t, map[string]string{"m": dsn, "down": "mongodb://127.0.0.1:1/?serverSelectionTimeoutMS=300", "pg": "postgres://x@h/db"}, fmt.Sprintf(`
version: 1
name: t
nodes:
  - id: active
    kind: db
    db:
      type: mongo
      connection: m
      database: sleuth_scratch
      collection: loans
      filter: {status: ACTIVE}
      sort: [{amount: -1}]
      projection: {ref: 1, amount: 1, _id: 0}
      extract: {top: rows.0.ref}
    assert:
      - {path: nodes.active.row_count, equals: 2}
      - {path: nodes.active.rows.0.ref, equals: L-2}
      - {path: nodes.active.rows.0.amount, equals: 250}
      - {path: nodes.active.out.top, equals: L-2}
  - id: by_id
    kind: db
    db:
      type: mongo
      connection: m
      database: sleuth_scratch
      collection: loans
      filter: {_id: {$oid: "%s"}}
    assert:
      - {path: nodes.by_id.rows.0._id, equals: "%s", message: ObjectId becomes a plain string}
      - {path: nodes.by_id.rows.0.created, matches: '^2026-01-02T03:04:05'}
      - {path: nodes.by_id.rows.0.meta.tags.1, equals: b}
  - id: totals
    kind: db
    db:
      type: mongo
      connection: m
      database: sleuth_scratch
      collection: loans
      pipeline:
        - {$group: {_id: "$status", n: {$sum: 1}, total: {$sum: "$amount"}}}
        - {$sort: {_id: 1}}
    assert:
      - {path: nodes.totals.row_count, equals: 2}
      - {path: nodes.totals.rows.0.n, equals: 2}
      - {path: nodes.totals.rows.0.total, equals: 350}
  - id: templated
    kind: db
    needs: [active]
    db:
      type: mongo
      connection: m
      database: sleuth_scratch
      collection: loans
      filter: {ref: "{{ .nodes.active.out.top }}"}
    assert: [{path: nodes.templated.rows.0.amount, equals: 250}]
  - id: settled
    kind: poll
    poll:
      interval: 100ms
      timeout: 10s
      db: {type: mongo, connection: m, database: sleuth_scratch, collection: settle, filter: {state: SETTLED}}
      until: [{path: nodes.settled.row_count, equals: 1}]
  - id: writes
    kind: db
    db:
      type: mongo
      connection: m
      database: sleuth_scratch
      collection: loans
      pipeline: [{$match: {}}, {$out: loans_copy}]
    assert: [{path: nodes.writes.row_count, gt: 0}]
  - id: bad_op
    kind: db
    db: {type: mongo, connection: m, database: sleuth_scratch, collection: loans, filter: {amount: {$bogus: 1}}}
    assert: [{path: nodes.bad_op.row_count, gt: 0}]
  - id: precise
    kind: db
    db: {type: mongo, connection: m, database: sleuth_scratch, collection: precise, extract: {big: rows.0.big}}
    assert:
      - {path: nodes.precise.rows.0.big, equals: 9007199254740993, message: a NumberLong beyond 2^53 must not be rounded}
      - {path: nodes.precise.rows.0.price, equals: 10.5, message: Decimal128 10.50 equals 10.5}
  - id: precise_off_by_one
    kind: db
    db: {type: mongo, connection: m, database: sleuth_scratch, collection: precise}
    assert: [{path: nodes.precise_off_by_one.rows.0.big, equals: 9007199254740992}]
  - id: precise_roundtrip
    kind: db
    needs: [precise]
    db:
      type: mongo
      connection: m
      database: sleuth_scratch
      collection: precise
      filter: {big: "{{ .nodes.precise.out.big }}"}
    assert: [{path: nodes.precise_roundtrip.row_count, equals: 1, message: the extracted long is sent back unchanged}]
  - id: unreachable
    kind: db
    db: {type: mongo, connection: down, database: d, collection: c, timeout: 2s}
    assert: [{path: nodes.unreachable.row_count, gt: 0}]
  - id: wrong_type
    kind: db
    db: {type: mongo, connection: pg, database: d, collection: c}
    assert: [{path: nodes.wrong_type.row_count, gt: 0}]
  - id: too_many
    kind: db
    db: {type: mongo, connection: m, database: sleuth_scratch, collection: loans, max_rows: 2}
    assert: [{path: nodes.too_many.row_count, gt: 0}]
`, id1.Hex(), id1.Hex()))
	for _, id := range []string{"active", "by_id", "totals", "templated", "settled", "precise", "precise_roundtrip"} {
		if s := step(r, id); s.Status != report.Passed {
			t.Errorf("%s: %+v checks=%+v", id, s, s.Checks)
		}
	}
	if s := step(r, "settled"); s.Poll == nil || s.Poll.Attempts < 2 {
		t.Errorf("poll should need several attempts: %+v", s.Poll)
	}
	want := map[string]string{"writes": KindSpec, "bad_op": KindSpec, "unreachable": KindInfra, "wrong_type": KindSpec, "too_many": KindSpec}
	for id, k := range want {
		if got := step(r, id).ErrorKind; got != k {
			t.Errorf("%s: kind %q want %q (%s)", id, got, k, step(r, id).Error)
		}
	}
	if step(r, "precise_off_by_one").Status != report.Failed {
		t.Error("9007199254740992 must not equal the stored 9007199254740993")
	}
	if e := step(r, "bad_op").Error; !regexp.MustCompile(`^mongo error \d+ \(\w+\)`).MatchString(e) {
		t.Errorf("the server's error code and name should be reported, got %q", e)
	}
	if !strings.Contains(step(r, "writes").Error, "$out") {
		t.Errorf("writes: %s", step(r, "writes").Error)
	}
	if n, _ := db.Collection("loans_copy").CountDocuments(bg, bson.M{}); n != 0 {
		t.Fatalf("$out ran! %d docs written", n)
	}
	if ev := step(r, "active").DB; ev == nil || ev.Type != "mongo" || ev.RowCount != 2 || !strings.HasPrefix(ev.Query, "find sleuth_scratch.loans") {
		t.Errorf("evidence: %+v", ev)
	}
}

// Parallel db nodes need their own pooled connections: a single pgx connection is not
// safe for concurrent use. Four 300ms queries on one named database must overlap.
// SLEUTH_TEST_PG_DSN=postgres://.../scratch go test -race ./internal/runner -run LivePGParallel
func TestLivePGParallelNodesUsePooledConnections(t *testing.T) {
	dsn := os.Getenv("SLEUTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("SLEUTH_TEST_PG_DSN not set")
	}
	var b strings.Builder
	b.WriteString("version: 1\nname: t\nnodes:\n")
	for i := 0; i < 4; i++ {
		fmt.Fprintf(&b, "  - id: q%d\n    kind: db\n    db: {connection: main, query: \"SELECT %d AS n, pg_sleep(0.3)\"}\n    assert: [{path: nodes.q%d.row_count, equals: 1}]\n", i, i, i)
	}
	p := t.TempDir() + "/f.yaml"
	os.WriteFile(p, []byte(b.String()), 0o644)
	f, err := flow.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	r, _, err := Run(f, &EnvConfig{Databases: map[string]string{"main": dsn}}, Options{Parallel: 4})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != report.Passed || time.Since(start) > 900*time.Millisecond {
		t.Fatalf("%s in %v: %+v", r.Status, time.Since(start), r.Steps)
	}
}

// A pooled connection that died between nodes (server restart, idle timeout) must be
// discarded and the query retried once on a fresh one, not reported as an outage.
func TestLivePGPoolRecoversFromKilledConnections(t *testing.T) {
	dsn := os.Getenv("SLEUTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("SLEUTH_TEST_PG_DSN not set")
	}
	bg := context.Background()
	admin, err := pgx.Connect(bg, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(bg)

	env := &EnvConfig{Databases: map[string]string{"main": dsn + "&application_name=sleuth_kill_probe"}}
	ctx, _, err := NewContext(env)
	if err != nil {
		t.Fatal(err)
	}
	conns := newDBConns(2)
	defer conns.close()
	q := &flow.DB{Connection: "main", Query: "SELECT 1 AS n"}
	if _, err := queryDB(ctx, env, conns, q, &report.Step{}); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(bg, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = 'sleuth_kill_probe'"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	rows, err := queryDB(ctx, env, conns, q, &report.Step{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("expected recovery on a fresh connection: rows=%v err=%v", rows, err)
	}
}

// bigint and numeric must come back exact and travel into the next query unchanged.
// SLEUTH_TEST_PG_DSN=postgres://.../scratch go test ./internal/runner -run LivePGExact
func TestLivePGExactBigintAndNumeric(t *testing.T) {
	dsn := os.Getenv("SLEUTH_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("SLEUTH_TEST_PG_DSN not set")
	}
	r := runDBs(t, map[string]string{"main": dsn}, `
version: 1
name: t
nodes:
  - id: a
    kind: db
    db:
      connection: main
      query: "SELECT 9007199254740993::bigint AS big, 12345678901234567890.123456789::numeric AS dec"
      extract: {big: rows.0.big}
    assert:
      - {path: nodes.a.rows.0.big, equals: 9007199254740993}
      - {path: nodes.a.rows.0.dec, equals: 12345678901234567890.123456789}
  - id: off_decimal
    kind: db
    db: {connection: main, query: "SELECT 12345678901234567890.123456788::numeric AS dec"}
    assert: [{path: nodes.off_decimal.rows.0.dec, equals: 12345678901234567890.123456789, message: a decimal off in the last digit must not pass}]
  - id: off
    kind: db
    db: {connection: main, query: "SELECT 9007199254740993::bigint AS big"}
    assert: [{path: nodes.off.rows.0.big, equals: 9007199254740992}]
  - id: b
    kind: db
    needs: [a]
    db: {connection: main, query: "SELECT $1::bigint = 9007199254740993 AS same", args: ["{{ .nodes.a.out.big }}"]}
    assert: [{path: nodes.b.rows.0.same, equals: true}]
`)
	if s := step(r, "a"); s.Status != report.Passed {
		t.Fatalf("a: %+v %+v", s, s.Checks)
	}
	if step(r, "off_decimal").Status != report.Failed {
		t.Fatal("a decimal that differs in its last digit must not compare equal")
	}
	if step(r, "off").Status != report.Failed {
		t.Fatal("an off-by-one bigint must not compare equal")
	}
	if s := step(r, "b"); s.Status != report.Passed {
		t.Fatalf("the extracted bigint was altered on the way back into a query: %+v %+v", s, s.Checks)
	}
}
