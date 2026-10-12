package report

import (
	"fmt"
	"sort"
	"strings"
)

// CombinedCoverage is registry coverage for one contract source across several runs: an
// operation counts as exercised if any of the runs called it.
type CombinedCoverage struct {
	Source    string   `json:"source"`
	Total     int      `json:"total"`
	Exercised int      `json:"exercised"`
	Percent   int      `json:"percent"`
	Flows     []string `json:"flows"`              // the runs that used this source
	Called    []string `json:"called,omitempty"`   // "METHOD /path", sorted
	Untested  []string `json:"untested,omitempty"` // sorted
}

// CombineCoverage merges the registry coverage of several runs, per contract source. Runs that
// used no contract are ignored. If the runs disagree about what a source publishes (the
// contract changed between them), coverage is computed over everything any of them saw, and a
// note says so. Evidence written before runs recorded the called operations is combined from
// what each run did not call.
func CombineCoverage(runs []*Run) ([]CombinedCoverage, []string) {
	type acc struct {
		universe map[string]bool
		called   map[string]bool
		untested []map[string]bool // per run, for evidence without a called list
		legacy   bool
		totals   map[int]bool
		sets     map[string]bool // each run's full operation list, to tell if the contract changed
		flows    []string
	}
	bySource := map[string]*acc{}
	var notes []string
	for _, r := range runs {
		if r.Drift == nil || len(r.Drift.Sources) == 0 {
			notes = append(notes, fmt.Sprintf("%s: used no contract, so it adds no coverage", r.Flow))
			continue
		}
		for _, sc := range r.Drift.Sources {
			a := bySource[sc.Source]
			if a == nil {
				a = &acc{universe: map[string]bool{}, called: map[string]bool{}, totals: map[int]bool{}, sets: map[string]bool{}}
				bySource[sc.Source] = a
			}
			a.flows = append(a.flows, r.Flow)
			a.totals[sc.Total] = true
			un := map[string]bool{}
			for _, op := range sc.Untested {
				a.universe[op] = true
				un[op] = true
			}
			a.untested = append(a.untested, un)
			for _, op := range sc.Called {
				a.universe[op] = true
				a.called[op] = true
			}
			if sc.Exercised > 0 && len(sc.Called) == 0 {
				a.legacy = true
			} else {
				ops := append(append([]string(nil), sc.Called...), sc.Untested...)
				sort.Strings(ops)
				a.sets[strings.Join(ops, "\n")] = true
			}
		}
	}

	names := make([]string, 0, len(bySource))
	for n := range bySource {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []CombinedCoverage
	for _, name := range names {
		a := bySource[name]
		if len(a.totals) > 1 || len(a.sets) > 1 {
			notes = append(notes, fmt.Sprintf("%s: the runs saw different versions of the contract; coverage is over every operation any of them saw", name))
		}
		if a.legacy {
			// some runs did not record what they called: an operation is untested only if no run
			// listed it as called and every run listed it as untested
			for op := range a.universe {
				untestedEverywhere := true
				for _, un := range a.untested {
					if !un[op] {
						untestedEverywhere = false
						break
					}
				}
				if !untestedEverywhere {
					a.called[op] = true
				}
			}
			notes = append(notes, fmt.Sprintf("%s: some evidence predates per-operation records; combined from what each run did not call", name))
		}
		c := CombinedCoverage{Source: name, Flows: a.flows}
		for op := range a.universe {
			if a.called[op] {
				c.Called = append(c.Called, op)
			} else {
				c.Untested = append(c.Untested, op)
			}
		}
		sort.Strings(c.Called)
		sort.Strings(c.Untested)
		c.Total, c.Exercised = len(a.universe), len(c.Called)
		if a.legacy {
			// an operation every old run called appears in no list at all: take the total from the
			// recorded counts instead, and count as exercised whatever is not untested everywhere
			for t := range a.totals {
				if t > c.Total {
					c.Total = t
				}
			}
			c.Exercised = c.Total - len(c.Untested)
		}
		if c.Total > 0 {
			c.Percent = c.Exercised * 100 / c.Total
		} else {
			c.Percent = 100
		}
		out = append(out, c)
	}
	return out, notes
}
