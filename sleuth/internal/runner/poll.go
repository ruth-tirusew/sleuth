package runner

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"time"

	"sleuth/internal/flow"
	"sleuth/internal/registry"
	"sleuth/internal/report"
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
	// truncated is set by attempt when the window, not the configured timeout, limited it.
	// An attempt that fails by hitting that deadline says nothing about the service; one that
	// fails fast for a real reason (connection refused, reset) does (see the verdict below).
	var truncated bool
	// settleUnsatisfied runs when the poll gives up, before the verdict is returned.
	settleUnsatisfied := func() {}

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
		// Intermediate answers ("PENDING") legitimately differ from the final shape, so
		// response findings are deferred: the verdict is made once, in finish, from the
		// satisfying attempt plus any undocumented status seen along the way. Request-side
		// findings (operation not in the contract, bad request body) are the same on every
		// attempt and block as in a request node, so the request is never sent.
		var attemptContract *flow.Contract
		var carried []report.Violation
		// answered holds, for every attempt that got a response, its response-side findings.
		// A finding present on all of them is persistent drift; one that changes from attempt to
		// attempt is an intermediate ("still pending") shape.
		var answered [][]report.Violation
		if n.Contract != nil {
			c := *n.Contract
			c.DeferResponse = true
			attemptContract = &c
		}
		attempt = func(remaining time.Duration) (map[string]any, int, int, error) {
			r := req
			r.Timeout = minDuration(reqTimeout, remaining).String()
			truncated = remaining < reqTimeout
			out, err := execRequest(ctx, flow.Node{ID: n.ID, Kind: flow.KindRequest, Request: &r, Contract: attemptContract}, step, specs)
			if cr := step.Contract; cr != nil {
				for _, v := range cr.Violations {
					if undocumented(v) {
						carried = append(carried, v)
					}
				}
			}
			if err != nil {
				return nil, 0, 0, err
			}
			if cr := step.Contract; cr != nil {
				var resp []report.Violation
				for _, v := range cr.Violations {
					if v.Where == "response" {
						resp = append(resp, v)
					}
				}
				answered = append(answered, resp)
			}
			return out, step.Response.Status, 0, nil
		}
		// When the poll gives up, what to report from its attempts:
		//   - operation / request findings (the same on every attempt);
		//   - undocumented statuses seen along the way;
		//   - response findings present on every answered attempt: persistent drift. This needs
		//     at least two answers; with one, a "still pending" shape cannot be told from drift.
		// Response findings that differ between attempts are intermediate shapes and are dropped.
		settleUnsatisfied = func() {
			cr := step.Contract
			if cr == nil {
				return
			}
			var kept []report.Violation
			for _, v := range cr.Violations {
				if v.Where != "response" {
					kept = append(kept, v)
				}
			}
			if len(answered) >= 2 {
				kept = mergeViolations(kept, persistent(answered))
			} else if len(answered) == 1 && len(answered[0]) > 0 {
				// One answer cannot tell a "still pending" shape from drift, so it is not reported
				// as drift, but it is not silent either.
				cr.Notes = append(cr.Notes, fmt.Sprintf("%d response finding(s) from the only answered attempt were not reported as drift: a poll needs two answers to tell a pending shape from a contract break", len(answered[0])))
			}
			cr.Violations = mergeViolations(kept, carried)
		}
		finish = func(result map[string]any) (map[string]any, error) {
			if cr := step.Contract; cr != nil {
				cr.Violations = mergeViolations(cr.Violations, carried)
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
			truncated = remaining < dbTimeout
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
	var lastErr error // the most recent attempt's error, for the message
	var fullErr error // the most recent failure of an attempt that had its full time budget
	answered := false // did any attempt get a usable answer?

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
			if !(truncated && isDeadlineErr(aerr)) { // only the window running out says nothing about the service
				fullErr = aerr
			}
		default:
			answered, fullErr = true, nil
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
		remaining = time.Until(deadline)
		if remaining <= interval {
			break
		}
		delay := nextDelay(p.Backoff, interval, i, rand.Float64())
		if delay > interval && delay > remaining-interval {
			delay = remaining - interval // keep room for one last attempt
		}
		res.History[len(res.History)-1].DelayMs = delay.Milliseconds()
		select {
		case <-time.After(delay):
		case <-ctx.runCtx().Done(): // the run's own timeout: stop polling now
		}
		if ctx.runCtx().Err() != nil {
			break
		}
	}

	settleUnsatisfied()
	// Verdict. "infrastructure" means the system never gave a usable answer, or the last
	// attempt that had its full time budget failed. An attempt cut short by the poll's own
	// deadline is the harness running out of time, not evidence about the service: a poll
	// that was answered on earlier attempts and merely never converged is a timeout.
	if fullErr != nil || (!answered && lastErr != nil) {
		err := fullErr
		if err == nil {
			err = lastErr
		}
		return nil, transportErr(fmt.Errorf("poll gave up after %d attempt(s) in %s: %w", res.Attempts, timeout, err))
	}
	return nil, timeoutErr(fmt.Errorf("condition not met after %d attempt(s) in %s", res.Attempts, timeout))
}

// vkey identifies a finding: two are the same only if where, path and message all match.
type vkey struct{ where, path, msg string }

func keyOf(v report.Violation) vkey { return vkey{v.Where, v.Path, v.Message} }

// mergeViolations appends the violations of extra that dst does not already hold.
func mergeViolations(dst, extra []report.Violation) []report.Violation {
	seen := make(map[vkey]bool, len(dst))
	for _, v := range dst {
		seen[keyOf(v)] = true
	}
	for _, v := range extra {
		if k := keyOf(v); !seen[k] {
			seen[k] = true
			dst = append(dst, v)
		}
	}
	return dst
}

// persistent returns the findings (in the first attempt's order) that every attempt reported.
func persistent(attempts [][]report.Violation) []report.Violation {
	var out []report.Violation
	for _, v := range attempts[0] {
		inAll := true
		for _, other := range attempts[1:] {
			found := false
			for _, o := range other {
				if keyOf(o) == keyOf(v) {
					found = true
					break
				}
			}
			if !found {
				inAll = false
				break
			}
		}
		if inAll {
			out = append(out, v)
		}
	}
	return out
}

// isDeadlineErr reports whether err is a timeout: a deadline passing, not the service failing.
// It relies on the HTTP client, pgx and the Mongo driver all wrapping the context error;
// matching on message text would also excuse a server that happens to say "deadline exceeded".
func isDeadlineErr(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func nextDelay(b *flow.Backoff, interval time.Duration, i int, rnd float64) time.Duration {
	if b == nil {
		return interval
	}
	factor := b.Factor
	if factor == 0 {
		factor = 2
	}
	d := float64(interval) * math.Pow(factor, float64(i-1))
	if b.Max != "" {
		if max, err := time.ParseDuration(b.Max); err == nil && d > float64(max) {
			d = float64(max)
		}
	}
	d *= 1 + b.Jitter*(2*rnd-1)
	if d < 1 {
		d = 1
	}
	return time.Duration(d)
}

func minDuration(a, b time.Duration) time.Duration {
	if b < a {
		return b
	}
	return a
}
