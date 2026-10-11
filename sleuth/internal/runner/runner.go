// Package runner executes a flow's DAG and records evidence.
package runner

import (
	"context"
	"fmt"
	"sort"
	"time"

	"sleuth/internal/flow"
	"sleuth/internal/registry"
	"sleuth/internal/report"
)

type Options struct {
	EnvName string
	// Parallel is the maximum number of independent nodes run at once; 0 or 1 is serial.
	Parallel int
	// FailOnSpecDrift makes the run fail when a contract source differs from its saved baseline
	// by at least this severity: "breaking" or "review". Empty records the differences only.
	FailOnSpecDrift string
	// Timeout, if positive, bounds the whole run. When it passes, requests, queries and poll
	// waits in flight are stopped, nodes not yet started are skipped, and the run is marked
	// timed out. Each such step fails as infrastructure: the system did not finish in time.
	Timeout time.Duration
	// CaptureShapes records, on each step that returned JSON, the structure of its response (or
	// database rows) as Step.Shape, so a run can be compared with, or recorded as, a shape snapshot.
	CaptureShapes bool
	// OnEvent, if set, is told about progress as it happens. It is called from a single
	// goroutine (the scheduler), in order, and must not block for long. Steps in events
	// carry unmasked evidence: mask before they leave the process.
	OnEvent func(Event)
}

// Event is a progress notification from Run.
type Event struct {
	Type string // run_start | step_start | step_done
	At   time.Time
	// run_start: every node and what it waits for.
	Nodes []EventNode
	// step_start: the step's id and kind. step_done: the finished step (including skipped).
	ID   string
	Kind string
	Step *report.Step
}

// EventNode describes one node of the plan in a run_start event.
type EventNode struct {
	ID    string   `json:"id"`
	Kind  string   `json:"kind"`
	Needs []string `json:"needs,omitempty"` // needs: plus nodes.<id> references
}

