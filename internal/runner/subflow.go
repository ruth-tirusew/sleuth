package runner

import (
	"context"
	"fmt"
	"time"

	"sleuth/internal/flow"
	"sleuth/internal/registry"
	"sleuth/internal/report"
)

// child returns the context a subflow runs in: the same env, secrets and run-wide timeout, but
// its own node outputs and params. The caller's nodes are not visible inside, so a subflow can
// change internally without breaking whoever calls it.
func (c *Context) child() *Context {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return &Context{run: c.run, grace: c.grace, data: map[string]any{
		"env": c.data["env"], "secrets": c.data["secrets"],
		"nodes": map[string]any{}, "params": map[string]any{},
	}}
}

func (c *Context) setParams(p map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data["params"] = p
}

// bindParams builds the params a flow runs with: each declared param takes the value it was
// given, else its default (evaluated in ctx), else the flow cannot run.
func bindParams(f *flow.Flow, ctx *Context, given map[string]any) (map[string]any, error) {
	out := map[string]any{}
	var missing []string
	for _, p := range f.Params {
		if v, ok := given[p.Name]; ok {
			out[p.Name] = v
			continue
		}
		if !p.HasDefault {
			missing = append(missing, p.Name)
			continue
		}
		v, err := ctx.Render(p.Default)
		if err != nil {
			return nil, fmt.Errorf("param %q default: %w", p.Name, err)
		}
		out[p.Name] = v
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("flow %q needs param(s) %v, which nothing supplied: run it from a flow that passes them, or give them defaults", f.Name, missing)
	}
	return out, nil
}

// execFlow runs a subflow node. The subflow's nodes run one at a time inside this node's worker
// slot: sharing the pool with the parent could leave every slot held by a parent waiting for a
// child that has none. Parallelism applies to the top-level flow's nodes.
func execFlow(ctx *Context, env *EnvConfig, conns *dbConns, n flow.Node, step *report.Step, specs map[string]*registry.Spec) (map[string]any, error) {
	sub := n.Sub
	if sub == nil {
		return nil, specErr(fmt.Errorf("subflow %q was not loaded", n.Flow))
	}
	step.Flow = n.Flow
	given := map[string]any{}
	for k, v := range n.Params {
		r, err := ctx.Render(v)
		if err != nil {
			return nil, specErr(fmt.Errorf("param %q: %w", k, err))
		}
		given[k] = r
	}
	child := ctx.child()
	params, err := bindParams(sub, child, given)
	if err != nil {
		return nil, specErr(err)
	}
	child.setParams(params)
	if len(params) > 0 {
		step.Params = params
	}
	order, err := sub.Order()
	if err != nil {
		return nil, specErr(err)
	}
	step.Children = runSerial(child, env, conns, order, specs)
	mainFailure := childFailure(n.Flow, step.Children)
	// the subflow's own cleanup runs when it ends, pass or fail; a failed cleanup step is
	// evidence and fails the run, but does not turn the calling node into a failure
	if cleanup := runFinally(child, env, conns, sub, step.Children, specs, nil); len(cleanup) > 0 {
		step.Children = append(step.Children, cleanup...)
	}
	if mainFailure != nil {
		return nil, mainFailure
	}
	out := map[string]any{}
	for name, v := range sub.Outputs {
		r, err := child.Render(v)
		if err != nil {
			return nil, specErr(fmt.Errorf("subflow %s: output %q: %w", n.Flow, name, err))
		}
		out[name] = r
	}
	step.Output = out
	return map[string]any{"out": out}, nil
}

