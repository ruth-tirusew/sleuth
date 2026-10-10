// Package report holds the evidence model and renderers.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const (
	Passed  = "passed"
	Failed  = "failed"
	Skipped = "skipped"
)

type Run struct {
	Flow       string    `json:"flow"`
	Env        string    `json:"env,omitempty"`
	Status     string    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	DurationMs int64     `json:"duration_ms"`
	Steps      []Step    `json:"steps"`
}

type Step struct {
	ID         string         `json:"id"`
	Kind       string         `json:"kind"`
	Status     string         `json:"status"`
	StartedAt  time.Time      `json:"started_at,omitempty"`
	DurationMs int64          `json:"duration_ms"`
	Request    *HTTPRequest   `json:"request,omitempty"`
	Response   *HTTPResponse  `json:"response,omitempty"`
	Checks     []CheckResult  `json:"checks,omitempty"`
	Output     map[string]any `json:"output,omitempty"`
	Error      string         `json:"error,omitempty"`
}

type HTTPRequest struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    any               `json:"body,omitempty"`
}

type HTTPResponse struct {
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       any               `json:"body,omitempty"`
	DurationMs int64             `json:"duration_ms"`
}

type CheckResult struct {
	Path     string `json:"path"`
	Op       string `json:"op"`
	Expected any    `json:"expected,omitempty"`
	Actual   any    `json:"actual,omitempty"`
	Passed   bool   `json:"passed"`
	Message  string `json:"message,omitempty"`
}

// WriteJSON writes the evidence file, masking any secret values.
func WriteJSON(path string, r *Run, secrets []string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	s := string(b)
	for _, sec := range secrets {
		esc, _ := json.Marshal(sec)
		s = strings.ReplaceAll(s, strings.Trim(string(esc), `"`), "***")
	}
	return os.WriteFile(path, []byte(s), 0o644)
}

// Print renders a human-readable summary.
func Print(w io.Writer, r *Run) {
	fmt.Fprintf(w, "Flow: %s  [%s]\n\n", r.Flow, strings.ToUpper(r.Status))
	icon := map[string]string{Passed: "✔", Failed: "✘", Skipped: "–"}
	for _, s := range r.Steps {
		fmt.Fprintf(w, " %s %-20s %-10s %5dms", icon[s.Status], s.ID, s.Kind, s.DurationMs)
		if s.Response != nil {
			fmt.Fprintf(w, "  %s %s -> %d", s.Request.Method, s.Request.URL, s.Response.Status)
		}
		fmt.Fprintln(w)
		for _, c := range s.Checks {
			if !c.Passed {
				fmt.Fprintf(w, "     ✘ %s %s: expected %v, got %v", c.Path, c.Op, c.Expected, c.Actual)
				if c.Message != "" {
					fmt.Fprintf(w, " (%s)", c.Message)
				}
				fmt.Fprintln(w)
			}
		}
		if s.Error != "" {
			fmt.Fprintf(w, "     error: %s\n", s.Error)
		}
	}
	fmt.Fprintf(w, "\n%d steps in %dms\n", len(r.Steps), r.DurationMs)
}
