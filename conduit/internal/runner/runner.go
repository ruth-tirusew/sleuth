// Package runner executes a flow's DAG and records evidence.
package runner

import (
	"fmt"
	"time"

	"conduit/internal/flow"
	"conduit/internal/report"
)

type Options struct {
	EnvName string
}

// Run executes the flow. It returns an error only for setup problems
// (bad env, missing secrets); node failures are recorded in the report.
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
	run := &report.Run{Flow: f.Name, Env: opts.EnvName, Status: report.Passed, StartedAt: time.Now().UTC()}
	status := map[string]string{}

	for _, n := range order {
		step := report.Step{ID: n.ID, Kind: n.Kind, StartedAt: time.Now().UTC()}
		var blocked string
		for _, d := range n.Deps() {
			if status[d] != report.Passed {
				blocked = d
				break
			}
		}
		if blocked != "" {
			step.Status = report.Skipped
			step.Error = fmt.Sprintf("skipped: dependency %q did not pass", blocked)
		} else {
			start := time.Now()
			var out map[string]any
			var err error
			switch n.Kind {
			case flow.KindRequest:
				out, err = execRequest(ctx, n, &step, specs)
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
			} else {
				step.Status = report.Passed
				ctx.SetNode(n.ID, out)
			}
		}
		status[n.ID] = step.Status
		if step.Status == report.Failed {
			run.Status = report.Failed
		}
		run.Steps = append(run.Steps, step)
	}
	run.DurationMs = time.Since(run.StartedAt).Milliseconds()
	return run, secrets, nil
}
