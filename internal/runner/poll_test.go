package runner

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sleuth/internal/flow"
	"sleuth/internal/report"
)

// asyncServer answers {"status":"PENDING"} until the n-th call, then {"status":"DONE", "id": 9}.
func asyncServer(readyAt int32) (*httptest.Server, *int32) {
	var calls int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) >= readyAt {
			fmt.Fprint(w, `{"status":"DONE","id":9}`)
			return
		}
		fmt.Fprint(w, `{"status":"PENDING"}`)
	})), &calls
}

const pollFlow = `
version: 1
name: t
nodes:
  - id: wait
    kind: poll
    poll:
      interval: 20ms
      timeout: %s
      request:
        url: "{{ .env.base }}/"
        extract: {job: body.id}
      until:
        - {path: nodes.wait.response.body.status, equals: DONE}
  - id: use
    kind: transform
    transform: {set: {job: "{{ .nodes.wait.out.job }}"}}
`

func TestPollSucceedsOnceConditionHolds(t *testing.T) {
	srv, calls := asyncServer(4)
	defer srv.Close()
	r := runYAML(t, srv.URL, fmt.Sprintf(pollFlow, "5s"))
	w := step(r, "wait")
	if w.Status != report.Passed || w.Poll == nil || w.Poll.Attempts != 4 || !w.Poll.Satisfied {
		t.Fatalf("%+v %+v", w, w.Poll)
	}
	if int(*calls) != 4 || len(w.Poll.History) != 4 || w.Poll.History[0].Passed || !w.Poll.History[3].Passed {
		t.Fatalf("calls=%d history=%+v", *calls, w.Poll.History)
	}
	// extract ran on the final response only, and later nodes can use it
	if w.Output["job"] == nil || step(r, "use").Status != report.Passed {
		t.Fatalf("output not chained: %+v %+v", w.Output, step(r, "use"))
	}
}

func TestPollTimeoutIsItsOwnKindAndKeepsEvidence(t *testing.T) {
	srv, _ := asyncServer(1 << 30)
	defer srv.Close()
	start := time.Now()
	r := runYAML(t, srv.URL, fmt.Sprintf(pollFlow, "150ms"))
	w := step(r, "wait")
	if w.Status != report.Failed || w.ErrorKind != KindTimeout {
		t.Fatalf("%+v", w)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("poll overran its timeout")
	}
	if w.Poll.Satisfied || w.Poll.Attempts < 2 || w.Response == nil || len(w.Checks) == 0 || w.Checks[0].Passed {
		t.Fatalf("evidence missing: %+v %+v", w.Poll, w.Checks)
	}
	if step(r, "use").Status != report.Skipped {
		t.Fatal("dependent must be skipped")
	}
}

func TestPollRetriesTransientErrorsButReportsInfraIfNeverAnswered(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 { // drop the connection twice, then answer
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		fmt.Fprint(w, `{"status":"DONE","id":1}`)
	}))
	defer srv.Close()
	r := runYAML(t, srv.URL, fmt.Sprintf(pollFlow, "5s"))
	if w := step(r, "wait"); w.Status != report.Passed || w.Poll.Attempts != 3 || w.Poll.History[0].Error == "" {
		t.Fatalf("%+v %+v", w, w.Poll)
	}

	dead := httptest.NewServer(nil)
	url := dead.URL
	dead.Close()
	r = runYAML(t, url, fmt.Sprintf(pollFlow, "150ms"))
	if w := step(r, "wait"); w.ErrorKind != KindInfra {
		t.Fatalf("never answered: kind %q, want infrastructure", w.ErrorKind)
	}
}

func TestPollSlowAttemptCannotOutliveWindow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
	}))
	defer srv.Close()
	start := time.Now()
	r := runYAML(t, srv.URL, fmt.Sprintf(pollFlow, "200ms"))
	if time.Since(start) > 1500*time.Millisecond || step(r, "wait").Status != report.Failed {
		t.Fatalf("took %s", time.Since(start))
	}
}

func TestPollBrokenUntilFailsFastAsSpec(t *testing.T) {
	srv, calls := asyncServer(1)
	defer srv.Close()
	r := runYAML(t, srv.URL, `
version: 1
name: t
nodes:
  - id: wait
    kind: poll
    poll:
      timeout: 5s
      request: {url: "{{ .env.base }}/"}
      until: [{path: nodes.wait.response.raw, matches: "("}]
`)
	if w := step(r, "wait"); w.ErrorKind != KindSpec || *calls != 1 {
		t.Fatalf("kind=%q calls=%d", w.ErrorKind, *calls)
	}
}