// Run executes the flow. It returns an error only for setup problems
// (bad env, missing secrets); node failures are recorded in the report.
//
// A node starts when everything it depends on (needs: plus nodes.<id> references) has
// finished; it runs only if all of those passed, otherwise it is skipped. Among ready
// nodes the earliest in flow order goes first, so with Parallel <= 1 execution is
// exactly the serial order. Evidence lists steps in that order whatever the timing.
func Run(f *flow.Flow, env *EnvConfig, opts Options) (*report.Run, []string, error) {
	order, err := f.Order()
	if err != nil {
		return nil, nil, err
	}
	ctx, secrets, err := NewContext(env)
	if err != nil {
		return nil, nil, err
	}
	specs, err := loadContracts(f, env)
	if err != nil {
		return nil, nil, err
	}
	baselineDrift, err := diffBaselines(env, specs)
	if err != nil {
		return nil, nil, err
	}
	workers := opts.Parallel
	if workers < 1 {
		workers = 1
	}
	conns := newDBConns(workers)
	defer conns.close()
	if opts.Timeout > 0 {
		rctx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
		defer cancel()
		ctx.run = rctx
	}
	run := &report.Run{Flow: f.Name, Env: opts.EnvName, Status: report.Passed, StartedAt: time.Now().UTC()}
	if workers > 1 {
		run.Parallel = workers
	}

	pos := map[string]int{}
	for i, n := range order {
		pos[n.ID] = i
	}
	deps := make([][]int, len(order))
	pending := make([]int, len(order))
	dependents := make([][]int, len(order))
	for i, n := range order {
		for _, d := range n.Deps() {
			deps[i] = append(deps[i], pos[d])
			dependents[pos[d]] = append(dependents[pos[d]], i)
		}
		pending[i] = len(deps[i])
	}

	emit := func(e Event) {
		if opts.OnEvent != nil {
			e.At = time.Now().UTC()
			opts.OnEvent(e)
		}
	}
	if opts.OnEvent != nil {
		plan := make([]EventNode, len(order))
		for i, n := range order {
			plan[i] = EventNode{ID: n.ID, Kind: n.Kind, Needs: n.Deps()}
		}
		emit(Event{Type: "run_start", Nodes: plan})
	}

	steps := make([]report.Step, len(order))
	jobs := make(chan int, len(order))
	done := make(chan int, len(order))
	for w := 0; w < workers; w++ {
		go func() {
			for i := range jobs {
				steps[i] = execNode(ctx, env, conns, order[i], specs)
				done <- i
			}
		}()
	}

	// Everything below runs on this goroutine. A worker's write to steps[i] happens
	// before its send on done, which happens before the read here.
	var ready []int // positions, ascending
	finished := 0
	var finish, resolve func(i int)
	resolve = func(i int) {
		for _, d := range deps[i] {
			if steps[d].Status != report.Passed {
				steps[i] = report.Step{
					ID: order[i].ID, Kind: order[i].Kind, StartedAt: time.Now().UTC(), Status: report.Skipped,
					Error: fmt.Sprintf("skipped: dependency %q did not pass", order[d].ID),
				}
				finish(i)
				return
			}
		}
		at := sort.SearchInts(ready, i)
		ready = append(ready, 0)
		copy(ready[at+1:], ready[at:])
		ready[at] = i
	}
	finish = func(i int) {
		finished++
		if opts.OnEvent != nil {
			st := steps[i]
			emit(Event{Type: "step_done", ID: st.ID, Kind: st.Kind, Step: &st})
		}
		if steps[i].Status == report.Failed {
			run.Status = report.Failed
		}
		for _, j := range dependents[i] {
			if pending[j]--; pending[j] == 0 {
				resolve(j)
			}
		}
	}
	for i := range order {
		if pending[i] == 0 {
			resolve(i)
		}
	}
	inflight := 0
	for finished < len(order) {
		for inflight < workers && len(ready) > 0 {
			i := ready[0]
			ready = ready[1:]
			if ctx.runCtx().Err() != nil { // past the run's timeout: do not start anything new
				steps[i] = report.Step{ID: order[i].ID, Kind: order[i].Kind, StartedAt: time.Now().UTC(), Status: report.Skipped,
					Error: fmt.Sprintf("not started: the run's timeout (%s) was reached", opts.Timeout)}
				finish(i)
				continue
			}
			emit(Event{Type: "step_start", ID: order[i].ID, Kind: order[i].Kind})
			jobs <- i
			inflight++
		}
		if inflight == 0 {
			if len(ready) > 0 {
				continue // nodes skipped for the timeout released more ready nodes: skip those too
			}
			break // unreachable: Order() rejected cycles
		}
		finish(<-done)
		inflight--
	}
	close(jobs)

	run.Steps = steps
	if ctx.runCtx().Err() != nil {
		run.TimedOut = true
		run.Timeout = opts.Timeout.String()
		run.Status = report.Failed
	}
	if opts.CaptureShapes {
		for i := range run.Steps {
			run.Steps[i].Shape = stepShape(&run.Steps[i])
		}
	}
	run.DurationMs = time.Since(run.StartedAt).Milliseconds()
	report.SummarizeDrift(run)
	if len(specs) > 0 {
		report.EnsureDrift(run) // the flow references a contract: report coverage and spec drift even if no call ran
	}
	published := map[string][]string{}
	for name, sp := range specs {
		published[name] = sp.Operations()
	}
	report.AddCoverage(run, published)
	report.AddSpecDrift(run, baselineDrift, opts.FailOnSpecDrift)
	return run, secrets, nil
}

// execNode runs one node whose dependencies all passed and records its evidence.
func execNode(ctx *Context, env *EnvConfig, conns *dbConns, n flow.Node, specs map[string]*registry.Spec) report.Step {
	step := report.Step{ID: n.ID, Kind: n.Kind, StartedAt: time.Now().UTC()}
	start := time.Now()
	var out map[string]any
	var err error
	switch n.Kind {
	case flow.KindRequest:
		out, err = execRequest(ctx, n, &step, specs)
	case flow.KindPoll:
		out, err = execPoll(ctx, env, conns, n, &step, specs)
	case flow.KindDB:
		out, err = execDB(ctx, env, conns, n, &step)
	case flow.KindAssertion:
		out, err = execAssertion(ctx, n, &step)
	case flow.KindTransform:
		out, err = execTransform(ctx, n, &step)
	}
	step.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		step.Status = report.Failed
		step.Error = err.Error()
		step.ErrorKind = kindOf(err)
		if ctx.runCtx().Err() != nil && step.ErrorKind != KindSpec {
			// whatever the step was doing when the run's timeout passed, it did not finish in time
			step.ErrorKind = KindInfra
			step.Error = "stopped: the run's timeout was reached (" + step.Error + ")"
		}
	} else {
		step.Status = report.Passed
		ctx.SetNode(n.ID, out)
	}
	return step
}
