package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"sleuth/internal/flow"
	"sleuth/internal/report"
)

// mongoClient returns the cached client for name, or creates one. The client carries no
// timeout of its own beyond what the DSN says: every operation runs under the calling
// node's context deadline, so a client first created by a starved poll attempt is not left
// with that attempt's few milliseconds for the rest of the run.
func (c *dbConns) mongoClient(name, dsn string) (cl *mongo.Client, reused bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl = c.mongo[name]; cl != nil {
		return cl, true, nil
	}
	cl, err = mongo.Connect(options.Client().ApplyURI(dsn))
	if err != nil {
		return nil, false, err
	}
	c.mongo[name] = cl
	return cl, false, nil
}

// dropMongo stops handing out bad, the client that failed to reach the server, so the next
// use reconnects. It only acts if bad is still the cached client: with parallel nodes another
// node may already have replaced it, and that healthy one must be left alone. bad is retired
// rather than disconnected, because other nodes may still be mid-query on it; it is closed
// when the run ends.
func (c *dbConns) dropMongo(name string, bad *mongo.Client) {
	if bad == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mongo[name] == bad {
		delete(c.mongo, name)
		c.retired = append(c.retired, bad)
	}
}

// queryMongo runs a find (filter/sort/projection) or an aggregation (pipeline). Only
// reads are issued; pipelines containing $out or $merge are rejected because those
// stages write.
func queryMongo(cctx context.Context, ctx *Context, conns *dbConns, dsn string, d *flow.DB, maxRows int, step *report.Step) ([]any, error) {
	if !strings.HasPrefix(dsn, "mongodb://") && !strings.HasPrefix(dsn, "mongodb+srv://") {
		return nil, specErr(fmt.Errorf("databases.%s is not a MongoDB URI but the node has type mongo", d.Connection))
	}
	var filter bson.D
	var pipeline []bson.D
	if d.Pipeline != nil {
		rendered, err := ctx.Render(d.Pipeline)
		if err != nil {
			return nil, specErr(fmt.Errorf("db.pipeline: %w", err))
		}
		if stage := writeStage(rendered); stage != "" {
			return nil, specErr(fmt.Errorf("db.pipeline contains %s, which writes; db nodes are read-only", stage))
		}
		var wrap struct{ P []bson.D }
		if err := extJSON(map[string]any{"P": rendered}, &wrap); err != nil {
			return nil, specErr(fmt.Errorf("db.pipeline: %w", err))
		}
		pipeline = append(wrap.P, bson.D{{Key: "$limit", Value: int64(maxRows + 1)}})
		step.DB.Query = fmt.Sprintf("aggregate %s.%s %s", d.Database, d.Collection, compact(rendered))
	} else {
		rf := map[string]any{}
		if d.Filter != nil {
			r, err := ctx.Render(d.Filter)
			if err != nil {
				return nil, specErr(fmt.Errorf("db.filter: %w", err))
			}
			rf, _ = r.(map[string]any)
		}
		if err := extJSON(rf, &filter); err != nil {
			return nil, specErr(fmt.Errorf("db.filter: %w", err))
		}
		step.DB.Query = fmt.Sprintf("find %s.%s %s", d.Database, d.Collection, compact(rf))
	}

	// run returns the rows, the client it used, and whether that client was a cached one.
	run := func() ([]any, *mongo.Client, bool, error) {
		cl, reused, err := conns.mongoClient(d.Connection, dsn)
		if err != nil {
			return nil, nil, false, err
		}
		coll := cl.Database(d.Database).Collection(d.Collection)
		var cur *mongo.Cursor
		if pipeline != nil {
			cur, err = coll.Aggregate(cctx, pipeline)
		} else {
			opts := options.Find().SetLimit(int64(maxRows + 1))
			if len(d.Sort) > 0 {
				var sort bson.D
				for _, s := range d.Sort {
					for k, v := range s {
						sort = append(sort, bson.E{Key: k, Value: v})
					}
				}
				opts.SetSort(sort)
			}
			if d.Projection != nil {
				var proj bson.D
				if err := extJSON(d.Projection, &proj); err != nil {
					return nil, cl, reused, specErr(fmt.Errorf("db.projection: %w", err))
				}
				opts.SetProjection(proj)
			}
			cur, err = coll.Find(cctx, filter, opts)
		}
		if err != nil {
			return nil, cl, reused, err
		}
		defer cur.Close(context.Background())
		var rows []any
		for cur.Next(cctx) {
			b, err := bson.MarshalExtJSON(cur.Current, false, false)
			if err != nil {
				return nil, cl, reused, specErr(err)
			}
			doc, err := decodeJSON(b) // numbers stay exact (json.Number)
			if err != nil {
				return nil, cl, reused, specErr(err)
			}
			rows = append(rows, plainExtJSON(doc))
		}
		return rows, cl, reused, cur.Err()
	}
	rows, cl, reused, err := run()
	if err != nil && mongoTransport(err) {
		conns.dropMongo(d.Connection, cl) // the client is no good any more: next use reconnects
		// A cached client may simply have gone stale between nodes: retry once with a fresh
		// one, unless this node's own deadline is already spent (a retry could not succeed).
		if reused && cctx.Err() == nil {
			var fresh *mongo.Client
			rows, fresh, _, err = run()
			if err != nil && mongoTransport(err) {
				conns.dropMongo(d.Connection, fresh)
			}
		}
	}
	if err != nil {
		return nil, mongoErr(err)
	}
	return rows, nil
}