func pollContractFlow(mode string) string {
	return `
version: 1
name: t
nodes:
  - id: wait
    kind: poll
    contract: ` + mode + `
    poll:
      interval: 10ms
      timeout: 5s
      request: {url: "{{ .env.base }}/users/7"}
      until:
        - {path: nodes.wait.response.body.name, exists: true}
`
}

// pollHandler answers `pending` until the n-th call, then `final`.
func pollHandler(n int32, pending, final string, pendingStatus int) http.HandlerFunc {
	var calls int32
	return func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) < n {
			w.WriteHeader(pendingStatus)
			fmt.Fprint(w, pending)
			return
		}
		fmt.Fprint(w, final)
	}
}

func TestPollContractIgnoresIntermediateShapeAndChecksSatisfyingAttempt(t *testing.T) {
	// the PENDING body violates the schema, but only the final one counts
	r := runContract(t, pollContractFlow("api"), pollHandler(3, `{"status":"PENDING"}`, `{"id":7,"name":"x"}`, 200))
	w := step(r, "wait")
	if w.Status != report.Passed || w.Poll.Attempts != 3 || w.Contract == nil || len(w.Contract.Violations) != 0 || w.Contract.Mode != "fail" {
		t.Fatalf("%+v %+v", w, w.Contract)
	}
}

func TestPollContractViolationIsContractKindNotInfraOrTimeout(t *testing.T) {
	start := time.Now()
	r := runContract(t, pollContractFlow("api"), pollHandler(2, `{"status":"PENDING"}`, `{"id":"7","name":"x"}`, 200))
	w := step(r, "wait")
	if w.Status != report.Failed || w.ErrorKind != KindContract || len(w.Contract.Violations) == 0 {
		t.Fatalf("%+v %+v", w, w.Contract)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("a satisfied poll must not wait out its timeout")
	}
}

func TestPollContractUndocumentedStatusOnEarlierAttemptIsKept(t *testing.T) {
	r := runContract(t, pollContractFlow("api"), pollHandler(3, `{}`, `{"id":7,"name":"x"}`, 202))
	w := step(r, "wait")
	if w.ErrorKind != KindContract || len(w.Contract.Violations) != 1 || !undocumented(w.Contract.Violations[0]) {
		t.Fatalf("%+v %+v", w, w.Contract)
	}
}

func TestPollContractWarnModeRecordsButPasses(t *testing.T) {
	r := runContract(t, pollContractFlow("{source: api, on_violation: warn}"), pollHandler(2, `{}`, `{"id":"7","name":"x"}`, 200))
	w := step(r, "wait")
	if w.Status != report.Passed || w.Contract.Mode != "warn" || len(w.Contract.Violations) != 1 {
		t.Fatalf("%+v %+v", w, w.Contract)
	}
}

func TestPollContractUnknownOperationIsNeverSent(t *testing.T) {
	var hits int32
	r := runContract(t, `
version: 1
name: t
nodes:
  - id: wait
    kind: poll
    contract: api
    poll:
      interval: 10ms
      timeout: 5s
      request: {url: "{{ .env.base }}/nope"}
      until: [{path: nodes.wait.response.status, equals: 200}]
`, func(w http.ResponseWriter, _ *http.Request) { atomic.AddInt32(&hits, 1) })
	if w := step(r, "wait"); w.ErrorKind != KindContract || atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("hits=%d %+v", hits, w)
	}
}

func TestNextDelay(t *testing.T) {
	i := 100 * time.Millisecond
	if nextDelay(nil, i, 5, 0.5) != i {
		t.Fatal("no backoff means a fixed interval")
	}
	b := &flow.Backoff{Max: "500ms"} // factor defaults to 2
	for n, want := range []time.Duration{100, 200, 400, 500, 500} {
		if got := nextDelay(b, i, n+1, 0.5); got != want*time.Millisecond {
			t.Fatalf("attempt %d: %v want %v", n+1, got, want*time.Millisecond)
		}
	}
	j := &flow.Backoff{Factor: 1, Jitter: 0.2}
	if lo, hi := nextDelay(j, i, 1, 0), nextDelay(j, i, 1, 0.999); lo != 80*time.Millisecond || hi < 119*time.Millisecond || hi > 120*time.Millisecond {
		t.Fatalf("jitter range: %v..%v", lo, hi)
	}
}

