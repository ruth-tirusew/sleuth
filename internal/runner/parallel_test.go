package runner

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"sleuth/internal/flow"
	"sleuth/internal/report"
)

func runParallel(t *testing.T, srvURL, yaml string, n int) *report.Run {
	t.Helper()
	p := t.TempDir() + "/f.yaml"
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := flow.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := Run(f, &EnvConfig{Vars: map[string]string{"base": srvURL}}, Options{Parallel: n})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func slowServer(d time.Duration, peak *int32) *httptest.Server {
	var cur int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := atomic.AddInt32(&cur, 1)
		for {
			p := atomic.LoadInt32(peak)
			if c <= p || atomic.CompareAndSwapInt32(peak, p, c) {
				break
			}
		}
		time.Sleep(d)
		atomic.AddInt32(&cur, -1)
		fmt.Fprint(w, `{"ok":true}`)
	}))
}

const diamond = `
version: 1
name: t
nodes:
  - {id: a, kind: request, request: {url: "{{ .env.base }}/a"}}
  - {id: b, kind: request, request: {url: "{{ .env.base }}/b"}}
  - {id: c, kind: request, request: {url: "{{ .env.base }}/c"}}
  - id: join
    kind: transform
    needs: [a, b, c]
    transform: {set: {x: "{{ .nodes.a.response.status }}"}}
`

func TestParallelOverlapsIndependentNodes(t *testing.T) {
	var peak int32
	srv := slowServer(200*time.Millisecond, &peak)
	defer srv.Close()

	start := time.Now()
	r := runParallel(t, srv.URL, diamond, 3)
	if el := time.Since(start); el > 500*time.Millisecond || atomic.LoadInt32(&peak) != 3 {
		t.Fatalf("elapsed %v, peak concurrency %d", el, peak)
	}
	if r.Status != report.Passed || r.Parallel != 3 || step(r, "join").Status != report.Passed {
		t.Fatalf("%+v", r)
	}

	atomic.StoreInt32(&peak, 0)
	start = time.Now()
	r = runParallel(t, srv.URL, diamond, 1)
	if el := time.Since(start); el < 600*time.Millisecond || atomic.LoadInt32(&peak) != 1 || r.Parallel != 0 {
		t.Fatalf("serial must not overlap: elapsed %v, peak %d, parallel %d", el, peak, r.Parallel)
	}
}

func TestParallelRespectsLimit(t *testing.T) {
	var peak int32
	srv := slowServer(100*time.Millisecond, &peak)
	defer srv.Close()
	runParallel(t, srv.URL, diamond, 2)
	if p := atomic.LoadInt32(&peak); p != 2 {
		t.Fatalf("peak %d, want 2", p)
	}
}

func TestParallelEvidenceIsInFlowOrderAndFailureSkipsOnlyDependents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			time.Sleep(50 * time.Millisecond) // finishes after "good", so completion order differs
			w.WriteHeader(500)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	y := `
version: 1
name: t
nodes:
  - {id: bad, kind: request, request: {url: "{{ .env.base }}/bad", extract: {x: body.missing}}}
  - {id: good, kind: request, request: {url: "{{ .env.base }}/good"}}
  - {id: after_bad, kind: transform, needs: [bad], transform: {set: {x: 1}}}
  - {id: after_good, kind: transform, needs: [good], transform: {set: {x: 1}}}
  - {id: after_after, kind: transform, needs: [after_bad], transform: {set: {x: 1}}}
`
	for i := 0; i < 20; i++ {
		r := runParallel(t, srv.URL, y, 4)
		var ids []string
		for _, s := range r.Steps {
			ids = append(ids, s.ID+":"+s.Status)
		}
		got := strings.Join(ids, " ")
		want := "bad:failed good:passed after_bad:skipped after_good:passed after_after:skipped"
		if got != want || r.Status != report.Failed {
			t.Fatalf("run %d:\n got %s\nwant %s", i, got, want)
		}
	}
}

func TestParallelSerialOrderIsUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) }))
	defer srv.Close()
	r := runParallel(t, srv.URL, diamond, 1)
	for i, want := range []string{"a", "b", "c", "join"} {
		if r.Steps[i].ID != want || (i > 0 && r.Steps[i].StartedAt.Before(r.Steps[i-1].StartedAt)) {
			t.Fatalf("%+v", r.Steps)
		}
	}
}

// A wide flow with shared state: many nodes read and write the context at once. Run with -race.
func TestParallelWideFlowSharesContextSafely(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"v":1}`) }))
	defer srv.Close()
	var b strings.Builder
	b.WriteString("version: 1\nname: t\nnodes:\n  - {id: root, kind: request, request: {url: \"{{ .env.base }}/r\", extract: {v: body.v}}}\n")
	var needs []string
	for i := 0; i < 24; i++ {
		fmt.Fprintf(&b, "  - id: n%d\n    kind: request\n    request: {url: \"{{ .env.base }}/x?v={{ .nodes.root.out.v }}&i=%d\"}\n", i, i)
		fmt.Fprintf(&b, "  - id: t%d\n    kind: transform\n    transform: {set: {v: \"{{ .nodes.n%d.response.status }}\", e: \"{{ .env.base }}\"}}\n", i, i)
		needs = append(needs, fmt.Sprintf("t%d", i))
	}
	fmt.Fprintf(&b, "  - {id: end, kind: transform, needs: [%s], transform: {set: {ok: 1}}}\n", strings.Join(needs, ", "))
	r := runParallel(t, srv.URL, b.String(), 8)
	if r.Status != report.Passed || len(r.Steps) != 50 {
		t.Fatalf("%s %d steps", r.Status, len(r.Steps))
	}
}

func TestParallelPollsRunSideBySide(t *testing.T) {
	srv1, _ := asyncServer(5)
	srv2, _ := asyncServer(5)
	defer srv1.Close()
	defer srv2.Close()
	y := strings.NewReplacer("%s", "5s").Replace(`
version: 1
name: t
nodes:
  - id: p1
    kind: poll
    poll:
      interval: 40ms
      timeout: 5s
      request: {url: "` + srv1.URL + `/"}
      until: [{path: nodes.p1.response.body.status, equals: DONE}]
  - id: p2
    kind: poll
    poll:
      interval: 40ms
      timeout: 5s
      request: {url: "` + srv2.URL + `/"}
      until: [{path: nodes.p2.response.body.status, equals: DONE}]
`)
	start := time.Now()
	r := runParallel(t, "", y, 2)
	if r.Status != report.Passed || time.Since(start) > 350*time.Millisecond {
		t.Fatalf("%s in %v", r.Status, time.Since(start))
	}
}
