package runner

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"sleuth/internal/flow"
	"sleuth/internal/report"
)

func runWithTimeout(t *testing.T, srvURL, yaml string, timeout time.Duration, parallel int) *report.Run {
	t.Helper()
	p := t.TempDir() + "/f.yaml"
	os.WriteFile(p, []byte(yaml), 0o644)
	f, err := flow.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := Run(f, &EnvConfig{Vars: map[string]string{"base": srvURL}}, Options{Timeout: timeout, Parallel: parallel})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

const slowFlow = `
version: 1
name: t
nodes:
  - {id: hang, kind: request, request: {url: "{{ .env.base }}/hang"}}
  - {id: after, kind: transform, needs: [hang], transform: {set: {x: 1}}}
  - id: wait
    kind: poll
    poll:
      interval: 50ms
      timeout: 10s
      request: {url: "{{ .env.base }}/pending"}
      until: [{path: nodes.wait.response.body.done, equals: true}]
  - {id: last, kind: transform, transform: {set: {y: 2}}}
`

// A request with the default 30s timeout, and a poll with a 10s window, both stop when the
// run's own 300ms timeout passes; nothing new starts after it.
func TestRunTimeoutStopsWorkInFlight(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hang" {
			<-r.Context().Done()
			return
		}
		w.Write([]byte(`{"done":false}`))
	}))
	defer srv.Close()
	for _, p := range []int{1, 4} {
		start := time.Now()
		r := runWithTimeout(t, srv.URL, slowFlow, 300*time.Millisecond, p)
		if took := time.Since(start); took > 2*time.Second {
			t.Fatalf("parallel=%d: the run took %s; the timeout must stop work in flight", p, took)
		}
		if !r.TimedOut || r.Status != report.Failed || r.Timeout != "300ms" {
			t.Fatalf("parallel=%d: %+v", p, r)
		}
		h := step(r, "hang")
		if h.ErrorKind != KindInfra || !strings.Contains(h.Error, "run's timeout") {
			t.Errorf("parallel=%d: hang: %+v", p, h)
		}
		if a := step(r, "after"); a.Status != report.Skipped {
			t.Errorf("parallel=%d: after: %+v", p, a)
		}
		// with -parallel 1 the poll and the last node never start; with 4 the poll was running
		w, last := step(r, "wait"), step(r, "last")
		if p == 1 && (w.Status != report.Skipped || !strings.Contains(w.Error, "not started")) {
			t.Errorf("parallel=1: wait should not have started: %+v", w)
		}
		if p == 4 && (w.ErrorKind != KindInfra || !strings.Contains(w.Error, "run's timeout")) {
			t.Errorf("parallel=4: the running poll should be stopped: %+v", w)
		}
		if p == 4 && last.Status != report.Passed {
			t.Errorf("parallel=4: a quick independent node still completes: %+v", last)
		}
	}
}

func TestNoTimeoutMeansNoLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	r := runWithTimeout(t, srv.URL, "version: 1\nname: t\nnodes:\n  - {id: a, kind: request, request: {url: \"{{ .env.base }}/\"}}\n", 0, 1)
	if r.TimedOut || r.Status != report.Passed {
		t.Fatalf("%+v", r)
	}
}
