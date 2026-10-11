// Package report holds the evidence model and renderers.
package report

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"sleuth/internal/shape"
	"sort"
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
	Parallel   int       `json:"parallel,omitempty"` // max nodes run at once; absent when serial
	Steps      []Step    `json:"steps"`
	// Drift summarises contract disagreements across the run; absent when no step used a contract.
	Drift *DriftSummary `json:"drift,omitempty"`
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
	Poll       *PollResult     `json:"poll,omitempty"`
	DB         *DBResult       `json:"db,omitempty"`
	// Shape is the structure of this step's response (or database rows), recorded when the run
	// was asked to capture shapes. It holds no values.
	Shape  *shape.Entry   `json:"shape,omitempty"`
	Output map[string]any `json:"output,omitempty"`
	Error  string         `json:"error,omitempty"`
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

// DBResult is the evidence of a db node.
type DBResult struct {
	Type       string           `json:"type"`       // postgres | mongo
	Connection string           `json:"connection"` // the name, never the DSN
	Query      string           `json:"query"`
	Args       []any            `json:"args,omitempty"`
	RowCount   int              `json:"row_count"`
	Rows       []map[string]any `json:"rows,omitempty"` // first 20 only
	Truncated  bool             `json:"rows_truncated,omitempty"`
	DurationMs int64            `json:"duration_ms"`
}

// PollResult is the evidence of a poll node: every attempt and why it ended.
type PollResult struct {
	Attempts   int           `json:"attempts"`
	IntervalMs int64         `json:"interval_ms"`
	TimeoutMs  int64         `json:"timeout_ms"`
	Satisfied  bool          `json:"satisfied"`
	History    []PollAttempt `json:"history"`
}

type PollAttempt struct {
	N         int    `json:"n"`
	ElapsedMs int64  `json:"elapsed_ms"`
	Status    int    `json:"status,omitempty"`    // HTTP status; 0 when the attempt got no response
	RowCount  int    `json:"row_count,omitempty"` // db polls: rows returned
	Passed    bool   `json:"passed"`
	Error     string `json:"error,omitempty"`
	DelayMs   int64  `json:"delay_ms,omitempty"` // wait before the next attempt
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
	return os.WriteFile(path, MaskJSON(b, secrets), 0o644)
}

// MaskJSON replaces every secret value in marshalled JSON with ***, in each form a secret can
// take on its way into a request (see secretForms). Anything that leaves the process
// (evidence files, a UI) must go through it.
func MaskJSON(b []byte, secrets []string) []byte {
	var escaped []string
	for _, f := range secretForms(secrets) {
		// both JSON spellings: with HTML characters escaped (\u0026, as json.Marshal writes them)
		// and without (as an encoder with SetEscapeHTML(false) writes them)
		esc, _ := json.Marshal(f)
		escaped = append(escaped, strings.Trim(string(esc), `"`))
		var plain bytes.Buffer
		enc := json.NewEncoder(&plain)
		enc.SetEscapeHTML(false)
		enc.Encode(f)
		escaped = append(escaped, strings.Trim(strings.TrimSpace(plain.String()), `"`))
	}
	sort.SliceStable(escaped, func(i, j int) bool { return len(escaped[i]) > len(escaped[j]) })
	return []byte(replaceAll(string(b), escaped))
}

// MaskText is MaskJSON for plain text: the printed report and error messages.
func MaskText(s string, secrets []string) string {
	return replaceAll(s, secretForms(secrets))
}

// minEncodedSecret is the shortest secret whose base64 form is also masked: the base64 of a
// very short value is a short, common-looking string, and masking it would mangle unrelated text.
const minEncodedSecret = 8

// secretForms lists every secret together with the forms it takes once it is put into a
// request: percent-encoded in a URL query or path (as Go writes a whole path, and as one
// escaped segment), and base64 (as in a Basic auth header, when
// the secret is the whole credential). Longest first, so a longer form is masked before any
// shorter form it contains.
func secretForms(secrets []string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(f string) {
		if f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	for _, sec := range secrets {
		if sec == "" {
			continue
		}
		add(sec)
		add(url.QueryEscape(sec))
		add(url.PathEscape(sec))
		add((&url.URL{Path: sec}).EscapedPath()) // inside a path, where "/" stays a separator
		if len(sec) >= minEncodedSecret {
			add(base64.StdEncoding.EncodeToString([]byte(sec)))
			add(base64.URLEncoding.EncodeToString([]byte(sec)))
			add(base64.RawStdEncoding.EncodeToString([]byte(sec)))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

func replaceAll(s string, forms []string) string {
	for _, f := range forms {
		s = strings.ReplaceAll(s, f, "***")
	}
	return s
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
		if d := s.DB; d != nil {
			fmt.Fprintf(w, "     db %s: %d row(s) in %dms\n", d.Connection, d.RowCount, d.DurationMs)
		}
		if p := s.Poll; p != nil {
			fmt.Fprintf(w, "     poll: %d attempt(s), every %dms, timeout %dms, satisfied=%v\n", p.Attempts, p.IntervalMs, p.TimeoutMs, p.Satisfied)
		}
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
	printDrift(w, r.Drift)
	fmt.Fprintf(w, "\n%d steps in %dms\n", len(r.Steps), r.DurationMs)
}
