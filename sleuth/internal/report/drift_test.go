package report

import (
	"bytes"
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
