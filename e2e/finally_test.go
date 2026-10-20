package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"sleuth/internal/report"
)

// Ctrl-C stops the run like a timeout does, but finally nodes still run, and the exit code is 130.
func TestInterruptRunsCleanup(t *testing.T) {
	dir := workdir(t)
	evidence := filepath.Join(dir, "evidence-int.json")
	cmd := exec.Command(sleuthBinary(t), "run", "-env", "env.yaml", "-out", evidence, "-run-id", "e2e-77", "interrupt.flow.yaml")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "BASE_URL="+startService(t), "APP_PASSWORD="+password)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(700*time.Millisecond, func() { cmd.Process.Signal(syscall.SIGINT) })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("the run did not stop after SIGINT\nstdout:\n%s", out.String())
	}
	if code := cmd.ProcessState.ExitCode(); code != 130 {
		t.Fatalf("exit %d, want 130\nstdout:\n%s\nstderr:\n%s", code, out.String(), errb.String())
	}
	b, err := os.ReadFile(evidence)
	if err != nil {
		t.Fatalf("no evidence: %v", err)
	}
	var run report.Run
	if err := json.Unmarshal(b, &run); err != nil {
		t.Fatal(err)
	}
	if !run.Interrupted || run.TimedOut || run.RunID != "e2e-77" {
		t.Errorf("evidence should say interrupted, with the run id: %+v", run)
	}
	r := &result{run: &run}
	if h := r.step("hang"); h.ErrorKind != "infrastructure" || !strings.Contains(h.Error, "interrupted") {
		t.Errorf("hang: %+v", h)
	}
	if tidy := r.step("tidy"); tidy.Status != report.Passed || !tidy.Cleanup || !strings.Contains(tidy.Request.URL, "run=e2e-77") {
		t.Errorf("cleanup must run after Ctrl-C with the same run id: %+v", tidy)
	}
	if !strings.Contains(errb.String(), "interrupted") || !strings.Contains(out.String(), "Run interrupted") {
		t.Errorf("the user should be told:\nstdout:\n%s\nstderr:\n%s", out.String(), errb.String())
	}
}
