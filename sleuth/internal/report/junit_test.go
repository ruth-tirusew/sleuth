package report

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"sleuth/internal/shape"
)

func TestJUnit(t *testing.T) {
	secret := "tok&en<1>"
	r := &Run{Flow: "checkout", Env: "staging", Status: Failed, StartedAt: time.Unix(0, 0).UTC(), DurationMs: 1500, Parallel: 4, TimedOut: true, Timeout: "1s",
		Steps: []Step{
			{ID: "ok", Kind: "request", Status: Passed, DurationMs: 12},
			{ID: "wrong", Kind: "assertion", Status: Failed, ErrorKind: "assertion", Error: "1 assertion(s) failed",
				Checks: []CheckResult{{Path: "nodes.a.status", Op: "equals", Expected: 200, Actual: 500, Message: "the value was " + secret}}},
			{ID: "down", Kind: "request", Status: Failed, ErrorKind: "infrastructure", Error: `Get "http://h/x?t=` + secret + `": refused`,
				Request: &HTTPRequest{Method: "GET", URL: "http://h/x?t=" + secret}},
			{ID: "later", Kind: "transform", Status: Skipped, Error: "skipped: dependency \"down\" did not pass"},
		},
		Drift: &DriftSummary{
			SpecDrift: []SpecDrift{{Source: "api", Baseline: "b.json", Breaking: 1, Failing: true, Changes: []SpecChange{{Severity: "breaking", Operation: "GET /x", Where: "response", Message: "property removed"}}}},
			Behaviour: &BehaviourDrift{Snapshot: "s.json", Breaking: 1, Failing: true, Changes: []shape.Change{{Step: "ok", Severity: "breaking", Path: "$.id", Message: "type changed"}}},
		},
	}
	b, err := JUnit(r, []string{secret})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "<?xml") {
		t.Fatal("missing XML header")
	}
	if strings.Contains(string(b), "tok&amp;en") || strings.Contains(string(b), "tok&en") || strings.Contains(string(b), "tok") {
		t.Fatalf("the secret leaked into the JUnit report:\n%s", b)
	}
	var doc junitSuites
	if err := xml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("not valid XML: %v\n%s", err, b)
	}
	s := doc.Suites[0]
	// 4 steps + spec drift + behaviour drift + run timeout
	if s.Name != "checkout" || s.Tests != 7 || s.Failures != 3 || s.Errors != 2 || s.Skipped != 1 || doc.Tests != 7 {
		t.Fatalf("counts: tests=%d failures=%d errors=%d skipped=%d\n%s", s.Tests, s.Failures, s.Errors, s.Skipped, b)
	}
	byName := map[string]junitCase{}
	for _, c := range s.Cases {
		byName[c.Name] = c
	}
	if c := byName["wrong"]; c.Failure == nil || c.Failure.Type != "assertion" || !strings.Contains(c.Failure.Body, "expected 200, got 500") {
		t.Errorf("an assertion is a failure with the failed check in the body: %+v", c.Failure)
	}
	if c := byName["down"]; c.Error == nil || c.Error.Type != "infrastructure" {
		t.Errorf("an unreachable system is an error, not a failure: %+v", c)
	}
	if c := byName["later"]; c.Skipped == nil {
		t.Error("a skipped step is skipped")
	}
	if c := byName["ok"]; c.Failure != nil || c.Error != nil || c.Time != "0.012" || c.Classname != "checkout.request" {
		t.Errorf("%+v", c)
	}
	if byName["spec drift: api"].Failure == nil || byName["behaviour drift"].Failure == nil || byName["run timeout"].Error == nil {
		t.Error("run-level verdicts must be test cases too, so a failing run never shows all green")
	}
}

func TestJUnitAllPassing(t *testing.T) {
	b, _ := JUnit(&Run{Flow: "f", Status: Passed, Steps: []Step{{ID: "a", Kind: "request", Status: Passed}}}, nil)
	var doc junitSuites
	if xml.Unmarshal(b, &doc) != nil || doc.Tests != 1 || doc.Failures+doc.Errors+doc.Skipped != 0 {
		t.Fatalf("%s", b)
	}
}
