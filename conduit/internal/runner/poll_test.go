package runner

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"conduit/internal/report"
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
