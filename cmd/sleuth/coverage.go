package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"sleuth/internal/report"
)

// coverageCmd combines the registry coverage of several runs' evidence files:
// sleuth coverage [-min N] [-list] [-json | -markdown] evidence.json...
//
// Exit codes: 0 fine, 1 a source is below -min, 2 usage error or unreadable evidence.
func coverageCmd(args []string) int {
	fs := flag.NewFlagSet("coverage", flag.ContinueOnError)
	min := fs.Int("min", 0, "fail (exit 1) if any contract source is exercised less than this percentage")
	list := fs.Bool("list", false, "list every operation no run called")
	asJSON := fs.Bool("json", false, "print the combined coverage as JSON")
	asMarkdown := fs.Bool("markdown", false, "print a Markdown table (for a CI job summary)")
	if err := fs.Parse(args); err != nil || fs.NArg() == 0 || *min < 0 || *min > 100 || (*asJSON && *asMarkdown) {
		fmt.Fprintln(os.Stderr, "usage: sleuth coverage [-min PERCENT] [-list] [-json | -markdown] evidence.json...")
		return 2
	}
	var runs []*report.Run
	for _, path := range fs.Args() {
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 2
		}
		var r report.Run
		if err := json.Unmarshal(b, &r); err != nil || r.Flow == "" {
			fmt.Fprintf(os.Stderr, "error: %s is not a Sleuth evidence file\n", path)
			return 2
		}
		runs = append(runs, &r)
	}
	combined, notes := report.CombineCoverage(runs)

	var below []string
	for _, c := range combined {
		if c.Percent < *min {
			below = append(below, fmt.Sprintf("%s: %d%% is below the minimum of %d%%", c.Source, c.Percent, *min))
		}
	}

	switch {
	case *asJSON:
		out := struct {
			Runs    int                       `json:"runs"`
			Sources []report.CombinedCoverage `json:"sources"`
			Notes   []string                  `json:"notes,omitempty"`
			Min     int                       `json:"min,omitempty"`
			Below   []string                  `json:"below,omitempty"`
		}{len(runs), combined, notes, *min, below}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
	case *asMarkdown:
		fmt.Printf("### Registry coverage (%d run(s))\n\n| Contract | Exercised | Total | Coverage | Flows |\n|---|---|---|---|---|\n", len(runs))
		for _, c := range combined {
			fmt.Printf("| %s | %d | %d | %d%% | %s |\n", c.Source, c.Exercised, c.Total, c.Percent, strings.Join(c.Flows, ", "))
		}
		if *list {
			for _, c := range combined {
				if len(c.Untested) > 0 {
					fmt.Printf("\n<details><summary>%s: %d operation(s) no flow called</summary>\n\n", c.Source, len(c.Untested))
					for _, op := range c.Untested {
						fmt.Printf("- `%s`\n", op)
					}
					fmt.Println("\n</details>")
				}
			}
		}
		for _, n := range append(notes, below...) {
			fmt.Printf("\n> %s\n", n)
		}
	default:
		fmt.Printf("Registry coverage across %d run(s)\n", len(runs))
		if len(combined) == 0 {
			fmt.Println("  no run used a contract")
		}
		for _, c := range combined {
			fmt.Printf("  %s: %d of %d operation(s) exercised (%d%%) by %s\n", c.Source, c.Exercised, c.Total, c.Percent, strings.Join(c.Flows, ", "))
			if *list {
				for _, op := range c.Untested {
					fmt.Printf("    not called: %s\n", op)
				}
			}
		}
		for _, n := range notes {
			fmt.Println("  note:", n)
		}
		for _, b := range below {
			fmt.Println("  FAIL", b)
		}
	}
	if len(below) > 0 {
		return 1
	}
	return 0
}
