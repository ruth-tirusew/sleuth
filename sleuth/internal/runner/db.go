package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"sleuth/internal/flow"
	"sleuth/internal/report"
)

const (
	defaultDBTimeout = 10 * time.Second
	defaultMaxRows   = 1000
	evidenceRows     = 20
)

// dbConns holds one connection pool per named database for the lifetime of a run, so
// consecutive db nodes (and every poll attempt) reuse connections instead of
// reconnecting. A pgx connection is not safe for concurrent use, so Postgres gets a
// pool sized to the run's parallelism; the Mongo client is already concurrency-safe.
type dbConns struct {
	mu       sync.Mutex
	maxConns int32
	pg       map[string]*pgxpool.Pool
	mongo    map[string]*mongo.Client
}

func newDBConns(maxConns int) *dbConns {
	if maxConns < 1 {
		maxConns = 1
	}
	return &dbConns{maxConns: int32(maxConns), pg: map[string]*pgxpool.Pool{}, mongo: map[string]*mongo.Client{}}
}

func (c *dbConns) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, pool := range c.pg {
		pool.Close()
		delete(c.pg, k)
	}
	for k, cl := range c.mongo {
		cl.Disconnect(context.Background())
		delete(c.mongo, k)
	}
}

// pgPool returns the pool for name, or creates one (connections are opened lazily).
// reused reports which.
func (c *dbConns) pgPool(ctx context.Context, name, dsn string) (pool *pgxpool.Pool, reused bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if pool = c.pg[name]; pool != nil {
		return pool, true, nil
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, false, err
	}
	cfg.MaxConns = c.maxConns
	if pool, err = pgxpool.NewWithConfig(ctx, cfg); err != nil {
		return nil, false, err
	}
	c.pg[name] = pool
	return pool, false, nil
}

// execDB runs a read-only query and exposes the rows to assertions and later nodes.
func execDB(ctx *Context, env *EnvConfig, conns *dbConns, n flow.Node, step *report.Step) (map[string]any, error) {
	rows, err := queryDB(ctx, env, conns, n.DB, step)
	if err != nil {
		return nil, err
	}
	result, err := dbResult(rows, n.DB.Extract)
	if err != nil {
		return nil, err
	}
	step.Output, _ = result["out"].(map[string]any)

	ctx.SetNode(n.ID, result) // assertions read nodes.<id>.rows / row_count
	checks, failed, err := evalChecks(ctx, n.Assert)
	step.Checks = checks
	if err != nil {
		return nil, err
	}
	if failed > 0 {
		return nil, assertErr(fmt.Errorf("%d assertion(s) failed on %d row(s)", failed, len(rows)))
	}
	return result, nil
}

// dbResult is the node output: rows, row_count and the extracted values.
func dbResult(rows []any, extract map[string]string) (map[string]any, error) {
	count := float64(len(rows))
	out, err := extractOutputs(map[string]any{"rows": rows, "row_count": count, "status": 0.0}, extract)
	if err != nil {
		return nil, err
	}
	return map[string]any{"rows": rows, "row_count": count, "out": out}, nil
}

// queryDB runs the node's query once and returns its rows. It fills step.DB with the
// evidence (connection name, query, row count, first rows) and classifies failures.
func queryDB(ctx *Context, env *EnvConfig, conns *dbConns, d *flow.DB, step *report.Step) ([]any, error) {
	dsn, err := env.DatabaseDSN(d.Connection)
	if err != nil {
		return nil, specErr(err)
	}
	timeout := defaultDBTimeout
	if d.Timeout != "" {
		if timeout, err = time.ParseDuration(d.Timeout); err != nil {
			return nil, specErr(err)
		}
	}
	maxRows := d.MaxRows
	if maxRows == 0 {
		maxRows = defaultMaxRows
	}
	step.DB = &report.DBResult{Type: d.Kind(), Connection: d.Connection}

	cctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	var rows []any
	switch d.Kind() {
	case flow.DBMongo:
		rows, err = queryMongo(cctx, ctx, conns, dsn, d, maxRows, timeout, step)
	default:
		rows, err = queryPostgres(cctx, ctx, conns, dsn, d, maxRows, step)
	}
	step.DB.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		return nil, err
	}
	if len(rows) > maxRows {
		return nil, specErr(fmt.Errorf("query returned more than max_rows=%d; aggregate, add a limit, or raise db.max_rows", maxRows))
	}
	step.DB.RowCount = len(rows)
	for i, r := range rows {
		if i == evidenceRows {
			step.DB.Truncated = true
			break
		}
		if m, ok := r.(map[string]any); ok {
			step.DB.Rows = append(step.DB.Rows, m)
		}
	}
	return rows, nil
}

