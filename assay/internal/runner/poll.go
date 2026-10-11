package runner

import (
	"errors"
	"fmt"
	"time"

	"assay/internal/flow"
	"assay/internal/registry"
	"assay/internal/report"
)

const (
	defaultPollInterval = time.Second
	defaultPollTimeout  = 30 * time.Second
)

// execPoll repeats a request or a database query until every `until` check passes or
// the window closes. Transient transport errors are retried; on timeout the failure
// kind says which it was: the system answered but never converged (timeout), or never
// answered at all (infrastructure). Extraction runs once, on the final successful result.
func execPoll(ctx *Context, env *EnvConfig, conns *dbConns, n flow.Node, step *report.Step, specs map[string]*registry.Spec) (map[string]any, error) {
	p := n.Poll
	interval, timeout := defaultPollInterval, defaultPollTimeout
	var err error
	if p.Interval != "" {
		if interval, err = time.ParseDuration(p.Interval); err != nil {
			return nil, specErr(err)
		}
	}
	if p.Timeout != "" {
		if timeout, err = time.ParseDuration(p.Timeout); err != nil {
			return nil, specErr(err)
		}
	}
	if interval <= 0 || timeout <= 0 {
		return nil, specErr(errors.New("poll interval and timeout must be positive"))
	}

	// attempt runs the target once, publishing its result as nodes.<id> for `until`.
	// remaining caps a single attempt so it can never outlive the window.
	var attempt func(remaining time.Duration) (result map[string]any, status int, rows int, err error)
	var finish func(result map[string]any) (map[string]any, error)

	switch {
	case p.Request != nil:
		req := *p.Request
		extract := req.Extract
		req.Extract = nil // intermediate responses are often incomplete
		reqTimeout := 30 * time.Second
		if req.Timeout != "" {
			if reqTimeout, err = time.ParseDuration(req.Timeout); err != nil {
				return nil, specErr(fmt.Errorf("bad timeout: %w", err))
			}
		}
		// Intermediate answers ("PENDING") legitimately differ from the final shape, so each
		// attempt is checked in warn mode and the verdict is made once, in finish: the
		// satisfying attempt's findings plus any undocumented status seen along the way.
		// Only request-side findings are deterministic enough to stop early.
		var attemptContract *flow.Contract
		var carried []report.Violation
		if n.Contract != nil {
			attemptContract = &flow.Contract{Source: n.Contract.Source, OnViolation: "warn"}
		}
		attempt = func(remaining time.Duration) (map[string]any, int, int, error) {
			r := req
			r.Timeout = minDuration(reqTimeout, remaining).String()
			out, err := execRequest(ctx, flow.Node{ID: n.ID, Kind: flow.KindRequest, Request: &r, Contract: attemptContract}, step, specs)
			if cr := step.Contract; cr != nil {
				for _, v := range cr.Violations {
					if undocumented(v) {
						carried = append(carried, v)
					}
				}
				if n.Contract.OnViolation == "warn" {
					cr.Mode = "warn"
				} else {
					cr.Mode = "fail"
				}
				for _, v := range cr.Violations {
					if v.Where != "response" && cr.Mode == "fail" { // operation/request: same every attempt
						return nil, 0, 0, contractErr(fmt.Errorf("poll request disagrees with %s contract: [%s] %s %s", cr.Source, v.Where, v.Path, v.Message))
					}
				}
			}
			if err != nil {
				return nil, 0, 0, err
			}
			return out, step.Response.Status, 0, nil
		}
		finish = func(result map[string]any) (map[string]any, error) {
			if cr := step.Contract; cr != nil {
				seen := map[string]bool{}
				for _, v := range cr.Violations {
					seen[v.Message] = true
				}
				for _, v := range carried {
					if !seen[v.Message] {
						seen[v.Message] = true
						cr.Violations = append(cr.Violations, v)
					}
				}
				if len(cr.Violations) > 0 && cr.Mode == "fail" {
					return nil, contractErr(fmt.Errorf("poll response: %d contract violation(s) against %s (first: [%s] %s %s)",
						len(cr.Violations), cr.Source, cr.Violations[0].Where, cr.Violations[0].Path, cr.Violations[0].Message))
				}
			}
			response, _ := result["response"].(map[string]any)
			outputs, err := extractOutputs(response, extract)
			if err != nil {
				return nil, err
			}
			step.Output = outputs
			return map[string]any{"response": response, "out": outputs}, nil
		}
	default:
		db := *p.DB
		extract := db.Extract
		db.Extract = nil
		dbTimeout := defaultDBTimeout
		if db.Timeout != "" {
			if dbTimeout, err = time.ParseDuration(db.Timeout); err != nil {
				return nil, specErr(err)
			}
		}
		attempt = func(remaining time.Duration) (map[string]any, int, int, error) {
			d := db
			d.Timeout = minDuration(dbTimeout, remaining).String()
			rows, err := queryDB(ctx, env, conns, &d, step)
			if err != nil {
				return nil, 0, 0, err
			}
			res, err := dbResult(rows, nil)
			return res, 0, len(rows), err
		}
		finish = func(result map[string]any) (map[string]any, error) {
			rows, _ := result["rows"].([]any)
			res, err := dbResult(rows, extract)
			if err != nil {
				return nil, err
			}
			step.Output, _ = res["out"].(map[string]any)
			return res, nil
		}
	}

	res := &report.PollResult{IntervalMs: interval.Milliseconds(), TimeoutMs: timeout.Milliseconds()}
	step.Poll = res
	start := time.Now()
	deadline := start.Add(timeout)
	var lastErr error

	for i := 1; ; i++ {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		a := report.PollAttempt{N: i}
		result, status, rows, aerr := attempt(remaining)
		a.ElapsedMs, a.Status, a.RowCount = time.Since(start).Milliseconds(), status, rows
		lastErr = nil
		switch {
		case aerr != nil && (kindOf(aerr) == KindSpec || kindOf(aerr) == KindContract):
			a.Error = aerr.Error()
			res.History, res.Attempts = append(res.History, a), i
			return nil, aerr // the flow or contract is broken; retrying cannot help
		case aerr != nil:
			a.Error, lastErr = aerr.Error(), aerr
		default:
			ctx.SetNode(n.ID, result) // until-checks read nodes.<id>.*
			results, failed, cerr := evalChecks(ctx, p.Until)
			step.Checks = results
			if cerr != nil {
				a.Error = cerr.Error()
				res.History, res.Attempts = append(res.History, a), i
				return nil, cerr
			}
			a.Passed = failed == 0
		}
		res.History, res.Attempts = append(res.History, a), i
		if a.Passed {
			res.Satisfied = true
			out, err := finish(result)
			if err != nil {
				return nil, err
			}
			out["attempts"] = float64(i)
			return out, nil
		}
		if time.Until(deadline) <= interval {
			break
		}
		time.Sleep(interval)
	}

	if lastErr != nil { // the final attempt got no usable answer
		return nil, transportErr(fmt.Errorf("poll gave up after %d attempt(s) in %s: %w", res.Attempts, timeout, lastErr))
	}
	return nil, timeoutErr(fmt.Errorf("condition not met after %d attempt(s) in %s", res.Attempts, timeout))
}

func minDuration(a, b time.Duration) time.Duration {
	if b < a {
		return b
	}
	return a
}
