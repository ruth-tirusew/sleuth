package report

import (
	"bytes"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
)

func TestSummarizeDrift(t *testing.T) {
	r := &Run{Steps: []Step{
		{ID: "plain", Kind: "request"}, // no contract: ignored
		{ID: "a", Contract: &ContractResult{Source: "api", Operation: "GET /x", Mode: "fail",
			Violations: []Violation{{Where: "response", Path: "$.id", Message: "want integer"}, {Where: "response", Path: "$.name", Message: "missing"}}}},
		{ID: "b", Contract: &ContractResult{Source: "api", Operation: "GET /y", Mode: "warn",
			Violations: []Violation{{Where: "response", Path: "$", Message: "status 401 is not documented"}},
			Notes:      []string{"contract has no schema for GET /y 200"}}},
		{ID: "c", Contract: &ContractResult{Source: "api", Operation: "GET /z", Mode: "fail"}}, // clean
	}}
	SummarizeDrift(r)
	d := r.Drift
	if d == nil || len(d.Findings) != 3 || d.Failing != 2 || d.Accepted != 1 || d.Operations != 2 || len(d.Coverage) != 1 {
		t.Fatalf("%+v", d)
	}
	var buf bytes.Buffer
	Print(&buf, r)
	out := buf.String()
	for _, want := range []string{"3 finding(s) across 2 operation(s)", "2 failing, 1 accepted", "[FAIL] api GET /x", "[warn] api GET /y", "coverage gaps: 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Index(out, "[FAIL]") > strings.Index(out, "[warn]") {
		t.Error("failing findings must be listed before accepted ones")
	}
}

func TestNoDriftSectionWithoutContracts(t *testing.T) {
	r := &Run{Steps: []Step{{ID: "a"}}}
	SummarizeDrift(r)
	if r.Drift != nil {
		t.Fatal("runs with no contract steps must have no drift section")
	}
	r = &Run{Steps: []Step{{ID: "a", Contract: &ContractResult{Source: "api", Operation: "GET /x", Mode: "fail"}}}}
	SummarizeDrift(r)
	var buf bytes.Buffer
	Print(&buf, r)
	if r.Drift == nil || len(r.Drift.Findings) != 0 || !strings.Contains(buf.String(), "Contract drift: none") {
		t.Fatalf("%+v\n%s", r.Drift, buf.String())
	}
}

