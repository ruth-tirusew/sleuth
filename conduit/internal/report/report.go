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
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	StartedAt  time.Time       `json:"started_at,omitempty"`
	DurationMs int64           `json:"duration_ms"`
	Request    *HTTPRequest    `json:"request,omitempty"`
	Response   *HTTPResponse   `json:"response,omitempty"`
	Checks     []CheckResult   `json:"checks,omitempty"`
	Contract   *ContractResult `json:"contract,omitempty"`
	Output     map[string]any  `json:"output,omitempty"`
	Error      string          `json:"error,omitempty"`
	// ErrorKind is set on failed steps: assertion, infrastructure or spec.
	ErrorKind string `json:"error_kind,omitempty"`
}

// ContractResult is the evidence of validating one call against its OpenAPI contract.
type ContractResult struct {
	Source     string      `json:"source"`
	Operation  string      `json:"operation,omitempty"` // "GET /customer/{id}"; empty if not in the contract
	Mode       string      `json:"mode"`                // fail | warn
	Violations []Violation `json:"violations,omitempty"`
	Notes      []string    `json:"notes,omitempty"` // things that could not be checked (e.g. empty schema)
}

type Violation struct {
	Where   string `json:"where"` // operation | request | response
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
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
		if c := s.Contract; c != nil {
			if len(c.Violations) == 0 {
				fmt.Fprintf(w, "     contract %s %s: ok\n", c.Source, c.Operation)
			}
			for _, v := range c.Violations {
				fmt.Fprintf(w, "     contract %s [%s] %s %s (%s)\n", c.Source, v.Where, v.Path, v.Message, c.Mode)
			}
			for _, n := range c.Notes {
				fmt.Fprintf(w, "     note: %s\n", n)
			}
		}
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
			if s.ErrorKind != "" {
				fmt.Fprintf(w, "     error [%s]: %s\n", s.ErrorKind, s.Error)
			} else {
				fmt.Fprintf(w, "     error: %s\n", s.Error)
			}
		}
	}
	fmt.Fprintf(w, "\n%d steps in %dms\n", len(r.Steps), r.DurationMs)
}
