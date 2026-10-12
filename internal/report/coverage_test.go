package report

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func runWith(flow string, sources ...SourceCoverage) *Run {
	r := &Run{Flow: flow}
	if len(sources) > 0 {
		r.Drift = &DriftSummary{Sources: sources}
	}
	return r
}

func TestCombineCoverageUnionsWhatEachRunCalled(t *testing.T) {
	a := runWith("browse", SourceCoverage{Source: "api", Total: 5, Exercised: 2, Called: []string{"GET /a", "POST /b"}, Untested: []string{"GET /c", "GET /d", "GET /e"}})
	b := runWith("auth", SourceCoverage{Source: "api", Total: 5, Exercised: 2, Called: []string{"GET /a", "GET /c"}, Untested: []string{"GET /d", "GET /e", "POST /b"}})
	c := runWith("db-only")
	got, notes := CombineCoverage([]*Run{a, b, c})
	if len(got) != 1 {
		t.Fatalf("%+v", got)
	}
	g := got[0]
	if g.Total != 5 || g.Exercised != 3 || g.Percent != 60 || !reflect.DeepEqual(g.Untested, []string{"GET /d", "GET /e"}) || !reflect.DeepEqual(g.Flows, []string{"browse", "auth"}) {
		t.Fatalf("%+v", g)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "db-only") {
		t.Errorf("a run without a contract adds no coverage, and is said so: %v", notes)
	}
}

func TestCombineCoverageAcrossContractVersions(t *testing.T) {
	a := runWith("old", SourceCoverage{Source: "api", Total: 2, Exercised: 1, Called: []string{"GET /a"}, Untested: []string{"GET /gone"}})
	b := runWith("new", SourceCoverage{Source: "api", Total: 2, Exercised: 1, Called: []string{"GET /new"}, Untested: []string{"GET /a"}})
	got, notes := CombineCoverage([]*Run{a, b})
	if got[0].Total != 3 || got[0].Exercised != 2 || !strings.Contains(strings.Join(notes, " "), "different versions") {
		t.Fatalf("%+v %v", got, notes)
	}
}

// Evidence written before runs recorded what they called: combined from what each did not call,
// with the total taken from the recorded counts (an operation every run called is in no list).
func TestCombineCoverageFromOlderEvidence(t *testing.T) {
	a := runWith("a", SourceCoverage{Source: "api", Total: 4, Exercised: 2, Untested: []string{"GET /c", "GET /d"}}) // called /a, /b
	b := runWith("b", SourceCoverage{Source: "api", Total: 4, Exercised: 2, Untested: []string{"GET /b", "GET /d"}}) // called /a, /c
	got, notes := CombineCoverage([]*Run{a, b})
	g := got[0]
	if g.Total != 4 || g.Exercised != 3 || !reflect.DeepEqual(g.Untested, []string{"GET /d"}) {
		t.Fatalf("%+v", g)
	}
	if !strings.Contains(strings.Join(notes, " "), "predates") {
		t.Errorf("%v", notes)
	}
}

func TestPrintCoverageModes(t *testing.T) {
	r := &Run{Flow: "f", Status: Passed, Drift: &DriftSummary{CallsChecked: 1, Findings: []DriftFinding{},
		Sources: []SourceCoverage{{Source: "api", Total: 3, Exercised: 1, Called: []string{"GET /a"}, Untested: []string{"GET /b", "GET /c"}}}}}
	out := func(mode string) string {
		var b bytes.Buffer
		PrintWith(&b, r, PrintOptions{Coverage: mode})
		return b.String()
	}
	if s := out(CoverageList); !strings.Contains(s, "1 of 3") || !strings.Contains(s, "not called: GET /b") {
		t.Errorf("list:\n%s", s)
	}
	if s := out(CoverageCount); !strings.Contains(s, "1 of 3") || strings.Contains(s, "not called") {
		t.Errorf("count:\n%s", s)
	}
	if s := out(CoverageOff); strings.Contains(s, "Registry coverage") {
		t.Errorf("off:\n%s", s)
	}
	if out("") != out(CoverageList) {
		t.Error("the default is the full list")
	}
}
