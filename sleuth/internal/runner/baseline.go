package runner

import (
	"fmt"
	"sort"

	"sleuth/internal/registry"
	"sleuth/internal/report"
)

// diffBaselines compares, for every contract source the flow uses that has a saved baseline,
// the baseline with the live document, and returns the differences. A baseline that cannot
// be read is a setup error: a drift check that silently did not run would be worse than none.
func diffBaselines(env *EnvConfig, specs map[string]*registry.Spec) ([]report.SpecDrift, error) {
	sources := make([]string, 0, len(specs))
	for name := range specs {
		sources = append(sources, name)
	}
	sort.Strings(sources)

	var out []report.SpecDrift
	for _, name := range sources {
		loc, err := env.BaselineLocation(name)
		if err != nil {
			return nil, err
		}
		if loc == "" {
			continue
		}
		base, err := registry.Load(loc)
		if err != nil {
			return nil, fmt.Errorf("baseline for contract source %q: %w", name, err)
		}
		d := report.SpecDrift{Source: name, Baseline: loc, Changes: []report.SpecChange{}}
		for _, c := range registry.Diff(base, specs[name]) {
			d.Changes = append(d.Changes, report.SpecChange{
				Severity: string(c.Severity), Operation: c.Operation, Where: c.Where, Path: c.Path, Message: c.Message,
			})
		}
		out = append(out, d)
	}
	return out, nil
}
