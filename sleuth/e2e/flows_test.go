package e2e

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"sleuth/internal/report"
)

// apiToken has characters that change when percent-encoded into a URL.
const apiToken = "p@ss w/rd&x=1"

type scenario struct {
	name  string
	flow  string
	opts  runOptions
	exit  int
	steps map[string]string
	check func(t *testing.T, r *result, parallel int)
}

// Every scenario runs at --parallel 1 and --parallel 4: the outcome must not depend on it.
func TestFlows(t *testing.T) {
	scenarios := []scenario{
		{
			name: "everything passes, secrets are masked", flow: "happy", exit: 0,
			steps: map[string]string{"login": "passed", "login_ok": "passed", "auth": "passed", "profile": "passed", "profile_ok": "passed"},
			check: func(t *testing.T, r *result, p int) {
				raw := r.rawEvidence(t)
				if strings.Contains(raw, password) || strings.Contains(r.stdout, password) || strings.Contains(r.stderr, password) {
					t.Error("the secret leaked into the evidence or the output")
				}
				if !strings.Contains(raw, "***") {
					t.Error("the masked value should be visible as ***")
				}
				if !strings.Contains(r.stdout, "PASSED") {
					t.Errorf("report should say PASSED:\n%s", r.stdout)
				}
				if p > 1 && r.run.Parallel != p || p == 1 && r.run.Parallel != 0 {
					t.Errorf("run.parallel = %d for -parallel %d", r.run.Parallel, p)
				}
			},
		},
		{
			name: "a secret never appears, in any encoding, in output or evidence", flow: "secret-in-url", exit: 3,
			opts:  runOptions{env: "env-secret.yaml", extraEnv: []string{"API_TOKEN=" + apiToken}},
			steps: map[string]string{"call": "infrastructure"},
			check: func(t *testing.T, r *result, _ int) {
				raw := r.rawEvidence(t)
				for _, form := range []string{apiToken, url.QueryEscape(apiToken), url.PathEscape(apiToken), "p@ss", "p%40ss"} {
					if strings.Contains(r.stdout, form) || strings.Contains(r.stderr, form) || strings.Contains(raw, form) {
						t.Errorf("the secret leaked as %q", form)
					}
				}
				if !strings.Contains(r.stdout, "***") {
					t.Errorf("the masked value should be visible as ***:\n%s", r.stdout)
				}
			},
		},
		{
			name: "a failed assertion exits 1 and skips its dependents", flow: "assertion-fail", exit: 1,
			steps: map[string]string{"health": "passed", "expect_sick": "assertion", "after": "skipped"},
			check: func(t *testing.T, r *result, _ int) {
				if c := r.step("expect_sick").Checks; len(c) != 1 || c[0].Passed || c[0].Message != "deliberately wrong" {
					t.Errorf("the failed check should be recorded with its message: %+v", c)
				}
			},
		},
		{
			name: "a broken flow is a spec error, exit 2", flow: "spec-error", exit: 2,
			steps: map[string]string{"broken": "spec"},
		},
		{
			name: "an unreachable host is infrastructure, exit 3", flow: "happy", exit: 3,
			opts:  runOptions{baseURL: "http://127.0.0.1:1"},
			steps: map[string]string{"login": "infrastructure", "login_ok": "skipped", "profile": "skipped"},
		},
		{
			name: "a hanging endpoint times out as infrastructure, exit 3", flow: "hang", exit: 3,
			steps: map[string]string{"hang": "infrastructure", "after": "skipped"},
		},
		{
			name: "assertion plus infrastructure failure: the assertion wins, exit 1", flow: "mixed", exit: 1,
			steps: map[string]string{"health": "passed", "wrong": "assertion", "down": "infrastructure"},
		},
		{
			name: "a missing secret is a setup error, exit 2", flow: "happy", exit: 2,
			opts: runOptions{noSecret: true},
			check: func(t *testing.T, r *result, _ int) {
				if !strings.Contains(r.stderr, "APP_PASSWORD") {
					t.Errorf("the error should name the missing secret:\n%s", r.stderr)
				}
			},
		},
		{
			name: "poll converges after several attempts", flow: "poll-ok", exit: 0,
			steps: map[string]string{"start": "passed", "wait": "passed", "done": "passed"},
			check: func(t *testing.T, r *result, _ int) {
				p := r.step("wait").Poll
				if p == nil || !p.Satisfied || p.Attempts != 3 || len(p.History) != 3 || !p.History[2].Passed || p.History[0].Passed {
					t.Errorf("poll evidence: %+v", p)
				}
			},
		},
		{
			name: "poll that never converges is a timeout, exit 1", flow: "poll-timeout", exit: 1,
			steps: map[string]string{"start": "passed", "wait": "timeout", "after": "skipped"},
			check: func(t *testing.T, r *result, _ int) {
				if p := r.step("wait").Poll; p == nil || p.Satisfied || p.Attempts < 3 {
					t.Errorf("poll evidence: %+v", p)
				}
			},
		},
		{
			name: "poll backoff widens the delays up to its cap", flow: "poll-backoff", exit: 0,
			steps: map[string]string{"wait": "passed"},
			check: func(t *testing.T, r *result, _ int) {
				p := r.step("wait").Poll
				if p == nil || p.Attempts != 5 {
					t.Fatalf("poll evidence: %+v", p)
				}
				var delays []int64
				for _, a := range p.History {
					if a.DelayMs > 0 {
						delays = append(delays, a.DelayMs)
					}
				}
				if len(delays) < 3 || delays[0] > 60 || delays[len(delays)-1] < 150 || delays[len(delays)-1] > 200 {
					t.Errorf("expected delays growing from ~40ms to the 200ms cap, got %v", delays)
				}
				for i := 1; i < len(delays); i++ {
					if delays[i] < delays[i-1] {
						t.Errorf("delays should not shrink: %v", delays)
					}
				}
			},
		},
		{
			name: "poll rides through dropped connections", flow: "poll-flaky", exit: 0,
			steps: map[string]string{"wait": "passed"},
			check: func(t *testing.T, r *result, _ int) {
				p := r.step("wait").Poll
				if p == nil || p.Attempts != 3 || p.History[0].Error == "" || p.History[1].Error == "" || p.History[2].Error != "" {
					t.Errorf("the first two attempts should record the error, the third succeed: %+v", p)
				}
			},
		},
		{
			name: "contract: every call matches and coverage is reported", flow: "contract-ok", exit: 0,
			steps: map[string]string{"health": "passed", "login": "passed", "user": "passed", "item": "passed"},
			check: func(t *testing.T, r *result, _ int) {
				d := r.run.Drift
				if d == nil || d.CallsChecked != 4 || len(d.Findings) != 0 || d.Failing != 0 {
					t.Fatalf("drift: %+v", d)
				}
				if len(d.Sources) != 1 || d.Sources[0].Exercised != 4 || d.Sources[0].Total != 4+len(d.Sources[0].Untested) {
					t.Fatalf("coverage: %+v", d.Sources)
				}
				if u := strings.Join(d.Sources[0].Untested, ","); !strings.Contains(u, "GET /legacy/{id}") || !strings.Contains(u, "POST /echo") {
					t.Errorf("untested operations should include the ones no step called: %s", u)
				}
			},
		},
		{
			name: "contract: a response that breaks the contract is drift, exit 1", flow: "contract-violation", exit: 1,
			steps: map[string]string{"legacy": "contract", "after": "skipped"},
			check: func(t *testing.T, r *result, _ int) {
				d := r.run.Drift
				if d == nil || d.Failing < 1 || !strings.Contains(fmt.Sprint(d.Findings), "total") {
					t.Errorf("the finding should name the bad field: %+v", d)
				}
			},
		},
		{
			name: "contract: a request that breaks the contract is never sent", flow: "contract-request-blocked", exit: 1,
			steps: map[string]string{"login": "contract"},
			check: func(t *testing.T, r *result, _ int) {
				if s := r.step("login"); s.Response != nil {
					t.Errorf("the request must not have been sent, but got a response: %+v", s.Response)
				}
			},
		},
		{
			name: "contract: accepted drift in warn mode passes", flow: "contract-warn", exit: 0,
			steps: map[string]string{"missing": "passed", "got_404": "passed"},
			check: func(t *testing.T, r *result, _ int) {
				if d := r.run.Drift; d == nil || d.Accepted < 1 || d.Failing != 0 {
					t.Errorf("an undocumented 404 should be accepted drift: %+v", d)
				}
			},
		},
		{
			name: "contract: an operation missing from the contract fails", flow: "contract-unknown-op", exit: 1,
			steps: map[string]string{"teapot": "contract"},
			check: func(t *testing.T, r *result, _ int) {
				if d := r.run.Drift; d == nil || len(d.Findings) == 0 || d.Findings[0].Where != "operation" {
					t.Errorf("%+v", d)
				}
			},
		},
		{
			name: "numbers beyond 2^53 are exact end to end", flow: "exact", exit: 0,
			steps: map[string]string{"big": "passed", "exact": "passed", "echo": "passed", "echoed": "passed"},
		},
		{
			name: "an id off by one in the last digit does not pass", flow: "exact-off", exit: 1,
			steps: map[string]string{"off_by_one": "assertion"},
		},
		{
			name: "spec drift against a saved baseline is reported, not failed, by default", flow: "contract-health", exit: 0,
			opts:  runOptions{env: "env-v2.yaml"},
			steps: map[string]string{"health": "passed", "big": "passed"},
			check: func(t *testing.T, r *result, _ int) {
				d := r.run.Drift
				if d == nil || len(d.SpecDrift) != 1 {
					t.Fatalf("drift: %+v", d)
				}
				sd := d.SpecDrift[0]
				if sd.Breaking < 4 || sd.CalledBreaking != 0 || sd.Failing || sd.Info < 1 {
					t.Errorf("v2 has four breaking changes (none in the two operations called) and one addition: %+v", sd)
				}
				if !strings.Contains(r.stdout, "Spec drift") {
					t.Errorf("the report should have a Spec drift section:\n%s", r.stdout)
				}
			},
		},
		{
			name: "-spec-drift-fail breaking fails the run on a moved spec, exit 1", flow: "contract-health", exit: 1,
			opts:  runOptions{env: "env-v2.yaml", args: []string{"-spec-drift-fail", "breaking"}},
			steps: map[string]string{"health": "passed", "big": "passed"},
			check: func(t *testing.T, r *result, _ int) {
				if d := r.run.Drift; d == nil || len(d.SpecDrift) != 1 || !d.SpecDrift[0].Failing {
					t.Errorf("%+v", d)
				}
			},
		},
		{
			name: "spec drift is still reported when the contract node never ran", flow: "drift-skipped", exit: 1,
			opts:  runOptions{env: "env-v2.yaml", args: []string{"-spec-drift-fail", "breaking"}},
			steps: map[string]string{"gate": "assertion", "health": "skipped"},
			check: func(t *testing.T, r *result, _ int) {
				d := r.run.Drift
				if d == nil || d.CallsChecked != 0 || len(d.SpecDrift) != 1 || !d.SpecDrift[0].Failing {
					t.Fatalf("the gate must hold even though no contract call ran: %+v", d)
				}
				if !strings.Contains(r.stdout, "no contract call ran") {
					t.Errorf("the report must not read as a clean result:\n%s", r.stdout)
				}
			},
		},
	}
	for _, sc := range scenarios {
		for _, p := range []int{1, 4} {
			sc, p := sc, p
			t.Run(fmt.Sprintf("%s/parallel=%d", sc.name, p), func(t *testing.T) {
				r := runFlow(t, sc.flow, p, sc.opts)
				expectExit(t, r, sc.exit)
				if sc.steps != nil {
					expectSteps(t, r, sc.steps)
				}
				if sc.check != nil {
					sc.check(t, r, p)
				}
			})
		}
	}
}