func TestPollBackoffRecordsDelaysAndStillSucceeds(t *testing.T) {
	srv, _ := asyncServer(4)
	defer srv.Close()
	r := runYAML(t, srv.URL, strings.Replace(fmt.Sprintf(pollFlow, "5s"), "      request:", "      backoff: {factor: 2}\n      request:", 1))
	w := step(r, "wait")
	if w.Status != report.Passed || w.Poll.Attempts != 4 {
		t.Fatalf("%+v %+v", w, w.Poll)
	}
	h := w.Poll.History
	if h[0].DelayMs != 20 || h[1].DelayMs != 40 || h[2].DelayMs != 80 || h[3].DelayMs != 0 {
		t.Fatalf("delays: %+v", h)
	}
}

func TestPollBackoffLeavesRoomForALastAttempt(t *testing.T) {
	srv, calls := asyncServer(1 << 30)
	defer srv.Close()
	y := strings.Replace(fmt.Sprintf(pollFlow, "300ms"), "      request:", "      backoff: {factor: 10}\n      request:", 1)
	start := time.Now()
	r := runYAML(t, srv.URL, y)
	if w := step(r, "wait"); w.ErrorKind != KindTimeout || time.Since(start) > time.Second || *calls < 2 {
		t.Fatalf("calls=%d %+v", *calls, w)
	}
}

func TestPollContractBadRequestBodyIsNeverSent(t *testing.T) {
	var hits int32
	r := runContract(t, `
version: 1
name: t
nodes:
  - id: wait
    kind: poll
    contract: api
    poll:
      interval: 10ms
      timeout: 5s
      request: {method: POST, url: "{{ .env.base }}/users", body: {name: 5, junk: true}}
      until: [{path: nodes.wait.response.status, equals: 201}]
`, func(w http.ResponseWriter, _ *http.Request) { atomic.AddInt32(&hits, 1) })
	w := step(r, "wait")
	if w.ErrorKind != KindContract || atomic.LoadInt32(&hits) != 0 || w.Response != nil || len(w.Contract.Violations) != 2 {
		t.Fatalf("hits=%d %+v %+v", hits, w, w.Contract)
	}
	if w.Poll.Attempts != 1 {
		t.Fatalf("a request that can never be valid must not be retried: %d attempts", w.Poll.Attempts)
	}
}

// A service that answered earlier but whose last attempt was cut short by the poll's own
// deadline never converged: that is a timeout (exit 1), not an infrastructure failure.
func TestPollStarvedFinalAttemptIsTimeoutNotInfrastructure(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) >= 3 {
			time.Sleep(3 * time.Second) // outlasts the window
		}
		fmt.Fprint(w, `{"status":"PENDING"}`)
	}))
	defer srv.Close()
	r := runYAML(t, srv.URL, fmt.Sprintf(pollFlow, "1200ms"))
	if w := step(r, "wait"); w.ErrorKind != KindTimeout || w.Poll.Attempts < 3 {
		t.Fatalf("want timeout after answered attempts, got kind=%q attempts=%d: %s", w.ErrorKind, w.Poll.Attempts, w.Error)
	}
}

// ...but a failure of an attempt that had its full budget is still infrastructure.
func TestPollFullBudgetFailureIsInfrastructure(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) > 1 {
			time.Sleep(2 * time.Second) // the service stops answering
		}
		fmt.Fprint(w, `{"status":"PENDING"}`)
	}))
	defer srv.Close()
	r := runYAML(t, srv.URL, `
version: 1
name: t
nodes:
  - id: wait
    kind: poll
    poll:
      interval: 50ms
      timeout: 1500ms
      request: {url: "{{ .env.base }}/", timeout: 100ms}
      until: [{path: nodes.wait.response.body.status, equals: DONE}]
`)
	if w := step(r, "wait"); w.ErrorKind != KindInfra {
		t.Fatalf("want infrastructure, got kind=%q: %s", w.ErrorKind, w.Error)
	}
}

// When a contract poll never converges, the last attempt's "still pending" shape is not
// drift, but an undocumented status seen on an earlier attempt still is.
func TestPollContractTimeoutKeepsUndocumentedStatusAndDropsIntermediateShape(t *testing.T) {
	var calls int32
	handler := func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(202) // not documented in the contract
			fmt.Fprint(w, `{}`)
			return
		}
		if n%2 == 0 { // 200, but the shape changes between attempts
			fmt.Fprint(w, `{"status":"PENDING"}`) // lacks id and name
		} else {
			fmt.Fprint(w, `{"id":7,"status":"PENDING"}`) // lacks name
		}
	}
	flowYAML := strings.Replace(pollContractFlow("api"), "timeout: 5s", "timeout: 400ms", 1)
	r := runContract(t, flowYAML, handler)
	w := step(r, "wait")
	if w.ErrorKind != KindTimeout {
		t.Fatalf("kind=%q: %s", w.ErrorKind, w.Error)
	}
	if len(w.Contract.Violations) != 1 || !undocumented(w.Contract.Violations[0]) {
		t.Fatalf("want only the undocumented 202, got %+v", w.Contract.Violations)
	}
	if r.Drift == nil || r.Drift.Failing != 1 {
		t.Fatalf("drift summary should hold just that finding: %+v", r.Drift)
	}
}

