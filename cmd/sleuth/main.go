// sleuth runs API flow specs and emits evidence.
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
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sleuth/internal/flow"
	"sleuth/internal/registry"
	"sleuth/internal/report"
	"sleuth/internal/runner"
	"sleuth/internal/serve"
	"sleuth/internal/shape"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, "usage: sleuth <run|validate|scaffold|serve|diff|snapshot|coverage> [flags] ...")
		return 2
	}
	cmd := args[0]
	if cmd == "scaffold" {
		return scaffold(args[1:])
	}
	if cmd == "serve" {
		return serveDashboard(args[1:])
	}
	if cmd == "diff" {
		return diffSpecs(args[1:])
	}
	if cmd == "coverage" {
		return coverageCmd(args[1:])
	}
	if cmd == "snapshot" {
		return snapshotSpec(args[1:])
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	envPath := fs.String("env", "", "environment config file (vars + secret names)")
	outPath := fs.String("out", "", "evidence JSON path (default evidence/<flow>-<timestamp>.json)")
	parallel := fs.Int("parallel", 1, "run up to N independent nodes at once (order is set by needs: and nodes.<id> references only)")
	shapesPath := fs.String("shapes", "", "compare this run's response shapes with a recorded snapshot (see -record-shapes)")
	recordShapes := fs.String("record-shapes", "", "after a passing run, record the shape of each step's response to this file")
	mergeShapes := fs.Bool("merge", false, "with -record-shapes: widen an existing snapshot with this run instead of replacing it")
	force := fs.Bool("force", false, "with -record-shapes: replace an existing snapshot (the old one is kept as FILE.prev) or record from a failed run")
	shapeFail := fs.String("shape-drift-fail", "", "fail when responses differ from the snapshot by at least this severity: breaking | any (default: report only)")
	runTimeout := fs.Duration("timeout", 0, "stop the whole run after this long (e.g. 10m): work in flight is stopped, unstarted nodes are skipped, exit 3")
	defaultCoverage := report.CoverageList
	if os.Getenv("CI") == "true" { // set by GitHub Actions, GitLab CI and most CI systems
		defaultCoverage = report.CoverageCount
	}
	coverageMode := fs.String("coverage", defaultCoverage, "how much registry coverage the printed report shows: list | count | off (default count when CI=true); the evidence always has all of it")
	junitPath := fs.String("junit", "", "also write the results as JUnit XML to this file, for CI")
	specFail := fs.String("spec-drift-fail", "", "fail when a contract source differs from its saved baseline by at least this severity: breaking | review (default: report only)")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 1 || *parallel < 1 || (*specFail != "" && *specFail != "breaking" && *specFail != "review") ||
		(*shapeFail != "" && *shapeFail != "breaking" && *shapeFail != "any") || (*shapeFail != "" && *shapesPath == "") ||
		((*mergeShapes || *force) && *recordShapes == "") || *runTimeout < 0 ||
		(*coverageMode != report.CoverageList && *coverageMode != report.CoverageCount && *coverageMode != report.CoverageOff) {
		fmt.Fprintf(os.Stderr, "usage: sleuth %s [flags] <flow.yaml>\n", cmd)
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
		// From here on anything printed can carry a secret (a URL in an error, a value in a
		// failed check), so it all goes through the same masking as the evidence file.
		known := env.SecretValues()
		fail := func(prefix string, err error) int {
			fmt.Fprintln(os.Stderr, report.MaskText(prefix+err.Error(), known))
			return 2
		}
		var snap *shape.Snapshot
		if *shapesPath != "" { // a snapshot that cannot be read is a setup error, before anything runs
			if snap, err = shape.Load(*shapesPath); err != nil {
				return fail("error: ", err)
			}
		}
		flowBytes, _ := os.ReadFile(fs.Arg(0))
		flowSHA := fmt.Sprintf("%x", sha256.Sum256(flowBytes))
		envName := strings.TrimSuffix(filepath.Base(*envPath), filepath.Ext(*envPath))
		r, secrets, err := runner.Run(f, env, runner.Options{EnvName: envName, Parallel: *parallel, FailOnSpecDrift: *specFail,
			CaptureShapes: snap != nil || *recordShapes != "", Timeout: *runTimeout})
		if err != nil {
			return fail("error: ", err)
		}
		if snap != nil {
			report.AddBehaviourDrift(r, report.BehaviourInput{Path: *shapesPath, Snap: snap, FailOn: *shapeFail, Flow: f.Name, FlowSHA: flowSHA, Now: time.Now()})
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
			return fail("error writing evidence: ", err)
		}
		if *junitPath != "" {
			if err := report.WriteJUnit(*junitPath, r, append(known, secrets...)); err != nil {
				return fail("error writing JUnit XML: ", err)
			}
		}
		var printed strings.Builder
		report.PrintWith(&printed, r, report.PrintOptions{Coverage: *coverageMode})
		fmt.Print(report.MaskText(printed.String(), append(known, secrets...)))
		fmt.Println("evidence:", out)
		if *recordShapes != "" {
			if code := recordShapesFile(*recordShapes, r, f.Name, flowSHA, envName, *mergeShapes, *force); code != 0 {
				if code == noRecord { // the run's own result is still the exit code
					return exitCode(r)
				}
				return code
			}
		}
		return exitCode(r)
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
	return 2
}

// snapshotSpec saves the live OpenAPI document of a contract source as its baseline, so later runs can
// report what changed since: sleuth snapshot -env e.yaml -source NAME [-out file] [-force]
func snapshotSpec(args []string) int {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	envPath := fs.String("env", "", "env file whose contracts: names the source")
	source := fs.String("source", "", "contract source name")
	out := fs.String("out", "", "where to write it (default: the source's baselines: entry, else baselines/<source>.openapi.json next to the env file)")
	force := fs.Bool("force", false, "overwrite an existing baseline")
	if err := fs.Parse(args); err != nil || *envPath == "" || *source == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: sleuth snapshot -env env.yaml -source NAME [-out file] [-force]")
		return 2
	}
	env, err := runner.LoadEnv(*envPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	loc, err := env.ContractLocation(*source)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	configured, err := env.BaselineLocation(*source)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	dest := *out
	if dest == "" {
		dest = configured
	}
	if dest == "" {
		dest = filepath.Join(filepath.Dir(*envPath), "baselines", *source+".openapi.json")
	}
	raw, err := registry.Fetch(loc)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: contract source", *source+":", err)
		return 2
	}
	sp, err := registry.Parse(loc, raw)
	if err != nil { // never save something that is not an OpenAPI document
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	if _, err := os.Stat(dest); err == nil && !*force {
		fmt.Fprintf(os.Stderr, "error: %s already exists. It is what drift is measured against; review `sleuth diff %s %s` first, then pass -force to replace it.\n", dest, dest, loc)
		return 2
	}
	if asJSON, err := registry.ToJSON(raw); err == nil { // a YAML spec is saved as JSON, like any baseline
		raw = asJSON
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		pretty.Reset()
		pretty.Write(raw)
	}
	pretty.WriteByte('\n')
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	if err := os.WriteFile(dest, pretty.Bytes(), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	fmt.Printf("saved %s (%d operations) to %s\n", *source, len(sp.Operations()), dest)
	if configured == "" {
		rel, relErr := filepath.Rel(filepath.Dir(*envPath), dest)
		if relErr != nil {
			rel = dest
		}
		fmt.Printf("add to %s so runs compare against it:\n  baselines:\n    %s: %s\n", *envPath, *source, rel)
	}
	return 0
}

// diffSpecs compares two versions of a published OpenAPI document and reports what a client
// written against the old one would trip over: sleuth diff [-fail-on breaking|review|never] OLD NEW
func diffSpecs(args []string) int {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	failOn := fs.String("fail-on", "breaking", "exit 1 when there is a change of this severity or worse: breaking | review | never")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 2 || (*failOn != "breaking" && *failOn != "review" && *failOn != "never") {
		fmt.Fprintln(os.Stderr, "usage: sleuth diff [-fail-on breaking|review|never] [-json] OLD NEW   (each a file path or http(s) URL of an OpenAPI 3.x document)")
		return 2
	}
	oldS, err := registry.Load(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: old:", err)
		return 2
	}
	newS, err := registry.Load(fs.Arg(1))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: new:", err)
		return 2
	}
	changes := registry.Diff(oldS, newS)
	count := map[registry.Severity]int{}
	for _, c := range changes {
		count[c.Severity]++
	}
	if *asJSON {
		b, _ := json.MarshalIndent(map[string]any{
			"old": fs.Arg(0), "new": fs.Arg(1), "changes": changes,
			"breaking": count[registry.Breaking], "review": count[registry.Review], "info": count[registry.Info],
		}, "", "  ")
		fmt.Println(string(b))
	} else {
		fmt.Printf("Comparing %s -> %s\n", fs.Arg(0), fs.Arg(1))
		if len(changes) == 0 {
			fmt.Println("\nNo differences that affect a client.")
		}
		last := registry.Severity("")
		for _, c := range changes {
			if c.Severity != last {
				last = c.Severity
				fmt.Printf("\n%s (%d)\n", strings.ToUpper(string(c.Severity)), count[c.Severity])
			}
			where := c.Operation
			if where == "" {
				where = "(document)"
			}
			loc := ""
			if c.Where != "operation" {
				loc = " [" + c.Where + "] " + c.Path
			}
			fmt.Printf("  %s%s  %s\n", where, strings.TrimRight(loc, " "), c.Message)
		}
		fmt.Printf("\n%d breaking, %d to review, %d compatible\n", count[registry.Breaking], count[registry.Review], count[registry.Info])
	}
	if (*failOn == "breaking" && count[registry.Breaking] > 0) || (*failOn == "review" && count[registry.Breaking]+count[registry.Review] > 0) {
		return 1
	}
	return 0
}

// serveDashboard runs the local web dashboard: sleuth serve [-dir flows] [-env env.yaml]
func serveDashboard(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	dir := fs.String("dir", ".", "directory whose .yaml/.yml files are the flows")
	envPath := fs.String("env", "", "default env file, for flows without a paired <name>.env.yaml next to them")
	addr := fs.String("addr", "127.0.0.1:8787", "listen address (loopback only)")
	evidence := fs.String("evidence", "evidence", "directory for each run's evidence JSON (empty: don't write)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: sleuth serve [-dir flows] [-env env.yaml] [-addr 127.0.0.1:8787] [-evidence dir]")
		return 2
	}
	if err := serve.CheckAddr(*addr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	if _, err := os.Stat(*dir); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	if *envPath != "" { // a typo here is a startup error; unset variables are reported per flow on the page
		if st, err := os.Stat(*envPath); err != nil || st.IsDir() {
			fmt.Fprintln(os.Stderr, "error: -env", *envPath, "is not a readable file")
			return 2
		}
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	token := hex.EncodeToString(raw[:])
	srv := serve.New(serve.Config{Dir: *dir, EnvPath: *envPath, EvidenceDir: *evidence, Token: token})
	fmt.Printf("sleuth dashboard: http://%s/#token=%s\n", *addr, token)
	fmt.Println("runs flows with this process's secrets; only this machine can reach it, and only with that token. Ctrl-C to stop.")
	if err := srv.Serve(*addr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	return 0
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
	if r.Drift != nil && r.Drift.Behaviour != nil && r.Drift.Behaviour.Failing {
		kinds[runner.KindContract] = true // the system answered, but not as it did when the snapshot was recorded
	}
	if r.Drift != nil {
		for _, sd := range r.Drift.SpecDrift {
			if sd.Failing { // the spec moved under the flow: the same class as a contract failure
				kinds[runner.KindContract] = true
			}
		}
	}
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
// provider's OpenAPI document: sleuth scaffold -env e.yaml -source api "GET /users/{id}"
func scaffold(args []string) int {
	fs := flag.NewFlagSet("scaffold", flag.ContinueOnError)
	envPath := fs.String("env", "", "env file whose contracts: names the OpenAPI source")
	source := fs.String("source", "", "contract source name (as used in `contract:`)")
	specLoc := fs.String("spec", "", "OpenAPI file or URL (alternative to resolving -source via -env)")
	id := fs.String("id", "", "node id (default: derived from the operation)")
	list := fs.Bool("list", false, "list the operations in the contract instead")
	if err := fs.Parse(args); err != nil || *source == "" || (!*list && fs.NArg() != 1) {
		fmt.Fprintln(os.Stderr, `usage: sleuth scaffold -source NAME (-env env.yaml | -spec openapi.json) [-id ID] "GET /path"`)
		fmt.Fprintln(os.Stderr, "       sleuth scaffold -source NAME (-env ... | -spec ...) -list")
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
