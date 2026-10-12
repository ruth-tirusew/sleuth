package e2e

import (
	"encoding/xml"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// -timeout stops the whole run: requests, polls and queries in flight, and nodes not started.
func TestRunTimeout(t *testing.T) {
	for _, p := range []int{1, 4} {
		p := p
		t.Run(fmt.Sprintf("parallel=%d", p), func(t *testing.T) {
			start := time.Now()
			r := runFlow(t, "slow-run", p, runOptions{args: []string{"-timeout", "700ms"}})
			took := time.Since(start)
			expectExit(t, r, 3)
			if took > 5*time.Second {
				t.Fatalf("the run took %s; -timeout 700ms must stop it", took)
			}
			if !r.run.TimedOut || r.run.Timeout != "700ms" {
				t.Errorf("evidence should record the timeout: %+v", r.run)
			}
			if !strings.Contains(r.stdout, "Run stopped: its timeout (700ms) was reached") {
				t.Errorf("%s", r.stdout)
			}
			if h := r.step("hang"); h.ErrorKind != "infrastructure" || !strings.Contains(h.Error, "run's timeout") {
				t.Errorf("hang: %+v", h)
			}
			if a := r.step("after"); a.Status != "skipped" {
				t.Errorf("after: %+v", a)
			}
			w := r.step("wait")
			if w.Status == "passed" {
				t.Errorf("wait cannot have converged: %+v", w)
			}
		})
	}
}

type junitDoc struct {
	Tests    int `xml:"tests,attr"`
	Failures int `xml:"failures,attr"`
	Errors   int `xml:"errors,attr"`
	Skipped  int `xml:"skipped,attr"`
	Suites   []struct {
		Name  string `xml:"name,attr"`
		Cases []struct {
			Name    string    `xml:"name,attr"`
			Failure *struct{} `xml:"failure"`
			Error   *struct{} `xml:"error"`
			Skipped *struct{} `xml:"skipped"`
		} `xml:"testcase"`
	} `xml:"testsuite"`
}

func readJUnit(t *testing.T, path string) (junitDoc, string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no JUnit file: %v", err)
	}
	var d junitDoc
	if err := xml.Unmarshal(b, &d); err != nil {
		t.Fatalf("not valid XML: %v\n%s", err, b)
	}
	return d, string(b)
}

func TestJUnitOutput(t *testing.T) {
	junit := func(dir string) string { return filepath.Join(dir, "results.xml") }

	dir := workdir(t)
	r := runFlowIn(t, dir, "happy", 1, runOptions{args: []string{"-junit", junit(dir)}})
	expectExit(t, r, 0)
	d, _ := readJUnit(t, junit(dir))
	if d.Tests != 5 || d.Failures+d.Errors+d.Skipped != 0 || d.Suites[0].Name != "happy" {
		t.Errorf("all passing: %+v", d)
	}

	dir = workdir(t)
	r = runFlowIn(t, dir, "assertion-fail", 4, runOptions{args: []string{"-junit", junit(dir)}})
	expectExit(t, r, 1)
	d, _ = readJUnit(t, junit(dir))
	if d.Tests != 3 || d.Failures != 1 || d.Skipped != 1 || d.Errors != 0 {
		t.Errorf("a failing run writes JUnit too, with the failure and the skip: %+v", d)
	}

	dir = workdir(t)
	r = runFlowIn(t, dir, "secret-in-url", 1, runOptions{env: "env-secret.yaml", extraEnv: []string{"API_TOKEN=" + apiToken}, args: []string{"-junit", junit(dir)}})
	expectExit(t, r, 3)
	d, raw := readJUnit(t, junit(dir))
	if d.Errors != 1 {
		t.Errorf("an unreachable host is an error: %+v", d)
	}
	for _, form := range []string{apiToken, url.QueryEscape(apiToken), url.PathEscape(apiToken), "p@ss", "p%40ss", "p@ss w/rd&amp;"} {
		if strings.Contains(raw, form) {
			t.Errorf("the secret leaked into the JUnit file as %q", form)
		}
	}

	// a run that fails only on drift is not all green in CI either
	dir = workdir(t)
	r = runFlowIn(t, dir, "contract-health", 1, runOptions{env: "env-v2.yaml", args: []string{"-spec-drift-fail", "breaking", "-junit", junit(dir)}})
	expectExit(t, r, 1)
	d, _ = readJUnit(t, junit(dir))
	if d.Failures != 1 {
		t.Errorf("spec drift that fails the run must be a failing test case: %+v", d)
	}
}
