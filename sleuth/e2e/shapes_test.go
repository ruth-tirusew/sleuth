package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sleuth/internal/shape"
)

// The scenario this exists for: a team has been testing against a service, and mid-testing
// someone changes a response. The endpoint is not in the service's OpenAPI document, and the
// flow's own assertions do not look at the changed fields, so every step still passes. Only a
// snapshot of the response shapes notices.
func TestBehaviouralSnapshots(t *testing.T) {
	for _, p := range []int{1, 4} {
		p := p
		t.Run(fmt.Sprintf("parallel=%d", p), func(t *testing.T) {
			base := startService(t)
			dir := workdir(t)
			snap := filepath.Join(dir, "snapshots", "shapes.json")
			opts := func(args ...string) runOptions { return runOptions{baseURL: base, args: args} }
			// every run uses the same working directory so the snapshot path is shared
			run := func(args ...string) *result { return runFlowIn(t, dir, "shapes", p, opts(args...)) }
			setVersion := func(v int) {
				resp, err := http.Post(fmt.Sprintf("%s/admin/profile-version?v=%d", base, v), "application/json", nil)
				if err != nil || resp.StatusCode != 200 {
					t.Fatalf("could not switch the service to version %d: %v", v, err)
				}
				resp.Body.Close()
			}
			load := func(path string) *shape.Snapshot {
				t.Helper()
				s, err := shape.Load(path)
				if err != nil {
					t.Fatal(err)
				}
				return s
			}

			// 1. record from a passing run
			r := run("-record-shapes", snap)
			expectExit(t, r, 0)
			if !strings.Contains(r.stdout, "recorded 3 step(s)") {
				t.Errorf("%s", r.stdout)
			}
			s1 := load(snap)
			if s1.Flow != "shapes" || s1.Recorded.Runs != 1 || s1.Recorded.Tool == "" || s1.FlowSHA == "" || time.Since(s1.Recorded.At) > time.Minute {
				t.Errorf("provenance: %+v", s1.Recorded)
			}
			for _, id := range []string{"health", "profile", "orders"} {
				if e, ok := s1.Steps[id]; !ok || e.Of != "response" || e.Status != 200 {
					t.Errorf("step %s: %+v", id, e)
				}
			}
			raw, _ := os.ReadFile(snap)
			for _, value := range []string{"alice", "Addis", "gift", "100.5", "1000"} {
				if strings.Contains(string(raw), value) {
					t.Errorf("a snapshot must hold no values, but contains %q", value)
				}
			}
			if o := s1.Steps["orders"].Shape.Items; o == nil || o.Fields["note"].Optional == false || o.Fields["id"].Optional {
				t.Errorf("an array's items teach which fields are optional: %+v", s1.Steps["orders"].Shape)
			}

			// 2. nothing changed: no drift, even with the strictest setting
			r = run("-shapes", snap, "-shape-drift-fail", "any")
			expectExit(t, r, 0)
			if b := r.run.Drift.Behaviour; b == nil || len(b.Changes) != 0 || b.Breaking != 0 {
				t.Fatalf("%+v", b)
			}
			if !strings.Contains(r.stdout, "Behaviour drift: 0 breaking") || strings.Contains(r.stdout, "Contract drift") {
				t.Errorf("the report should have a behaviour section and no contract section:\n%s", r.stdout)
			}

			// 3. someone changes the DTO. Every step still passes; only the snapshot notices.
			setVersion(2)
			r = run("-shapes", snap)
			expectExit(t, r, 0) // report-only by default
			expectSteps(t, r, map[string]string{"health": "passed", "profile": "passed", "orders": "passed", "ok": "passed"})
			b := r.run.Drift.Behaviour
			if b == nil || b.Breaking < 4 || b.Info < 1 || b.Failing {
				t.Fatalf("behaviour drift: %+v", b)
			}
			got := map[string]string{}
			for _, c := range b.Changes {
				got[c.Step+" "+c.Path] = c.Severity
			}
			for _, want := range []string{"profile $.id", "profile $.balance", "profile $.tags", "profile $.address.zip"} {
				if got[want] != shape.Breaking {
					t.Errorf("expected a breaking change at %q, got %v", want, got)
				}
			}
			if got["profile $.nick"] != shape.Info {
				t.Errorf("a new field is compatible: %v", got)
			}
			if _, drifted := got["orders $.note"]; drifted || got["health $"] != "" {
				t.Errorf("the unchanged steps must not report drift: %v", got)
			}
			if !strings.Contains(r.stdout, "Behaviour drift:") || !strings.Contains(r.stdout, "type changed from integer to string") {
				t.Errorf("%s", r.stdout)
			}
			expectExit(t, run("-shapes", snap, "-shape-drift-fail", "breaking"), 1)
			expectExit(t, run("-shapes", snap, "-shape-drift-fail", "any"), 1)

			// 4. replacing the snapshot is a decision: it shows what is accepted and keeps the old one
			before, _ := os.ReadFile(snap)
			r = run("-record-shapes", snap)
			expectExit(t, r, 2)
			if !strings.Contains(r.stderr, "nothing was written") || !strings.Contains(r.stderr, "$.balance") || !strings.Contains(r.stderr, "-force") {
				t.Errorf("the refusal should list the changes and name -force:\n%s", r.stderr)
			}
			if after, _ := os.ReadFile(snap); string(after) != string(before) {
				t.Error("a refused recording must not touch the snapshot")
			}
			r = run("-record-shapes", snap, "-force")
			expectExit(t, r, 0)
			if !strings.Contains(r.stdout, "accepting") || !strings.Contains(r.stdout, ".prev") {
				t.Errorf("%s", r.stdout)
			}
			prev := load(snap + ".prev")
			if !contains(prev.Steps["profile"].Shape.Fields["id"].Type, "integer") || !contains(load(snap).Steps["profile"].Shape.Fields["id"].Type, "string") {
				t.Error("the old snapshot should be kept as .prev, and the new one should describe the new response")
			}
			expectExit(t, run("-shapes", snap, "-shape-drift-fail", "any"), 0) // now it matches

			// 5. -merge widens instead of replacing
			merged := filepath.Join(dir, "snapshots", "merged.json")
			setVersion(1)
			expectExit(t, run("-record-shapes", merged), 0)
			setVersion(2)
			r = run("-record-shapes", merged, "-merge")
			expectExit(t, r, 0)
			m := load(merged)
			prof := m.Steps["profile"]
			if m.Recorded.Runs != 2 || prof.Observations != 2 || len(prof.Shape.Fields["id"].Type) != 2 || !prof.Shape.Fields["balance"].Optional || !prof.Shape.Fields["nick"].Optional {
				t.Errorf("merging two runs should union types and make one-sided fields optional: %+v", m.Recorded)
			}
			// both versions are now within what the snapshot describes
			expectExit(t, run("-shapes", merged, "-shape-drift-fail", "breaking"), 0)
			setVersion(1)
			expectExit(t, run("-shapes", merged, "-shape-drift-fail", "breaking"), 0)

			// 6. a different HTTP status is a different response
			setVersion(3)
			r = run("-shapes", merged, "-shape-drift-fail", "breaking")
			expectExit(t, r, 1)
			if !strings.Contains(r.stdout, "HTTP status changed from 200 to 404") {
				t.Errorf("%s", r.stdout)
			}
			setVersion(1)
		})
	}
}

