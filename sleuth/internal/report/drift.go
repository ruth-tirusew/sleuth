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
	// SpecDrift is, per contract source with a saved baseline, how the live spec differs from it.
	SpecDrift []SpecDrift `json:"spec_drift,omitempty"`
	// Sources says, per contract source, how much of the published registry this run exercised.
	Sources []SourceCoverage `json:"sources,omitempty"`
}

// SpecDrift is the difference between a contract source's saved baseline and the live document,
// judged from the point of view of a client written against the baseline (see `sleuth diff`).
type SpecDrift struct {
	Source         string       `json:"source"`
	Baseline       string       `json:"baseline"`
	Breaking       int          `json:"breaking"`
	Review         int          `json:"review"`
	Info           int          `json:"info"`
	CalledBreaking int          `json:"called_breaking"` // breaking changes in operations this run called
	Failing        bool         `json:"failing,omitempty"`
	Changes        []SpecChange `json:"changes"`
}

type SpecChange struct {
	Severity  string `json:"severity"` // breaking | review | info
	Operation string `json:"operation,omitempty"`
	Where     string `json:"where"`
	Path      string `json:"path,omitempty"`
	Message   string `json:"message"`
	Called    bool   `json:"called,omitempty"` // a step of this run called the operation
}

// SourceCoverage is the registry-wide view of one contract source: the operations the
// document publishes against the ones this run actually called. An operation nobody calls
// is one nobody is checking against live behaviour, so it can drift unnoticed.
type SourceCoverage struct {
	Source    string   `json:"source"`
	Total     int      `json:"total"`
	Exercised int      `json:"exercised"`
	Untested  []string `json:"untested,omitempty"` // "METHOD /path", sorted
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

// AddCoverage records, for each contract source, which of its published operations the
// run called. all maps a source name to every operation it publishes ("METHOD /path").
// An operation counts as exercised when a step called it, whether or not the call passed.
func AddCoverage(r *Run, all map[string][]string) {
	if r.Drift == nil {
		return
	}
	called := map[string]map[string]bool{}
	for _, s := range r.Steps {
		if c := s.Contract; c != nil && c.Operation != "" {
			if called[c.Source] == nil {
				called[c.Source] = map[string]bool{}
			}
			called[c.Source][c.Operation] = true
		}
	}
	names := make([]string, 0, len(all))
	for name := range all {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sc := SourceCoverage{Source: name, Total: len(all[name])}
		for _, op := range all[name] {
			if called[name][op] {
				sc.Exercised++
			} else {
				sc.Untested = append(sc.Untested, op)
			}
		}
		sort.Strings(sc.Untested)
		r.Drift.Sources = append(r.Drift.Sources, sc)
	}
}

// AddSpecDrift records how each contract source differs from its baseline. A change is marked
// "called" when a step of this run called its operation. failOn is "", "breaking" or "review":
// when a source has a change at least that severe, it is marked failing and the run fails.
func AddSpecDrift(r *Run, drifts []SpecDrift, failOn string) {
	if r.Drift == nil || len(drifts) == 0 {
		return
	}
	called := map[string]map[string]bool{}
	for _, s := range r.Steps {
		if c := s.Contract; c != nil && c.Operation != "" {
			if called[c.Source] == nil {
				called[c.Source] = map[string]bool{}
			}
			called[c.Source][c.Operation] = true
		}
	}
	for _, d := range drifts {
		d.Breaking, d.Review, d.Info, d.CalledBreaking = 0, 0, 0, 0
		for i := range d.Changes {
			c := &d.Changes[i]
			c.Called = c.Operation != "" && called[d.Source][c.Operation]
			switch c.Severity {
			case "breaking":
				d.Breaking++
				if c.Called {
					d.CalledBreaking++
				}
			case "review":
				d.Review++
			default:
				d.Info++
			}
		}
		d.Failing = (failOn == "breaking" && d.Breaking > 0) || (failOn == "review" && d.Breaking+d.Review > 0)
		if d.Failing {
			r.Status = Failed
		}
		r.Drift.SpecDrift = append(r.Drift.SpecDrift, d)
	}
}

const maxUntestedShown = 15

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
	for _, sd := range d.SpecDrift {
		tag := ""
		if sd.Failing {
			tag = "FAIL  "
		}
		fmt.Fprintf(w, "Spec drift: %s%s vs baseline %s: %d breaking, %d to review, %d compatible", tag, sd.Source, sd.Baseline, sd.Breaking, sd.Review, sd.Info)
		if sd.Breaking > 0 {
			fmt.Fprintf(w, " (%d breaking in operations this run called)", sd.CalledBreaking)
		}
		fmt.Fprintln(w)
		shown := 0
		for _, c := range sd.Changes {
			if c.Severity == "info" {
				continue
			}
			if shown == maxUntestedShown {
				fmt.Fprintf(w, "  ... and more (all listed in the evidence JSON)\n")
				break
			}
			shown++
			mark := " "
			if c.Called {
				mark = "*"
			}
			fmt.Fprintf(w, "  %s[%s] %s [%s] %s %s\n", mark, c.Severity, c.Operation, c.Where, c.Path, c.Message)
		}
		if shown > 0 {
			fmt.Fprintln(w, "  (* = an operation this run called)")
		}
	}
	for _, sc := range d.Sources {
		pct := 100
		if sc.Total > 0 {
			pct = sc.Exercised * 100 / sc.Total
		}
		fmt.Fprintf(w, "Registry coverage: %s: %d of %d published operation(s) exercised (%d%%)\n", sc.Source, sc.Exercised, sc.Total, pct)
		for i, op := range sc.Untested {
			if i == maxUntestedShown {
				fmt.Fprintf(w, "  ... and %d more (all listed in the evidence JSON)\n", len(sc.Untested)-maxUntestedShown)
				break
			}
			fmt.Fprintf(w, "  not called: %s\n", op)
		}
	}
	if len(d.Coverage) > 0 {
		fmt.Fprintf(w, "Contract coverage gaps: %d call(s) the contract could not fully validate\n", len(d.Coverage))
		for _, g := range d.Coverage {
			fmt.Fprintf(w, "  %s %s: %s  (step %s)\n", g.Source, g.Operation, g.Note, g.Step)
		}
	}
}