// With --parallel 4 four independent slow requests overlap; with 1 they do not.
func TestParallelismOverlapsIndependentNodes(t *testing.T) {
	type span struct{ start, end time.Time }
	spans := func(r *result) []span {
		var out []span
		for _, id := range []string{"s1", "s2", "s3", "s4"} {
			s := r.step(id)
			if s.Status != report.Passed {
				t.Fatalf("step %s: %+v", id, s)
			}
			out = append(out, span{s.StartedAt, s.StartedAt.Add(time.Duration(s.DurationMs) * time.Millisecond)})
		}
		return out
	}
	latestStart := func(sp []span) (t0 time.Time) {
		for _, s := range sp {
			if s.start.After(t0) {
				t0 = s.start
			}
		}
		return
	}
	earliestEnd := func(sp []span) time.Time {
		e := sp[0].end
		for _, s := range sp {
			if s.end.Before(e) {
				e = s.end
			}
		}
		return e
	}

	serial := runFlow(t, "parallel", 1, runOptions{})
	expectExit(t, serial, 0)
	sp := spans(serial)
	if latestStart(sp).Before(earliestEnd(sp)) {
		t.Errorf("with -parallel 1 the requests must not overlap: %+v", sp)
	}
	if serial.run.DurationMs < 1100 {
		t.Errorf("four 300ms requests one after another take over a second, took %dms", serial.run.DurationMs)
	}

	par := runFlow(t, "parallel", 4, runOptions{})
	expectExit(t, par, 0)
	sp = spans(par)
	if !latestStart(sp).Before(earliestEnd(sp)) {
		t.Errorf("with -parallel 4 all four requests should be in flight at once: %+v", sp)
	}
	if par.run.DurationMs > 900 {
		t.Errorf("four overlapping 300ms requests should take well under a second, took %dms", par.run.DurationMs)
	}
	if par.run.Parallel != 4 || par.step("all").Status != report.Passed {
		t.Errorf("run.parallel=%d, all=%+v", par.run.Parallel, par.step("all"))
	}
}
