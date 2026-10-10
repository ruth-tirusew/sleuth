// conduit runs API flow specs and emits evidence.
//
// Exit codes: 0 all passed, 1 a node failed, 2 usage/spec/config error.
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
		if r.Status != report.Passed {
			return 1
		}
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
	return 2
}
