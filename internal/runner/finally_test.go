package runner

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"sleuth/internal/report"
)

// cleanupServer records the requests it gets, in order.
func cleanupServer(failDelete bool) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		switch {
		case r.URL.Path == "/slow":
			time.Sleep(2 * time.Second)
		case r.Method == "DELETE" && failDelete:
			w.WriteHeader(500)
		case r.URL.Path == "/boom":
			w.WriteHeader(500)
		}
		fmt.Fprint(w, `{"id":"42"}`)
	}))
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string{}, seen...) }
}

const finallyFlow = `version: 1
name: top
nodes:
  - {id: make, kind: request, request: {method: POST, url: "{{ .env.base }}/things"}}
  - id: check
    kind: assertion
    assert: [{path: nodes.make.response.body.id, equals: "nope"}]
finally:
  - {id: drop, kind: request, request: {method: DELETE, url: "{{ .env.base }}/things/{{ .nodes.make.response.body.id }}"}}
`

func TestFinallyRunsAfterAFailedMainNode(t *testing.T) {
	srv, seen := cleanupServer(false)
	defer srv.Close()
	for _, workers := range []int{1, 4} {
		r := runTree(t, srv.URL, map[string]string{"top.yaml": finallyFlow}, Options{Parallel: workers})
		if r.Status != report.Failed {
			t.Fatalf("status %s", r.Status)
		}
		drop := step(r, "drop")
		if drop.Status != report.Passed || !drop.Cleanup {
			t.Fatalf("cleanup step not run: %+v", drop)
		}
		if r.Steps[len(r.Steps)-1].ID != "drop" {
			t.Fatalf("cleanup should come last: %+v", r.Steps)
		}
	}
	if got := strings.Join(seen(), ","); !strings.Contains(got, "DELETE /things/42") {
		t.Fatalf("server saw %s", got)
	}
}

func TestFinallySkippedWhenWhatItCleansNeverHappened(t *testing.T) {
	srv, seen := cleanupServer(false)
	defer srv.Close()
	flow := `version: 1
name: top
nodes:
  - {id: make, kind: request, request: {method: POST, url: "{{ .env.base }}/boom"}}
  - {id: made, kind: assertion, assert: [{path: nodes.make.response.status, equals: 201}]}
finally:
  - {id: drop, kind: request, needs: [made], request: {method: DELETE, url: "{{ .env.base }}/things/1"}}
`
	r := runTree(t, srv.URL, map[string]string{"top.yaml": flow}, Options{})
	drop := step(r, "drop")
	if drop.Status != report.Skipped || !strings.Contains(drop.Error, "nothing to clean up") {
		t.Fatalf("want skipped, got %+v", drop)
	}
	for _, s := range seen() {
		if strings.HasPrefix(s, "DELETE") {
			t.Fatalf("cleanup request was sent: %v", seen())
		}
	}
}

func TestFailedCleanupFailsTheRunWithItsOwnKind(t *testing.T) {
	srv, _ := cleanupServer(true)
	defer srv.Close()
	// the main flow passes; only the delete's own check fails (a 500 is data, not an error)
	flow := `version: 1
name: top
nodes:
  - {id: make, kind: request, request: {method: POST, url: "{{ .env.base }}/things"}}
finally:
  - {id: drop, kind: request, request: {method: DELETE, url: "{{ .env.base }}/things/{{ .nodes.make.response.body.id }}"}}
  - {id: dropped, kind: assertion, assert: [{path: nodes.drop.response.status, equals: 204}]}
`
	r := runTree(t, srv.URL, map[string]string{"top.yaml": flow}, Options{})
	if r.Status != report.Failed {
		t.Fatalf("a failed cleanup must fail the run, got %s", r.Status)
	}
	d := step(r, "dropped")
	if d.Status != report.Failed || d.ErrorKind != KindCleanup || !d.Cleanup {
		t.Fatalf("want a failed cleanup step of kind cleanup, got %+v", d)
	}
	if step(r, "make").Status != report.Passed {
		t.Fatalf("main node should have passed")
	}
}

func TestCleanupStillRunsAfterTheRunTimeout(t *testing.T) {
	srv, seen := cleanupServer(false)
	defer srv.Close()
	flow := `version: 1
name: top
nodes:
  - {id: make, kind: request, request: {method: POST, url: "{{ .env.base }}/things"}}
  - {id: hang, kind: request, needs: [make], request: {url: "{{ .env.base }}/slow"}}
finally:
  - {id: drop, kind: request, request: {method: DELETE, url: "{{ .env.base }}/things/{{ .nodes.make.response.body.id }}"}}
`
	r := runTree(t, srv.URL, map[string]string{"top.yaml": flow}, Options{Timeout: 300 * time.Millisecond, CleanupGrace: 5 * time.Second})
	if !r.TimedOut {
		t.Fatalf("run should have timed out")
	}
	if drop := step(r, "drop"); drop.Status != report.Passed {
		t.Fatalf("cleanup must run after the timeout: %+v", drop)
	}
	if got := strings.Join(seen(), ","); !strings.Contains(got, "DELETE /things/42") {
		t.Fatalf("server saw %s", got)
	}
}

func TestSubflowFinallyRunsWhenTheSubflowEnds(t *testing.T) {
	srv, seen := cleanupServer(false)
	defer srv.Close()
	r := runTree(t, srv.URL, map[string]string{
		"top.yaml":  "version: 1\nname: top\nnodes:\n  - {id: sub, kind: flow, flow: _sub.yaml}\n",
		"_sub.yaml": finallyFlow,
	}, Options{})
	sub := step(r, "sub")
	if sub.Status != report.Failed {
		t.Fatalf("subflow's main node failed, so the node must fail: %+v", sub)
	}
	drop := child(sub, "drop")
	if drop.Status != report.Passed || !drop.Cleanup {
		t.Fatalf("subflow cleanup did not run: %+v", sub.Children)
	}
	if got := strings.Join(seen(), ","); !strings.Contains(got, "DELETE /things/42") {
		t.Fatalf("server saw %s", got)
	}
}

func TestFailedSubflowCleanupDoesNotBlockTheCaller(t *testing.T) {
	srv, _ := cleanupServer(true)
	defer srv.Close()
	sub := strings.Replace(finallyFlow, `equals: "nope"`, `equals: "42"`, 1) + `  - {id: dropped, kind: assertion, assert: [{path: nodes.drop.response.status, equals: 204}]}
`
	r := runTree(t, srv.URL, map[string]string{
		"top.yaml":  "version: 1\nname: top\nnodes:\n  - {id: sub, kind: flow, flow: _sub.yaml}\n  - {id: after, kind: assertion, needs: [sub], assert: [{path: nodes.sub.out, exists: false}]}\n",
		"_sub.yaml": sub,
	}, Options{})
	if step(r, "sub").Status != report.Passed || step(r, "after").Status == report.Skipped {
		t.Fatalf("a cleanup failure must not skip the caller's later nodes: %+v", r.Steps)
	}
	if r.Status != report.Failed {
		t.Fatalf("but the run must still fail, got %s", r.Status)
	}
	if d := child(step(r, "sub"), "dropped"); d.ErrorKind != KindCleanup {
		t.Fatalf("got %+v", d)
	}
}