func TestSnapshotsAreNeverTakenFromAFailingRunOrReadFromTheWrongFile(t *testing.T) {
	dir := workdir(t)
	base := startService(t)
	snap := filepath.Join(dir, "never.json")

	// a failing run is not recorded (and its own exit code stands)
	r := runFlowIn(t, dir, "assertion-fail", 1, runOptions{baseURL: base, args: []string{"-record-shapes", snap}})
	expectExit(t, r, 1)
	if _, err := os.Stat(snap); err == nil {
		t.Error("a snapshot of a failing run would bless the failure")
	}
	if !strings.Contains(r.stderr, "not recording") {
		t.Errorf("it should say why nothing was recorded:\n%s", r.stderr)
	}

	// a file that is not a snapshot is a setup error, before anything runs
	r = runFlowIn(t, dir, "shapes", 1, runOptions{baseURL: base, args: []string{"-shapes", filepath.Join(dir, "baseline.v1.json")}})
	expectExit(t, r, 2)
	if !strings.Contains(r.stderr, "not a Sleuth shape snapshot") {
		t.Errorf("%s", r.stderr)
	}
	expectExit(t, runFlowIn(t, dir, "shapes", 1, runOptions{baseURL: base, args: []string{"-shapes", filepath.Join(dir, "missing.json")}}), 2)

	// flags that make no sense together are usage errors
	expectExit(t, runFlowIn(t, dir, "shapes", 1, runOptions{baseURL: base, args: []string{"-shape-drift-fail", "breaking"}}), 2) // no -shapes
	expectExit(t, runFlowIn(t, dir, "shapes", 1, runOptions{baseURL: base, args: []string{"-merge"}}), 2)                        // no -record-shapes
}

func TestChangedFlowIsNotedNotRejected(t *testing.T) {
	dir := workdir(t)
	base := startService(t)
	snap := filepath.Join(dir, "s.json")
	expectExit(t, runFlowIn(t, dir, "shapes", 1, runOptions{baseURL: base, args: []string{"-record-shapes", snap}}), 0)

	b, _ := os.ReadFile(filepath.Join(dir, "shapes.flow.yaml"))
	os.WriteFile(filepath.Join(dir, "shapes.flow.yaml"), append(b, []byte("# edited since the snapshot\n")...), 0o644)
	r := runFlowIn(t, dir, "shapes", 1, runOptions{baseURL: base, args: []string{"-shapes", snap}})
	expectExit(t, r, 0)
	if !strings.Contains(r.stdout, "the flow file has changed since the snapshot was recorded") {
		t.Errorf("%s", r.stdout)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

var _ = json.Marshal