func TestAddCoverageCountsCalledOperationsPerSource(t *testing.T) {
	r := &Run{Drift: &DriftSummary{}, Steps: []Step{
		{ID: "a", Contract: &ContractResult{Source: "api", Operation: "GET /users/{id}"}},
		{ID: "b", Contract: &ContractResult{Source: "api", Operation: "GET /users/{id}", Violations: []Violation{{Where: "response"}}}}, // failing still counts, once
		{ID: "c", Contract: &ContractResult{Source: "api"}},                                                                             // operation not in the contract: nothing to credit
		{ID: "d", Status: Skipped},
		{ID: "e", Contract: &ContractResult{Source: "other", Operation: "POST /x"}},
	}}
	AddCoverage(r, map[string][]string{
		"api":   {"POST /users", "GET /users/{id}", "GET /empty"},
		"other": {"POST /x"},
	})
	got := r.Drift.Sources
	if len(got) != 2 || got[0].Source != "api" || got[0].Total != 3 || got[0].Exercised != 1 ||
		len(got[0].Untested) != 2 || got[0].Untested[0] != "GET /empty" || got[0].Untested[1] != "POST /users" {
		t.Fatalf("%+v", got)
	}
	if got[1].Source != "other" || got[1].Exercised != 1 || len(got[1].Untested) != 0 {
		t.Fatalf("%+v", got[1])
	}

	var out strings.Builder
	printDrift(&out, r.Drift)
	for _, want := range []string{"api: 1 of 3 published operation(s) exercised (33%)", "not called: GET /empty", "other: 1 of 1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}

	none := &Run{} // no contract used: no drift section, and coverage must not invent one
	AddCoverage(none, map[string][]string{"api": {"GET /x"}})
	if none.Drift != nil {
		t.Fatal("coverage must not create a drift section")
	}
}

func TestAddSpecDriftMarksCalledOperationsAndFailsOnThreshold(t *testing.T) {
	mk := func() *Run {
		return &Run{Status: Passed, Drift: &DriftSummary{}, Steps: []Step{
			{ID: "a", Contract: &ContractResult{Source: "api", Operation: "GET /users/{id}"}},
		}}
	}
	drifts := func() []SpecDrift {
		return []SpecDrift{{Source: "api", Baseline: "base.json", Changes: []SpecChange{
			{Severity: "breaking", Operation: "GET /users/{id}", Where: "response", Path: "200 $.nick", Message: "property removed"},
			{Severity: "breaking", Operation: "GET /gone", Where: "operation", Message: "operation removed"},
			{Severity: "review", Operation: "GET /users/{id}", Where: "response", Message: "format changed"},
			{Severity: "info", Operation: "GET /new", Where: "operation", Message: "operation added"},
		}}}
	}

	r := mk()
	AddSpecDrift(r, drifts(), "")
	sd := r.Drift.SpecDrift[0]
	if sd.Breaking != 2 || sd.Review != 1 || sd.Info != 1 || sd.CalledBreaking != 1 || sd.Failing || r.Status != Passed {
		t.Fatalf("report-only must count but not fail: %+v status=%s", sd, r.Status)
	}
	if !sd.Changes[0].Called || sd.Changes[1].Called || sd.Changes[3].Called {
		t.Fatalf("only the operation a step called is marked: %+v", sd.Changes)
	}

	r = mk()
	AddSpecDrift(r, drifts(), "breaking")
	if !r.Drift.SpecDrift[0].Failing || r.Status != Failed {
		t.Fatalf("breaking threshold: %+v", r.Drift.SpecDrift[0])
	}

	// review-only drift does not trip "breaking" but does trip "review"
	reviewOnly := []SpecDrift{{Source: "api", Changes: []SpecChange{{Severity: "review", Operation: "GET /x", Where: "response", Message: "format changed"}}}}
	r = mk()
	AddSpecDrift(r, reviewOnly, "breaking")
	if r.Drift.SpecDrift[0].Failing {
		t.Fatal("review must not trip the breaking threshold")
	}
	r = mk()
	AddSpecDrift(r, reviewOnly, "review")
	if !r.Drift.SpecDrift[0].Failing || r.Status != Failed {
		t.Fatal("review must trip the review threshold")
	}

	var out strings.Builder
	r = mk()
	AddSpecDrift(r, drifts(), "breaking")
	printDrift(&out, r.Drift)
	for _, want := range []string{"Spec drift: FAIL  api vs baseline base.json: 2 breaking, 1 to review, 1 compatible (1 breaking in operations this run called)", "*[breaking] GET /users/{id}", " [breaking] GET /gone", "(* = an operation this run called)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "GET /new") {
		t.Error("compatible changes are summarised in the count, not listed")
	}

	none := &Run{} // no contract used: no drift section to hang it on
	AddSpecDrift(none, drifts(), "breaking")
	if none.Drift != nil {
		t.Fatal("must not invent a drift section")
	}
}

func TestDriftNoneSaysWhenNothingWasChecked(t *testing.T) {
	r := &Run{Steps: []Step{{ID: "a", Status: Skipped}}}
	EnsureDrift(r)
	var buf bytes.Buffer
	Print(&buf, r)
	if !strings.Contains(buf.String(), "no contract call ran") {
		t.Fatalf("an empty result must not read as a clean one:\n%s", buf.String())
	}
}

func TestPrintDoesNotReorderEvidence(t *testing.T) {
	r := &Run{Steps: []Step{
		{ID: "w", Contract: &ContractResult{Source: "api", Operation: "GET /w", Mode: "warn", Violations: []Violation{{Where: "response", Message: "first"}}}},
		{ID: "f", Contract: &ContractResult{Source: "api", Operation: "GET /f", Mode: "fail", Violations: []Violation{{Where: "response", Message: "second"}}}},
	}}
	SummarizeDrift(r)
	before := []string{r.Drift.Findings[0].Step, r.Drift.Findings[1].Step}
	Print(&bytes.Buffer{}, r)
	if after := []string{r.Drift.Findings[0].Step, r.Drift.Findings[1].Step}; after[0] != before[0] || after[1] != before[1] {
		t.Fatalf("printing changed the report's data: %v -> %v", before, after)
	}
}

func TestMaskingCoversEncodedForms(t *testing.T) {
	sec := "p@ss w/rd&<x>"
	secrets := []string{sec, "short"}
	forms := []string{
		sec,
		url.QueryEscape(sec), // a query value
		url.PathEscape(sec),  // one escaped path segment
		(&url.URL{Path: "/x/" + sec}).EscapedPath(), // a whole path, where "/" stays a separator
		base64.StdEncoding.EncodeToString([]byte(sec)),
	}
	text := strings.Join(forms, " | ") + " | short"
	got := MaskText(text, secrets)
	for _, f := range append(forms, "short") {
		if strings.Contains(got, strings.TrimPrefix(f, "/x/")) {
			t.Errorf("%q survived masking: %s", f, got)
		}
	}
	// JSON, in both escapings of & and <
	for _, j := range []string{
		`{"u":"` + url.QueryEscape(sec) + `","raw":"p@ss w/rd&<x>"}`,
		`{"u":"` + url.QueryEscape(sec) + `","raw":"p@ss w/rd&<x>"}`,
	} {
		if m := string(MaskJSON([]byte(j), secrets)); strings.Contains(m, "p@ss") || strings.Contains(m, "p%40ss") {
			t.Errorf("JSON masking missed a form: %s", m)
		}
	}
	// the base64 of a very short secret is not masked: it would mangle ordinary text
	if MaskText("c2hvcnQ=", []string{"short"}) != "c2hvcnQ=" {
		t.Error("short secrets should not have their base64 masked")
	}
}
