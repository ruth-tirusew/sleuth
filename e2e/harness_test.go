// Package e2e runs the real sleuth binary against a service started in-process, and checks
// what a user sees: exit codes, the printed report and the evidence file. Nothing outside
// this repository is needed. Run it with `go test ./e2e` (or `make e2e`).
package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"sleuth/internal/mockservice"
	"sleuth/internal/report"
)

const password = "s3cret"

var evidenceN int

var (
	buildOnce sync.Once
	binary    string
	buildErr  error
)

// sleuthBinary builds the CLI once per test run (with -race if SLEUTH_E2E_RACE=1), the first time a test needs it. In -short
// mode every e2e test is skipped before this is reached, so no build happens.
func sleuthBinary(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("end-to-end tests are skipped in -short mode")
	}
	buildOnce.Do(func() {
		trackSources()
		dir, err := os.MkdirTemp("", "sleuth-e2e-bin-*")
		if err != nil {
			buildErr = err
			return
		}
		binary = filepath.Join(dir, "sleuth")
		root, _ := filepath.Abs("..")
		args := []string{"build", "-o", binary}
		if os.Getenv("SLEUTH_E2E_RACE") == "1" { // a race in the program makes its exit code 66
			args = append(args, "-race")
		}
		cmd := exec.Command("go", append(args, "./cmd/sleuth")...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = errors.New("go build failed: " + err.Error() + "\n" + string(out))
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binary
}

// trackSources opens every source file of the program under test. Go's test cache treats the
// files a test reads as its inputs, and this package does not import cmd/sleuth (it runs the
// built binary), so without this a change to the program would leave a stale cached "pass".
func trackSources() {
	root, _ := filepath.Abs("..")
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if n := d.Name(); n == ".git" || n == "evidence" || n == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if n := d.Name(); strings.HasSuffix(n, ".go") || n == "go.mod" || n == "go.sum" {
			if f, err := os.Open(path); err == nil {
				f.Close()
			}
		}
		return nil
	})
}

// result is one invocation of the binary.
type result struct {
	exit     int
	stdout   string
	stderr   string
	dir      string // the working directory the command ran in
	evidence string // path of the evidence file, if the command writes one
	run      *report.Run
}

func (r *result) step(id string) report.Step {
	if r.run != nil {
		for _, s := range r.run.Steps {
			if s.ID == id {
				return s
			}
		}
	}
	return report.Step{}
}

// rawEvidence is the evidence file exactly as written (to check for leaked secrets).
func (r *result) rawEvidence(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(r.evidence)
	if err != nil {
		t.Fatalf("no evidence file: %v\nstdout:\n%s\nstderr:\n%s", err, r.stdout, r.stderr)
	}
	return string(b)
}

// invoke runs the binary in dir with extra environment variables.
func invoke(t *testing.T, dir string, env []string, args ...string) *result {
	t.Helper()
	cmd := exec.Command(sleuthBinary(t), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		code = ee.ExitCode()
	case err != nil:
		t.Fatalf("could not run sleuth: %v", err)
	}
	return &result{exit: code, stdout: out.String(), stderr: errb.String(), dir: dir}
}

// workdir copies the test flows and env files into a fresh directory (relative paths such as
// the baseline resolve there) and writes the v1 contract as baseline.v1.json.
func workdir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join("testdata", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "baseline.v1.json"), mockservice.Spec(1), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// startService runs a fresh mock service (so its counters start at zero) and returns its URL.
func startService(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(mockservice.New().Handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

// runOptions tunes one `sleuth run`.
type runOptions struct {
	env      string   // env file in testdata (default env.yaml)
	args     []string // extra run flags, before the flow
	baseURL  string   // use this instead of the live mock service
	noSecret bool     // do not provide APP_PASSWORD
	extraEnv []string
}

// runFlow runs `sleuth run` on testdata/<flow>.flow.yaml and parses the evidence it wrote.
func runFlow(t *testing.T, flow string, parallel int, o runOptions) *result {
	t.Helper()
	return runFlowIn(t, workdir(t), flow, parallel, o)
}

// runFlowIn is runFlow in a given working directory, so several runs can share files such as
// a snapshot. Each run still writes its own evidence file.
func runFlowIn(t *testing.T, dir, flow string, parallel int, o runOptions) *result {
	t.Helper()
	base := o.baseURL
	if base == "" {
		base = startService(t)
	}
	envFile := o.env
	if envFile == "" {
		envFile = "env.yaml"
	}
	env := append([]string{"BASE_URL=" + base}, o.extraEnv...)
	if !o.noSecret {
		env = append(env, "APP_PASSWORD="+password)
	}
	evidenceN++
	evidence := filepath.Join(dir, fmt.Sprintf("evidence-%d.json", evidenceN))
	args := []string{"run", "-env", envFile, "-out", evidence, "-parallel", itoa(parallel)}
	args = append(args, o.args...)
	args = append(args, flow+".flow.yaml")
	r := invoke(t, dir, env, args...)
	r.evidence = evidence
	if b, err := os.ReadFile(evidence); err == nil {
		var run report.Run
		if err := json.Unmarshal(b, &run); err != nil {
			t.Fatalf("evidence is not valid JSON: %v", err)
		}
		r.run = &run
	}
	return r
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// expectSteps checks the outcome of each named step: "passed", "skipped", or the error_kind
// of a failed step ("assertion", "contract", "timeout", "infrastructure", "spec").
func expectSteps(t *testing.T, r *result, want map[string]string) {
	t.Helper()
	if r.run == nil {
		t.Fatalf("no evidence\nstdout:\n%s\nstderr:\n%s", r.stdout, r.stderr)
	}
	for id, w := range want {
		s := r.step(id)
		if s.ID == "" {
			t.Errorf("step %q is not in the evidence", id)
			continue
		}
		got := s.ErrorKind
		switch s.Status {
		case report.Passed:
			got = "passed"
		case report.Skipped:
			got = "skipped"
		}
		if got != w {
			t.Errorf("step %q: got %q, want %q (error: %s)", id, got, w, s.Error)
		}
	}
}

func expectExit(t *testing.T, r *result, want int) {
	t.Helper()
	if r.exit != want {
		t.Fatalf("exit code %d, want %d\nstdout:\n%s\nstderr:\n%s", r.exit, want, r.stdout, r.stderr)
	}
}
