package runner

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"assay/internal/flow"
	"assay/internal/report"
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