func TestMergeViolationsKeysOnWherePathAndMessage(t *testing.T) {
	same := report.Violation{Where: "response", Path: "$.a", Message: "X"}
	got := mergeViolations(
		[]report.Violation{same},
		[]report.Violation{
			same, // exact duplicate: dropped
			{Where: "response", Path: "$.b", Message: "X"}, // same text, other path: kept
			{Where: "request", Path: "$.a", Message: "X"},  // same text, other place: kept
			{Where: "response", Path: "$.a", Message: "Y"}, // other text: kept
		})
	if len(got) != 4 {
		t.Fatalf("want 4 distinct findings, got %d: %+v", len(got), got)
	}
}

// A fast, real failure after a good answer is infrastructure even when the poll window is
// shorter than the default request timeout: only a failure caused by the window running
// out says nothing about the service.
func TestPollFastFailureAfterAnAnswerIsInfrastructureEvenInAShortWindow(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) > 1 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close() // the service dies: an instant connection error, not a slow one
			return
		}
		fmt.Fprint(w, `{"status":"PENDING"}`)
	}))
	defer srv.Close()
	// no request timeout set: the default is 30s, longer than this 2s window
	r := runYAML(t, srv.URL, fmt.Sprintf(pollFlow, "2s"))
	if w := step(r, "wait"); w.ErrorKind != KindInfra {
		t.Fatalf("want infrastructure, got kind=%q: %s", w.ErrorKind, w.Error)
	}
}

// A schema violation present on every attempt is not an intermediate shape: it is real
// drift and must survive a poll that never converges. Shapes that vary between attempts
// are still dropped.
func TestPollContractKeepsPersistentViolationsDropsVaryingOnes(t *testing.T) {
	flowYAML := strings.Replace(pollContractFlow("api"), "timeout: 5s", "timeout: 400ms", 1)

	persistent := runContract(t, flowYAML, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":"seven"}`) // id has the wrong type and name is missing, on every attempt
	})
	p := step(persistent, "wait")
	if p.ErrorKind != KindTimeout || p.Contract == nil {
		t.Fatalf("setup: %+v", p)
	}
	found := false
	for _, v := range p.Contract.Violations {
		if v.Where == "response" && strings.Contains(v.Path, "id") {
			found = true
		}
	}
	if !found || persistent.Drift == nil || persistent.Drift.Failing == 0 {
		t.Fatalf("a violation present on every attempt is real drift and must be kept: %+v", p.Contract.Violations)
	}

	var calls int32
	varying := runContract(t, flowYAML, func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1)%2 == 1 {
			fmt.Fprint(w, `{"status":"P1"}`) // lacks id and name
		} else {
			fmt.Fprint(w, `{"id":7,"status":"P2"}`) // lacks name
		}
	})
	if v := step(varying, "wait").Contract.Violations; len(v) != 0 {
		t.Fatalf("shapes that change between attempts are intermediate, not drift: %+v", v)
	}
}

// With a single answer a pending shape cannot be told from drift, so it is not reported as
// drift, but the poll says so instead of dropping it silently.
func TestPollContractSingleAnswerIsNotSilent(t *testing.T) {
	var calls int32
	handler := func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			fmt.Fprint(w, `{"id":"seven"}`) // an answer with a contract violation
			return
		}
		time.Sleep(2 * time.Second) // then the service stops answering within the window
	}
	flowYAML := strings.Replace(pollContractFlow("api"), "timeout: 5s", "timeout: 600ms", 1)
	flowYAML = strings.Replace(flowYAML, "request: {url:", "request: {timeout: 100ms, url:", 1)
	r := runContract(t, flowYAML, handler)
	w := step(r, "wait")
	if w.Contract == nil || len(w.Contract.Notes) == 0 || !strings.Contains(strings.Join(w.Contract.Notes, " "), "only answered attempt") {
		t.Fatalf("the held-back finding must be visible as a note: %+v", w.Contract)
	}
}