// runSerial runs nodes (already in dependency order) one after another. A node whose
// dependency did not pass is skipped, as at the top level.
func runSerial(ctx *Context, env *EnvConfig, conns *dbConns, order []flow.Node, specs map[string]*registry.Spec) []report.Step {
	pos := map[string]int{}
	for i, n := range order {
		pos[n.ID] = i
	}
	steps := make([]report.Step, len(order))
	for i, n := range order {
		skip := func(why string) {
			steps[i] = report.Step{ID: n.ID, Kind: n.Kind, StartedAt: time.Now().UTC(), Status: report.Skipped, Error: why}
		}
		blocked := ""
		for _, d := range n.Deps() {
			if steps[pos[d]].Status != report.Passed {
				blocked = d
				break
			}
		}
		switch {
		case blocked != "":
			skip(fmt.Sprintf("skipped: dependency %q did not pass", blocked))
		case ctx.runCtx().Err() != nil:
			skip("not started: the run's timeout was reached")
		default:
			steps[i] = execNode(ctx, env, conns, n, specs)
		}
	}
	return steps
}

// runFinally runs f's cleanup nodes one at a time, after its main nodes (done) have finished,
// under a context of its own so a run that hit its timeout still cleans up. A node whose
// dependencies did not pass is skipped. A failed node is marked Cleanup with kind "cleanup".
// emit, if set, is told about each step as it starts and finishes.
func runFinally(ctx *Context, env *EnvConfig, conns *dbConns, f *flow.Flow, done []report.Step, specs map[string]*registry.Spec, emit func(Event)) []report.Step {
	if len(f.Finally) == 0 {
		return nil
	}
	order, err := f.FinallyOrder()
	if err != nil { // Validate rejects this; kept so a hand-built flow cannot panic here
		return []report.Step{{ID: "finally", Kind: "finally", Status: report.Failed, Cleanup: true,
			StartedAt: time.Now().UTC(), Error: err.Error(), ErrorKind: KindCleanup}}
	}
	grace := ctx.grace
	if grace <= 0 {
		grace = DefaultCleanupGrace
	}
	gctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	ctx.run = gctx // every node has finished, so nothing else reads it

	status := map[string]string{}
	for _, s := range done {
		status[s.ID] = s.Status
	}
	var out []report.Step
	for _, n := range order {
		blocked := ""
		for _, d := range n.Deps() {
			if status[d] != report.Passed {
				blocked = d
				break
			}
		}
		var st report.Step
		if blocked != "" {
			st = report.Step{ID: n.ID, Kind: n.Kind, StartedAt: time.Now().UTC(), Status: report.Skipped,
				Error: fmt.Sprintf("skipped: %q did not pass, so there is nothing to clean up", blocked)}
		} else {
			if emit != nil {
				emit(Event{Type: "step_start", ID: n.ID, Kind: n.Kind})
			}
			st = execNode(ctx, env, conns, n, specs)
		}
		st.Cleanup = true
		if st.Status == report.Failed {
			st.ErrorKind = KindCleanup
		}
		status[n.ID] = st.Status
		if emit != nil {
			c := st
			emit(Event{Type: "step_done", ID: c.ID, Kind: c.Kind, Step: &c})
		}
		out = append(out, st)
	}
	return out
}

// childFailure turns failed steps inside a subflow into one error for the node that ran it. The
// message names the first failed step; the kind is the most severe among all failed steps, in
// the order the exit code uses (spec, then product failures, then infrastructure), so a caller
// that retries on infrastructure failures is not misled.
func childFailure(path string, steps []report.Step) error {
	rank := func(kind string) int {
		switch kind {
		case KindCleanup:
			return 0
		case KindSpec:
			return 3
		case KindAssertion, KindContract, KindTimeout:
			return 2
		case KindInfra:
			return 1
		}
		return 3 // an unlabelled failure is treated as a flow problem
	}
	var first *report.Step
	worst := ""
	for i := range steps {
		s := &steps[i]
		if s.Status != report.Failed {
			continue
		}
		if first == nil {
			first = s
		}
		if worst == "" || rank(s.ErrorKind) > rank(worst) {
			worst = s.ErrorKind
		}
	}
	if first == nil {
		return nil
	}
	return &kindError{worst, fmt.Errorf("subflow %s: step %s failed: %s", path, first.ID, first.Error)}
}
