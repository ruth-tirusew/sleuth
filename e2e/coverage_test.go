package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCombinedCoverageAcrossFlows(t *testing.T) {
	dir := workdir(t)
	base := startService(t)
	var files []string
	for _, flow := range []string{"contract-ok", "contract-health", "happy"} { // happy uses no contract
		r := runFlowIn(t, dir, flow, 1, runOptions{baseURL: base})
		expectExit(t, r, 0)
		files = append(files, r.evidence)
	}

	r := invoke(t, dir, nil, append([]string{"coverage"}, files...)...)
	expectExit(t, r, 0)
	// the mock's v1 contract has 9 operations; contract-ok calls 4, contract-health 2 (one shared)
	if !strings.Contains(r.stdout, "api: 5 of 9 operation(s) exercised (55%) by contract-ok, contract-health") {
		t.Errorf("%s", r.stdout)
	}
	if !strings.Contains(r.stdout, "happy: used no contract") {
		t.Errorf("a run without a contract should be noted:\n%s", r.stdout)
	}

	expectExit(t, invoke(t, dir, nil, append([]string{"coverage", "-min", "50"}, files...)...), 0)
	below := invoke(t, dir, nil, append([]string{"coverage", "-min", "60"}, files...)...)
	expectExit(t, below, 1)
	if !strings.Contains(below.stdout, "FAIL api: 55% is below the minimum of 60%") {
		t.Errorf("%s", below.stdout)
	}

	j := invoke(t, dir, nil, append([]string{"coverage", "-json"}, files...)...)
	expectExit(t, j, 0)
	var doc struct {
		Runs    int `json:"runs"`
		Sources []struct {
			Source    string   `json:"source"`
			Exercised int      `json:"exercised"`
			Untested  []string `json:"untested"`
		} `json:"sources"`
	}
	if err := json.Unmarshal([]byte(j.stdout), &doc); err != nil || doc.Runs != 3 || doc.Sources[0].Exercised != 5 || len(doc.Sources[0].Untested) != 4 {
		t.Errorf("%v %+v\n%s", err, doc, j.stdout)
	}

	md := invoke(t, dir, nil, append([]string{"coverage", "-markdown", "-list"}, files...)...)
	expectExit(t, md, 0)
	if !strings.Contains(md.stdout, "| api | 5 | 9 | 55% | contract-ok, contract-health |") || !strings.Contains(md.stdout, "`GET /legacy/{id}`") {
		t.Errorf("%s", md.stdout)
	}
	if l := invoke(t, dir, nil, append([]string{"coverage", "-list"}, files...)...); !strings.Contains(l.stdout, "not called: POST /echo") {
		t.Errorf("%s", l.stdout)
	}

	notEvidence := filepath.Join(dir, "x.json")
	os.WriteFile(notEvidence, []byte(`{"openapi":"3.1.0"}`), 0o644)
	expectExit(t, invoke(t, dir, nil, "coverage", notEvidence), 2)
	expectExit(t, invoke(t, dir, nil, "coverage"), 2)
	expectExit(t, invoke(t, dir, nil, "coverage", "-min", "150", files[0]), 2)
}

func TestCoverageOutputModes(t *testing.T) {
	list := runFlow(t, "contract-ok", 1, runOptions{extraEnv: []string{"CI="}})
	if !strings.Contains(list.stdout, "not called:") {
		t.Errorf("outside CI the report lists the operations no step called:\n%s", list.stdout)
	}
	ci := runFlow(t, "contract-ok", 1, runOptions{extraEnv: []string{"CI=true"}})
	if !strings.Contains(ci.stdout, "Registry coverage: api: 4 of 9") || strings.Contains(ci.stdout, "not called:") {
		t.Errorf("in CI the report shows only the counts:\n%s", ci.stdout)
	}
	if len(ci.run.Drift.Sources[0].Untested) != 5 {
		t.Error("the evidence keeps the full list whatever the printed report shows")
	}
	if forced := runFlow(t, "contract-ok", 1, runOptions{extraEnv: []string{"CI=true"}, args: []string{"-coverage", "list"}}); !strings.Contains(forced.stdout, "not called:") {
		t.Error("-coverage list overrides the CI default")
	}
	if off := runFlow(t, "contract-ok", 1, runOptions{args: []string{"-coverage", "off"}}); strings.Contains(off.stdout, "Registry coverage") {
		t.Errorf("%s", off.stdout)
	}
	expectExit(t, runFlow(t, "contract-ok", 1, runOptions{args: []string{"-coverage", "verbose"}}), 2)
}