// Safety: the query runs in a READ ONLY transaction that is always rolled back, it is
// wrapped as a subquery (so only SELECT/WITH can execute), and values are bound as
// parameters ($1...), never interpolated into the SQL.
func queryPostgres(cctx context.Context, ctx *Context, conns *dbConns, dsn string, d *flow.DB, maxRows int, step *report.Step) ([]any, error) {
	args := make([]any, len(d.Args))
	for i, a := range d.Args {
		var err error
		if args[i], err = ctx.Render(a); err != nil {
			return nil, specErr(fmt.Errorf("db.args[%d]: %w", i, err))
		}
	}
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") && !strings.Contains(dsn, "host=") {
		return nil, specErr(fmt.Errorf("databases.%s is not a Postgres DSN but the node has type postgres", d.Connection))
	}
	query := strings.TrimRight(strings.TrimSpace(d.Query), "; \n\t")
	step.DB.Query, step.DB.Args = query, args

	// Postgres does the type conversion (uuid, timestamptz, numeric, jsonb ...) to JSON.
	wrapped := fmt.Sprintf("SELECT coalesce(json_agg(q), '[]'::json) FROM (SELECT * FROM (%s) inner_q LIMIT %d) q", query, maxRows+1)
	run := func() (raw []byte, reused bool, err error) {
		pool, reused, err := conns.pgPool(cctx, d.Connection, dsn)
		if err != nil {
			return nil, false, err
		}
		conn, err := pool.Acquire(cctx)
		if err != nil {
			return nil, reused, err
		}
		defer conn.Release()
		defer func() {
			if err != nil && !isPgError(err) {
				conn.Conn().Close(context.Background()) // no good any more: the pool discards it on release
			}
		}()
		tx, err := conn.BeginTx(cctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			return nil, reused, err
		}
		defer tx.Rollback(context.Background())
		err = tx.QueryRow(cctx, wrapped, args...).Scan(&raw)
		return raw, reused, err
	}
	raw, reused, err := run()
	if err != nil && !isPgError(err) && reused { // a pooled connection may have gone stale between nodes: retry once
		raw, _, err = run()
	}
	if err != nil {
		return nil, dbErr(err)
	}
	var rows []any
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, specErr(fmt.Errorf("decode result: %w", err))
	}
	return rows, nil
}

// isPgError reports whether the database answered with an error about the query itself.
// Errors that say the connection is gone (class 08, or 57P01-57P03: the server shut down
// or killed the session) arrive as PgErrors too but are connection failures, not query
// failures: the connection must be discarded and the outcome is infrastructure.
func isPgError(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && !connectionLevel(pg)
}

func connectionLevel(pg *pgconn.PgError) bool {
	return strings.HasPrefix(pg.Code, "08") || pg.Code == "57P01" || pg.Code == "57P02" || pg.Code == "57P03"
}

// dbErr classifies a database error. If the database itself answered with an error
// (bad SQL, unknown table/column, permission denied) the query is wrong for this
// database: a spec problem. Anything else (refused, timeout, DNS, auth handshake)
// means we never got a usable answer: infrastructure.
func dbErr(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && !connectionLevel(pg) {
		hint := ""
		if pg.Code == "42601" || pg.Code == "42703" || pg.Code == "42P01" || pg.Code == "25006" {
			hint = " (the query must be a read-only SELECT/WITH; check names and syntax)"
		}
		return specErr(fmt.Errorf("database error %s: %s%s", pg.Code, pg.Message, hint))
	}
	return transportErr(fmt.Errorf("database: %w", err))
}
