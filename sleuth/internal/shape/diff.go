package shape

import (
	"fmt"
	"sort"
	"strings"
)

// Severity of a change, from the point of view of a client written against the snapshot.
const (
	Breaking = "breaking"
	Info     = "info"
)

// Change is one difference between a recorded shape and an observed one.
type Change struct {
	Step     string `json:"step,omitempty"`
	Severity string `json:"severity"`
	Path     string `json:"path,omitempty"`
	Message  string `json:"message"`
}

// Diff lists how an observed shape differs from a recorded one. Paths look like $.items[].price.
//
//   - a type the recording did not have (including null, or a decimal where there were only
//     integers) is breaking: a client may not handle it;
//   - a field the recording always had that is missing, or is now sometimes missing, is breaking;
//   - a field that was added is info;
//   - a type that was recorded but did not appear this time is not reported: that is variety in
//     the data, not a change.
func Diff(recorded, observed Shape) []Change {
	var out []Change
	diff(recorded, observed, "$", &out, 0)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return out[i].Severity == Breaking
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func diff(rec, obs Shape, path string, out *[]Change, depth int) {
	if depth > maxDepth {
		return
	}
	var added []string
	for _, t := range obs.Type {
		if !covers(rec.Type, t) {
			added = append(added, t)
		}
	}
	if len(added) > 0 {
		msg := fmt.Sprintf("type is now %s (recorded: %s)", strings.Join(added, " or "), strings.Join(rec.Type, " or "))
		if len(rec.Type) == 1 && len(obs.Type) == 1 {
			msg = fmt.Sprintf("type changed from %s to %s", rec.Type[0], obs.Type[0])
		}
		*out = append(*out, Change{Severity: Breaking, Path: path, Message: msg})
	}
	if has(rec.Type, TObject) && has(obs.Type, TObject) {
		for _, k := range sortedKeys(rec.Fields) {
			rf := rec.Fields[k]
			of, ok := obs.Fields[k]
			fp := path + "." + k
			switch {
			case !ok:
				if !rf.Optional {
					*out = append(*out, Change{Severity: Breaking, Path: fp, Message: "field is missing (the recording always had it)"})
				}
			case of.Optional && !rf.Optional:
				*out = append(*out, Change{Severity: Breaking, Path: fp, Message: "field is now sometimes missing (the recording always had it)"})
				diff(rf.Shape, of.Shape, fp, out, depth+1)
			default:
				diff(rf.Shape, of.Shape, fp, out, depth+1)
			}
		}
		for _, k := range sortedKeys(obs.Fields) {
			if _, ok := rec.Fields[k]; !ok {
				*out = append(*out, Change{Severity: Info, Path: path + "." + k, Message: "field added"})
			}
		}
	}
	if has(rec.Type, TArray) && has(obs.Type, TArray) && rec.Items != nil && obs.Items != nil {
		diff(*rec.Items, *obs.Items, path+"[]", out, depth+1)
	}
}

// covers reports whether the recorded types already allow t. Every integer is also a number.
func covers(recorded []string, t string) bool {
	return has(recorded, t) || (t == TInteger && has(recorded, TNumber))
}

func sortedKeys(m map[string]Field) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