// mongoTransport reports whether err means the client could not reach the server, so it
// is worth discarding. It is not: the server rejecting the operation, the flow being wrong,
// or this node's own deadline passing (a slow query is not a broken client, and dropping
// the shared client would make every other node on that connection reconnect).
func mongoTransport(err error) bool {
	var ke *kindError
	if errors.As(err, &ke) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	var se mongo.ServerError
	return !errors.As(err, &se)
}

// extJSON converts a JSON-able value (with extended-JSON operators such as {"$oid": ...})
// to BSON.
func extJSON(v any, out any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return bson.UnmarshalExtJSON(b, false, out)
}

// writeStage finds a pipeline stage that writes ($out, $merge) anywhere in the tree.
func writeStage(v any) string {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if k == "$out" || k == "$merge" {
				return k
			}
			if s := writeStage(e); s != "" {
				return s
			}
		}
	case []any:
		for _, e := range x {
			if s := writeStage(e); s != "" {
				return s
			}
		}
	}
	return ""
}

// plainExtJSON turns relaxed extended JSON wrappers into plain values so assertions
// compare naturally: {"$oid": "x"} -> "x", {"$date": "..."} -> "...", {"$numberLong": "9007199254740993"}
// -> that exact number (never rounded through float64).
func plainExtJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 1 {
			for k, e := range x {
				switch k {
				case "$oid", "$date", "$uuid", "$symbol":
					return plainExtJSON(e)
				case "$numberDecimal", "$numberLong", "$numberInt", "$numberDouble":
					if s, ok := e.(string); ok {
						n, _ := numberFromString(s) // exact; NaN / Infinity stay strings
						return n
					}
				}
			}
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = plainExtJSON(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = plainExtJSON(e)
		}
		return out
	}
	return v
}

func compact(v any) string {
	b, _ := json.Marshal(v)
	s := string(b)
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// mongoErr classifies a failure. If MongoDB itself rejected the operation (bad operator, not
// authorized) the query is wrong for this database (spec) and the message carries the
// server's error code and name; otherwise we never got an answer (infrastructure).
func mongoErr(err error) error {
	var ke *kindError
	if errors.As(err, &ke) {
		return err
	}
	var ce mongo.CommandError
	if errors.As(err, &ce) {
		name := ce.Name
		if name == "" {
			name = "unnamed"
		}
		return specErr(fmt.Errorf("mongo error %d (%s): %s", ce.Code, name, ce.Message))
	}
	var se mongo.ServerError
	if errors.As(err, &se) {
		return specErr(fmt.Errorf("mongo error (codes %v): %w", se.ErrorCodes(), err))
	}
	return transportErr(fmt.Errorf("mongo: %w", err))
}
