// assay runs API flow specs and emits evidence.
//
// Exit codes:
//
//	0 all passed
//	1 assertion, contract or poll-timeout failure (the system responded wrongly / off-spec / never converged)
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

	"assay/internal/flow"
	"assay/internal/registry"
	"assay/internal/report"
	"assay/internal/runner"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, "usage: assay <run|validate|scaffold> [flags] ...")
		return 2
	}
	cmd := args[0]
	if cmd == "scaffold" {
		return scaffold(args[1:])
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	envPath := fs.String("env", "", "environment config file (vars + secret names)")
	outPath := fs.String("out", "", "evidence JSON path (default evidence/<flow>-<timestamp>.json)")
	parallel := fs.Int("parallel", 1, "run up to N independent nodes at once (order is set by needs: and nodes.<id> references only)")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 || *parallel < 1 {
		fmt.Fprintf(os.Stderr, "usage: assay %s [flags] <flow.yaml>\n", cmd)
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
		if *parallel > 1 {
			printWaves(f, *parallel)
		}
		return 0
	case "run":
		env, err := runner.LoadEnv(*envPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 2
		}
		r, secrets, err := runner.Run(f, env, runner.Options{EnvName: strings.TrimSuffix(filepath.Base(*envPath), filepath.Ext(*envPath)), Parallel: *parallel})
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 2
		}
		out := *outPath
		if out == "" {
			out = filepath.Join("evidence", fmt.Sprintf("%s-%s.json", f.Name, time.Now().UTC().Format("20060102T150405Z")))
		}
		err = os.MkdirAll(filepath.Dir(out), 0o755)
		if err == nil {
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

// printWaves shows what --parallel would run together, so a missing needs: shows up
// before a run rather than as a flaky one.
func printWaves(f *flow.Flow, n int) {
	waves, err := f.Waves()
	if err != nil {
		return
	}
	together := false
	for _, w := range waves {
		if len(w) > 1 {
			together = true
		}
	}
	if !together {
		fmt.Printf("parallel (up to %d): nothing to overlap, every node depends on the one before it\n", n)
		return
	}
	fmt.Printf("parallel (up to %d): nodes in the same wave may run at the same time\n", n)
	for i, w := range waves {
		fmt.Printf("  wave %d: %s\n", i+1, strings.Join(w, ", "))
	}
	fmt.Println("  anything that must run before another node needs `needs:` (or a nodes.<id> reference)")
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
	case kinds[runner.KindAssertion], kinds[runner.KindContract], kinds[runner.KindTimeout]:
		return 1
	case kinds[runner.KindInfra]:
		return 3
	}
	return 0
}

// scaffold prints a starter node (plus a status assertion) for one operation of a
// provider's OpenAPI document: assay scaffold -env e.yaml -source api "GET /users/{id}"
func scaffold(args []string) int {
	fs := flag.NewFlagSet("scaffold", flag.ContinueOnError)
	envPath := fs.String("env", "", "env file whose contracts: names the OpenAPI source")
	source := fs.String("source", "", "contract source name (as used in `contract:`)")
	specLoc := fs.String("spec", "", "OpenAPI file or URL (alternative to resolving -source via -env)")
	id := fs.String("id", "", "node id (default: derived from the operation)")
	list := fs.Bool("list", false, "list the operations in the contract instead")
	if err := fs.Parse(args); err != nil || *source == "" || (!*list && fs.NArg() != 1) {
		fmt.Fprintln(os.Stderr, `usage: assay scaffold -source NAME (-env env.yaml | -spec openapi.json) [-id ID] "GET /path"`)
		fmt.Fprintln(os.Stderr, "       assay scaffold -source NAME (-env ... | -spec ...) -list")
		return 2
	}
	var spec *registry.Spec
	var err error
	if *specLoc != "" {
		spec, err = registry.Load(*specLoc)
	} else {
		var env *runner.EnvConfig
		if env, err = runner.LoadEnv(*envPath); err == nil {
			spec, err = runner.LoadContract(env, *source)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	if *list {
		for _, k := range spec.Operations() {
			fmt.Println(k)
		}
		return 0
	}
	method, path, ok := strings.Cut(strings.TrimSpace(fs.Arg(0)), " ")
	if !ok {
		fmt.Fprintln(os.Stderr, `error: operation must look like "GET /path"`)
		return 2
	}
	op, found := spec.Find(method, path)
	if !found {
		fmt.Fprintf(os.Stderr, "error: %s %s is not in the contract (try -list)\n", method, path)
		return 2
	}
	nodeID := *id
	if nodeID == "" {
		nodeID = registry.NodeID(op.Key)
	}
	fmt.Print(spec.ScaffoldNode(op, *source, nodeID))
	return 0
}
