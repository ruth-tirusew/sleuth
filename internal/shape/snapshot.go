package shape

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Format identifies the snapshot file format.
const Format = "sleuth-shapes/1"

// Snapshot is a recorded set of response shapes for one flow, keyed by step id. It holds no values.
type Snapshot struct {
	Format   string           `json:"format"`
	Flow     string           `json:"flow"`
	FlowSHA  string           `json:"flow_sha256,omitempty"`
	Recorded Provenance       `json:"recorded"`
	Steps    map[string]Entry `json:"steps"`
}

// Provenance says where a snapshot came from.
type Provenance struct {
	At   time.Time `json:"at"`
	Tool string    `json:"tool,omitempty"`
	Env  string    `json:"env,omitempty"`
	Runs int       `json:"runs"` // how many runs were merged into it
}

// Entry is what one step returned: the HTTP status and shape of its response, or the shape of
// the rows of a database step.
type Entry struct {
	Of           string `json:"of"` // response | rows
	Status       int    `json:"status,omitempty"`
	Operation    string `json:"operation,omitempty"` // "GET /users/{id}", when the step is bound to a contract
	Observations int    `json:"observations,omitempty"`
	Shape        Shape  `json:"shape"`
}

// Load reads a snapshot file.
func Load(path string) (*Snapshot, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: not a snapshot: %w", path, err)
	}
	if !strings.HasPrefix(s.Format, "sleuth-shapes/") {
		return nil, fmt.Errorf("%s: not a Sleuth shape snapshot (format %q)", path, s.Format)
	}
	if s.Format != Format {
		return nil, fmt.Errorf("%s: snapshot format %q is not supported by this version (%s)", path, s.Format, Format)
	}
	if s.Steps == nil {
		s.Steps = map[string]Entry{}
	}
	return &s, nil
}

// Save writes the snapshot as indented JSON, creating the directory if needed.
func (s *Snapshot) Save(path string) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// Compare lists how the observed entries differ from the snapshot, step by step.
//
// A different HTTP status is breaking and ends the comparison for that step (it is a different
// response, so its body is not compared). A step in the snapshot that produced nothing, and a
// step with no snapshot, are info.
func Compare(snap *Snapshot, observed map[string]Entry) []Change {
	var out []Change
	ids := map[string]bool{}
	for id := range snap.Steps {
		ids[id] = true
	}
	for id := range observed {
		ids[id] = true
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)

	for _, id := range sorted {
		rec, hasRec := snap.Steps[id]
		obs, hasObs := observed[id]
		switch {
		case hasRec && !hasObs:
			out = append(out, Change{Step: id, Severity: Info, Message: "recorded in the snapshot, but produced no response this run (skipped, failed before a response, or not JSON)"})
		case !hasRec && hasObs:
			out = append(out, Change{Step: id, Severity: Info, Message: "no snapshot for this step"})
		case rec.Of != obs.Of:
			out = append(out, Change{Step: id, Severity: Breaking, Message: fmt.Sprintf("the step now returns %s, the snapshot recorded %s", obs.Of, rec.Of)})
		case rec.Status != obs.Status:
			out = append(out, Change{Step: id, Severity: Breaking, Path: "status", Message: fmt.Sprintf("HTTP status changed from %d to %d", rec.Status, obs.Status)})
		default:
			for _, c := range Diff(rec.Shape, obs.Shape) {
				c.Step = id
				out = append(out, c)
			}
		}
	}
	return out
}

// MergeSnapshots widens old with the entries of next: shapes are merged, observations added. Steps that
// are in only one of them are kept. A step whose HTTP status or kind differs between the two
// cannot be merged into one shape; those are returned as conflicts and the step is left as it was.
func MergeSnapshots(old, next *Snapshot) (*Snapshot, []string) {
	out := &Snapshot{Format: Format, Flow: next.Flow, FlowSHA: next.FlowSHA, Recorded: next.Recorded, Steps: map[string]Entry{}}
	out.Recorded.Runs = old.Recorded.Runs + next.Recorded.Runs
	var conflicts []string
	for id, e := range old.Steps {
		out.Steps[id] = e
	}
	ids := make([]string, 0, len(next.Steps))
	for id := range next.Steps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		n := next.Steps[id]
		o, ok := old.Steps[id]
		if !ok {
			out.Steps[id] = n
			continue
		}
		if o.Of != n.Of || o.Status != n.Status {
			conflicts = append(conflicts, fmt.Sprintf("step %s: the snapshot has %s with status %d, this run has %s with status %d", id, o.Of, o.Status, n.Of, n.Status))
			continue
		}
		o.Shape = Merge(o.Shape, n.Shape)
		o.Observations += n.Observations
		if n.Operation != "" {
			o.Operation = n.Operation
		}
		out.Steps[id] = o
	}
	return out, conflicts
}
