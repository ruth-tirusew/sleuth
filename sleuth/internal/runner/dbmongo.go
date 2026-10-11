package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"sleuth/internal/flow"
	"sleuth/internal/report"
)

func (c *dbConns) mongoClient(name, dsn string, timeout time.Duration) (cl *mongo.Client, reused bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl = c.mongo[name]; cl != nil {
		return cl, true, nil
	}
	cl, err = mongo.Connect(options.Client().ApplyURI(dsn).SetServerSelectionTimeout(timeout))
	if err != nil {
		return nil, false, err
	}
	c.mongo[name] = cl
	return cl, false, nil
}

// queryMongo runs a find (filter/sort/projection) or an aggregation (pipeline). Only
// reads are issued; pipelines containing $out or $merge are rejected because those
// stages write.
func queryMongo(cctx context.Context, ctx *Context, conns *dbConns, dsn string, d *flow.DB, maxRows int, timeout time.Duration, step *report.Step) ([]any, error) {
	if !strings.HasPrefix(dsn, "mongodb://") && !strings.HasPrefix(dsn, "mongodb+srv://") {
		return nil, specErr(fmt.Errorf("databases.%s is not a MongoDB URI but the node has type mongo", d.Connection))
	}
	var filter bson.D
	var pipeline []bson.D
	var desc string
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
		desc = "aggregate"
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
		desc = "find"
		step.DB.Query = fmt.Sprintf("find %s.%s %s", d.Database, d.Collection, compact(rf))
	}
	_ = desc

	run := func() ([]any, bool, error) {
		cl, reused, err := conns.mongoClient(d.Connection, dsn, timeout)
		if err != nil {
			return nil, false, err
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
					return nil, reused, specErr(fmt.Errorf("db.projection: %w", err))
				}
				opts.SetProjection(proj)
			}
			cur, err = coll.Find(cctx, filter, opts)
		}
		if err != nil {
			return nil, reused, err
		}
		defer cur.Close(context.Background())
		var rows []any
		for cur.Next(cctx) {
			b, err := bson.MarshalExtJSON(cur.Current, false, false)
			if err != nil {
				return nil, reused, specErr(err)
			}
			var doc any
			if err := json.Unmarshal(b, &doc); err != nil {
				return nil, reused, specErr(err)
			}
			rows = append(rows, plainExtJSON(doc))
		}
		return rows, reused, cur.Err()
	}
	rows, _, err := run()
	if err != nil {
		return nil, mongoErr(err)
	}
	return rows, nil
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
// compare naturally: {"$oid": "x"} -> "x", {"$date": "..."} -> "...", decimals -> number.
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
						var f float64
						if _, err := fmt.Sscanf(s, "%g", &f); err == nil {
							return f
						}
						return s
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

// mongoErr: if MongoDB itself rejected the operation (bad operator, not authorized) the
// query is wrong for this database (spec); otherwise we never got an answer (infrastructure).
func mongoErr(err error) error {
	var ke *kindError
	if errors.As(err, &ke) {
		return err
	}
	var se mongo.ServerError
	if errors.As(err, &se) {
		return specErr(fmt.Errorf("mongo error %v: %w", se.HasErrorCode(0), err))
	}
	return transportErr(fmt.Errorf("mongo: %w", err))
}
