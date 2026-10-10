// conduit runs API flow specs and emits evidence.
//
// Exit codes:
//
//	0 all passed
//	1 assertion or contract failure (the system responded wrongly / off-spec)
//	2 usage, config, secret or flow-spec error (results can't be trusted)
//	3 infrastructure failure only (timeout/connection; safe for CI to retry)
//
// When a run has several kinds of failure, 2 wins over 1, and 1 over 3.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"conduit/internal/flow"
	"conduit/internal/report"
	"conduit/internal/runner"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, "usage: conduit <run|validate> [flags] <flow.yaml>")
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	envPath := fs.String("env", "", "environment config file (vars + secret names)")
	outPath := fs.String("out", "", "evidence JSON path (default evidence/<flow>-<timestamp>.json)")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "usage: conduit %s [flags] <flow.yaml>\n", cmd)
		return 2
	}
	f, err := flow.Load(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	switch cmd {
	case "validate":
		order, _ := f.Order()
		ids := make([]string, len(order))
		for i, n := range order {
			ids[i] = n.ID
		}
		fmt.Printf("OK: %s (%s)\n", f.Name, strings.Join(ids, " -> "))
		return 0
	case "run":
		env, err := runner.LoadEnv(*envPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 2
		}
		r, secrets, err := runner.Run(f, env, runner.Options{EnvName: strings.TrimSuffix(filepath.Base(*envPath), filepath.Ext(*envPath))})
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 2
		}
		out := *outPath
		if out == "" {
			out = filepath.Join("evidence", fmt.Sprintf("%s-%s.json", f.Name, time.Now().UTC().Format("20060102T150405Z")))
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err == nil {
			err = report.WriteJSON(out, r, secrets)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "error writing evidence:", err)
			return 2
		}
		report.Print(os.Stdout, r)
		fmt.Println("evidence:", out)
		return exitCode(r)
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
	return 2
}

func exitCode(r *report.Run) int {
	kinds := map[string]bool{}
	for _, s := range r.Steps {
		if s.Status == report.Failed {
			kinds[s.ErrorKind] = true
		}
	}
	switch {
	case kinds[runner.KindSpec]:
		return 2
	case kinds[runner.KindAssertion], kinds[runner.KindContract]:
		return 1
	case kinds[runner.KindInfra]:
		return 3
	}
	return 0
}
