package report

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"sleuth/internal/shape"
)

// DriftSummary is the run-level view of registry-vs-live disagreement: every contract
// violation across all steps, plus the places where the contract was too thin to check.
// It is reported, never a crash: a run with drift still completes and records everything.
type DriftSummary struct {
	Findings []DriftFinding `json:"findings"`
	// CallsChecked is how many contract steps produced evidence. Zero means no contract call
	// actually ran (for example every contract node was skipped), so "no findings" says nothing.
	CallsChecked int           `json:"calls_checked"`
	Coverage     []CoverageGap `json:"coverage_gaps,omitempty"`
	Failing      int           `json:"failing"`    // findings that failed their step
	Accepted     int           `json:"accepted"`   // findings recorded under on_violation: warn
	Operations   int           `json:"operations"` // distinct operations with at least one finding
	// SpecDrift is, per contract source with a saved baseline, how the live spec differs from it.
	SpecDrift []SpecDrift `json:"spec_drift,omitempty"`
	// Sources says, per contract source, how much of the published registry this run exercised.
	Sources []SourceCoverage `json:"sources,omitempty"`
	// Behaviour is how this run's responses differ from a recorded shape snapshot (see
	// docs/decisions/0007). It needs no contract.
	Behaviour *BehaviourDrift `json:"behaviour,omitempty"`

	behaviourOnly bool // the block exists only for Behaviour: no contract was involved
}

// BehaviourDrift compares the shapes of this run's responses with a recorded snapshot.
type BehaviourDrift struct {
	Snapshot   string         `json:"snapshot"` // the file compared against
	RecordedAt time.Time      `json:"recorded_at"`
	Tool       string         `json:"tool,omitempty"`
	Runs       int            `json:"runs"`
	Breaking   int            `json:"breaking"`
	Info       int            `json:"info"`
	Failing    bool           `json:"failing,omitempty"`
	Changes    []shape.Change `json:"changes"`
	Notes      []string       `json:"notes,omitempty"`
}

// BehaviourInput is what AddBehaviourDrift needs to know about the snapshot and the flow.
type BehaviourInput struct {
	Path    string // the snapshot file, for the report
	Snap    *shape.Snapshot
	FailOn  string // "", "breaking" or "any"
	Flow    string
	FlowSHA string
	Now     time.Time
}

// AddBehaviourDrift compares the shapes captured on r's steps with the snapshot and records the
// result in r.Drift.Behaviour. With FailOn "breaking" (or "any", which also fails on compatible
// changes) a difference of that severity fails the run.
func AddBehaviourDrift(r *Run, in BehaviourInput) {
	observed := map[string]shape.Entry{}
	for _, s := range r.Steps {
		if s.Shape != nil {
			observed[s.ID] = *s.Shape
		}
	}
	b := &BehaviourDrift{Snapshot: in.Path, RecordedAt: in.Snap.Recorded.At, Tool: in.Snap.Recorded.Tool, Runs: in.Snap.Recorded.Runs, Changes: []shape.Change{}}
	b.Changes = append(b.Changes, shape.Compare(in.Snap, observed)...)
	for _, c := range b.Changes {
		if c.Severity == shape.Breaking {
			b.Breaking++
		} else {
			b.Info++
		}
	}
	b.Failing = (in.FailOn == "breaking" && b.Breaking > 0) || (in.FailOn == "any" && len(b.Changes) > 0)
	if in.Snap.Flow != "" && in.Flow != "" && in.Snap.Flow != in.Flow {
		b.Notes = append(b.Notes, fmt.Sprintf("the snapshot was recorded for flow %q, this is %q", in.Snap.Flow, in.Flow))
	}
	if in.Snap.FlowSHA != "" && in.FlowSHA != "" && in.Snap.FlowSHA != in.FlowSHA {
		b.Notes = append(b.Notes, "the flow file has changed since the snapshot was recorded")
	}
	if len(observed) == 0 {
		b.Notes = append(b.Notes, "no step produced a JSON response, so nothing was compared")
	}
	if r.Drift == nil {
		r.Drift = &DriftSummary{Findings: []DriftFinding{}, behaviourOnly: true}
	}
	r.Drift.Behaviour = b
	if b.Failing {
		r.Status = Failed
	}
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
	// Called lists the operations exercised, so coverage from several runs can be combined
	// exactly (see CombineCoverage).
	Called []string `json:"called,omitempty"`
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
		d.CallsChecked++
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

// EnsureDrift makes sure the run has a drift block. The runner calls it when the flow
// references a contract source, so that spec drift and coverage are reported (and the
// spec-drift gate is enforced) even when no contract step ran.
func EnsureDrift(r *Run) {
	if r.Drift == nil {
		r.Drift = &DriftSummary{Findings: []DriftFinding{}}
	}
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
				sc.Called = append(sc.Called, op)
			} else {
				sc.Untested = append(sc.Untested, op)
			}
		}
		sort.Strings(sc.Untested)
		sort.Strings(sc.Called)
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
func printDrift(w io.Writer, d *DriftSummary, o PrintOptions) {
	if d == nil {
		return
	}
	fmt.Fprintln(w)
	if d.behaviourOnly {
		printBehaviour(w, d.Behaviour)
		return
	}
	defer printBehaviour(w, d.Behaviour)
	if len(d.Findings) == 0 {
		if d.CallsChecked == 0 {
			fmt.Fprintln(w, "Contract drift: none found, but no contract call ran, so nothing was checked")
		} else {
			fmt.Fprintln(w, "Contract drift: none")
		}
	} else {
		fmt.Fprintf(w, "Contract drift: %d finding(s) across %d operation(s) — %d failing, %d accepted (warn)\n",
			len(d.Findings), d.Operations, d.Failing, d.Accepted)
		// Failing findings first, on a copy: printing must not reorder the report's own data.
		shown := append([]DriftFinding(nil), d.Findings...)
		sort.SliceStable(shown, func(i, j int) bool { return shown[i].Mode < shown[j].Mode })
		for _, f := range shown {
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
		if o.Coverage == CoverageOff {
			break
		}
		pct := 100
		if sc.Total > 0 {
			pct = sc.Exercised * 100 / sc.Total
		}
		fmt.Fprintf(w, "Registry coverage: %s: %d of %d published operation(s) exercised (%d%%)\n", sc.Source, sc.Exercised, sc.Total, pct)
		if o.Coverage == CoverageCount {
			continue
		}
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

func printBehaviour(w io.Writer, b *BehaviourDrift) {
	if b == nil {
		return
	}
	tag := ""
	if b.Failing {
		tag = "FAIL  "
	}
	age := ""
	if !b.RecordedAt.IsZero() {
		age = fmt.Sprintf(", recorded %s", b.RecordedAt.Local().Format("2006-01-02"))
	}
	fmt.Fprintf(w, "Behaviour drift: %s%d breaking, %d compatible, against %s (%d run(s)%s)\n", tag, b.Breaking, b.Info, b.Snapshot, b.Runs, age)
	shown := 0
	for _, c := range b.Changes {
		if shown == maxUntestedShown {
			fmt.Fprintf(w, "  ... and more (all listed in the evidence JSON)\n")
			break
		}
		shown++
		where := c.Step
		if c.Path != "" {
			where += " " + c.Path
		}
		fmt.Fprintf(w, "  [%s] %s: %s\n", c.Severity, strings.TrimSpace(where), c.Message)
	}
	for _, n := range b.Notes {
		fmt.Fprintf(w, "  note: %s\n", n)
	}
}
