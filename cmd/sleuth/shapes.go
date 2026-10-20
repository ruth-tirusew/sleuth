package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime/debug"
	"sort"
	"time"

	"sleuth/internal/report"
	"sleuth/internal/shape"
)

// noRecord is returned by recordShapesFile when it deliberately wrote nothing because the run did
// not pass: the run's own exit code then stands, since that failure is already the signal.
const noRecord = -1

// recordShapesFile writes the snapshot of the shapes captured on r's steps to path.
//
//   - Only a passing run is recorded (unless force): a snapshot of a failing run would bless the failure.
//   - An existing snapshot is not replaced silently. If this run differs from it, the differences
//     are printed and nothing is written unless force (the old file is kept as path.prev) or merge
//     (widen the old snapshot with this run) is given.
//   - Recording over an identical snapshot just refreshes it.
func recordShapesFile(path string, r *report.Run, flow, flowSHA, env string, merge, force bool) int {
	if r.Status != report.Passed && !force {
		fmt.Fprintf(os.Stderr, "not recording %s: the run did not pass, and a snapshot of a failing run would bless the failure (-force overrides)\n", path)
		return noRecord
	}
	cur := map[string]shape.Entry{}
	for _, s := range report.Flatten(r.Steps) {
		if s.Shape != nil {
			cur[s.ID] = *s.Shape
		}
	}
	if len(cur) == 0 {
		fmt.Fprintf(os.Stderr, "error: not recording %s: no step returned JSON, so there is no shape to record\n", path)
		return 2
	}
	next := &shape.Snapshot{
		Format: shape.Format, Flow: flow, FlowSHA: flowSHA, Steps: cur,
		Recorded: shape.Provenance{At: time.Now().UTC().Truncate(time.Second), Tool: toolVersion(), Env: env, Runs: 1},
	}

	old, err := shape.Load(path)
	if errors.Is(err, fs.ErrNotExist) { // no snapshot yet
		return saveSnapshot(next, path, fmt.Sprintf("recorded %d step(s) to %s", len(cur), path))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}

	changes := shape.Compare(old, cur)
	if merge {
		merged, conflicts := shape.MergeSnapshots(old, next)
		if len(conflicts) > 0 {
			for _, c := range conflicts {
				fmt.Fprintln(os.Stderr, "error: cannot merge:", c)
			}
			return 2
		}
		if code := backup(path, old); code != 0 {
			return code
		}
		return saveSnapshot(merged, path, fmt.Sprintf("merged this run into %s (now %d run(s), %d step(s)); the previous snapshot is %s.prev", path, merged.Recorded.Runs, len(merged.Steps), path))
	}
	if len(changes) == 0 { // nothing to accept: just refresh the provenance
		return saveSnapshot(next, path, fmt.Sprintf("recorded %d step(s) to %s (no changes from the previous snapshot)", len(cur), path))
	}
	if !force {
		fmt.Fprintf(os.Stderr, "error: %s already exists (recorded %s) and this run differs from it. Accepting these changes is a decision, so nothing was written:\n",
			path, old.Recorded.At.Local().Format("2006-01-02"))
		printChangesTo(os.Stderr, changes)
		fmt.Fprintf(os.Stderr, "pass -force to replace it (the old one is kept as %s.prev), or -merge to widen it with this run\n", path)
		return 2
	}
	fmt.Printf("accepting %d change(s) from the snapshot recorded %s:\n", len(changes), old.Recorded.At.Local().Format("2006-01-02"))
	printChanges(changes)
	if code := backup(path, old); code != 0 {
		return code
	}
	return saveSnapshot(next, path, fmt.Sprintf("recorded %d step(s) to %s; the previous snapshot is %s.prev", len(cur), path, path))
}

func saveSnapshot(s *shape.Snapshot, path, msg string) int {
	if err := s.Save(path); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	fmt.Println(msg)
	return 0
}

// backup keeps the snapshot that is about to be replaced as path.prev.
func backup(path string, old *shape.Snapshot) int {
	if err := old.Save(path + ".prev"); err != nil {
		fmt.Fprintln(os.Stderr, "error: could not keep the previous snapshot:", err)
		return 2
	}
	return 0
}

func printChanges(cs []shape.Change) { printChangesTo(os.Stdout, cs) }

func printChangesTo(w *os.File, cs []shape.Change) {
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].Step < cs[j].Step })
	for _, c := range cs {
		where := c.Step
		if c.Path != "" {
			where += " " + c.Path
		}
		fmt.Fprintf(w, "  [%s] %s: %s\n", c.Severity, where, c.Message)
	}
}

// toolVersion identifies this build in a snapshot's provenance.
func toolVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	rev, dirty := "", ""
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) >= 7 {
				rev = s.Value[:7]
			}
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "+dirty"
			}
		}
	}
	if rev != "" {
		return "sleuth " + rev + dirty
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return "sleuth " + v
	}
	return "sleuth dev"
}
