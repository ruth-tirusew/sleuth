package report

import (
	"fmt"
	"io"
	"sort"
)

// DriftSummary is the run-level view of registry-vs-live disagreement: every contract
// violation across all steps, plus the places where the contract was too thin to check.
// It is reported, never a crash: a run with drift still completes and records everything.
type DriftSummary struct {
	Findings   []DriftFinding `json:"findings"`
	Coverage   []CoverageGap  `json:"coverage_gaps,omitempty"`
	Failing    int            `json:"failing"`    // findings that failed their step
	Accepted   int            `json:"accepted"`   // findings recorded under on_violation: warn
	Operations int            `json:"operations"` // distinct operations with at least one finding
}

type DriftFinding struct {
	Step      string `json:"step"`
	Source    string `json:"source"`
	Operation string `json:"operation,omitempty"`
	Where     string `json:"where"` // operation | request | response
	Path      string `json:"path,omitempty"`
	Message   string `json:"message"`
	Mode      string `json:"mode"` // fail | warn
}

// CoverageGap is a documented operation whose contract could not validate the call.
type CoverageGap struct {
	Step      string `json:"step"`
	Source    string `json:"source"`
	Operation string `json:"operation,omitempty"`
	Note      string `json:"note"`
}

// SummarizeDrift fills r.Drift from the steps' contract evidence. It leaves Drift nil
// when no step used a contract, so runs without contracts have no drift section.
func SummarizeDrift(r *Run) {
	d := &DriftSummary{Findings: []DriftFinding{}}
	used := false
	ops := map[string]bool{}
	for _, s := range r.Steps {
		c := s.Contract
		if c == nil {
			continue
		}
		used = true
		for _, v := range c.Violations {
			d.Findings = append(d.Findings, DriftFinding{
				Step: s.ID, Source: c.Source, Operation: c.Operation,
				Where: v.Where, Path: v.Path, Message: v.Message, Mode: c.Mode,
			})
			if c.Mode == "warn" {
				d.Accepted++
			} else {
				d.Failing++
			}
			ops[c.Source+" "+c.Operation] = true
		}
		for _, n := range c.Notes {
			d.Coverage = append(d.Coverage, CoverageGap{Step: s.ID, Source: c.Source, Operation: c.Operation, Note: n})
		}
	}
	if !used {
		return
	}
	d.Operations = len(ops)
	r.Drift = d
}

// printDrift renders the drift section of the CLI report.
func printDrift(w io.Writer, d *DriftSummary) {
	if d == nil {
		return
	}
	fmt.Fprintln(w)
	if len(d.Findings) == 0 {
		fmt.Fprintln(w, "Contract drift: none")
	} else {
		fmt.Fprintf(w, "Contract drift: %d finding(s) across %d operation(s) — %d failing, %d accepted (warn)\n",
			len(d.Findings), d.Operations, d.Failing, d.Accepted)
		sort.SliceStable(d.Findings, func(i, j int) bool { return d.Findings[i].Mode < d.Findings[j].Mode }) // fail first
		for _, f := range d.Findings {
			tag := "FAIL"
			if f.Mode == "warn" {
				tag = "warn"
			}
			fmt.Fprintf(w, "  [%s] %s %s [%s] %s %s  (step %s)\n", tag, f.Source, f.Operation, f.Where, f.Path, f.Message, f.Step)
		}
	}
	if len(d.Coverage) > 0 {
		fmt.Fprintf(w, "Contract coverage gaps: %d call(s) the contract could not fully validate\n", len(d.Coverage))
		for _, g := range d.Coverage {
			fmt.Fprintf(w, "  %s %s: %s  (step %s)\n", g.Source, g.Operation, g.Note, g.Step)
		}
	}
}
