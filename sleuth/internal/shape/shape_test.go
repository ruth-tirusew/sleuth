package shape

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func decode(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestInferTypesAndNumbers(t *testing.T) {
	s := Infer(decode(t, `{"i":1,"n":1.5,"e":1e2,"s":"x","b":true,"z":null,"o":{"a":1},"l":[1,2]}`))
	want := map[string]string{"i": "integer", "n": "number", "e": "number", "s": "string", "b": "boolean", "z": "null", "o": "object", "l": "array"}
	for k, typ := range want {
		if got := s.Fields[k].Type; len(got) != 1 || got[0] != typ {
			t.Errorf("%s: %v, want %s", k, got, typ)
		}
	}
	if Infer(float64(3)).Type[0] != TInteger || Infer(3.5).Type[0] != TNumber || Infer(7).Type[0] != TInteger {
		t.Error("float64 / int inputs: an integral float64 is an integer")
	}
}

func TestInferStoresNoValues(t *testing.T) {
	b, _ := json.Marshal(Infer(decode(t, `{"name":"alice-secret-name","card":"4111111111111111","items":[{"price":99.5}]}`)))
	for _, leak := range []string{"alice", "4111", "99.5"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("a shape must hold no values, but contains %q: %s", leak, b)
		}
	}
}

func TestArrayItemsAreMergedAndOptionalFieldsLearned(t *testing.T) {
	s := Infer(decode(t, `[{"id":1,"tag":"a"},{"id":2},{"id":3,"tag":null}]`))
	it := s.Items
	if it == nil || it.Fields["id"].Optional || !it.Fields["tag"].Optional {
		t.Fatalf("id is always present, tag is sometimes missing: %+v", it)
	}
	if got := it.Fields["tag"].Type; !reflect.DeepEqual(got, []string{"null", "string"}) {
		t.Errorf("tag types: %v", got)
	}
	if e := Infer(decode(t, `[]`)); e.Items != nil || e.Type[0] != TArray {
		t.Errorf("an empty array has no item shape: %+v", e)
	}
}

func TestMerge(t *testing.T) {
	a := Infer(decode(t, `{"id":1,"a":"x"}`))
	b := Infer(decode(t, `{"id":2.5,"b":true}`))
	m := Merge(a, b)
	if !reflect.DeepEqual(m.Fields["id"].Type, []string{"integer", "number"}) {
		t.Errorf("id: %v", m.Fields["id"].Type)
	}
	if !m.Fields["a"].Optional || !m.Fields["b"].Optional {
		t.Error("a field in only one observation is optional")
	}
	// a field is not "missing" from an observation that is not an object at all
	n := Merge(Infer(decode(t, `{"a":1}`)), Infer(decode(t, `null`)))
	if n.Fields["a"].Optional || !reflect.DeepEqual(n.Type, []string{"null", "object"}) {
		t.Errorf("object|null: %+v", n)
	}
}

func TestDiffFromTheClientsSide(t *testing.T) {
	rec := Infer(decode(t, `{"id":1,"name":"a","price":2,"tags":["x"],"owner":{"id":1},"note":"n"}`))
	cases := []struct {
		name     string
		observed string
		want     []string // "severity path" in order
	}{
		{"identical", `{"id":1,"name":"a","price":2,"tags":["x"],"owner":{"id":1},"note":"n"}`, nil},
		{"different values only", `{"id":99,"name":"zzz","price":3,"tags":["q","r"],"owner":{"id":7},"note":""}`, nil},
		{"field removed", `{"id":1,"price":2,"tags":["x"],"owner":{"id":1},"note":"n"}`, []string{"breaking $.name"}},
		{"type changed", `{"id":"1","name":"a","price":2,"tags":["x"],"owner":{"id":1},"note":"n"}`, []string{"breaking $.id"}},
		{"integer widened to a decimal", `{"id":1,"name":"a","price":2.5,"tags":["x"],"owner":{"id":1},"note":"n"}`, []string{"breaking $.price"}},
		{"null where a string was", `{"id":1,"name":null,"price":2,"tags":["x"],"owner":{"id":1},"note":"n"}`, []string{"breaking $.name"}},
		{"array item type changed", `{"id":1,"name":"a","price":2,"tags":[5],"owner":{"id":1},"note":"n"}`, []string{"breaking $.tags[]"}},
		{"nested field removed", `{"id":1,"name":"a","price":2,"tags":["x"],"owner":{},"note":"n"}`, []string{"breaking $.owner.id"}},
		{"field added is compatible", `{"id":1,"name":"a","price":2,"tags":["x"],"owner":{"id":1},"note":"n","extra":1}`, []string{"info $.extra"}},
		{"a number where an integer was is fine the other way", `{"id":1,"name":"a","price":2,"tags":[],"owner":{"id":1},"note":"n"}`, nil},
		{"became an array", `[1]`, []string{"breaking $"}},
	}
	for _, c := range cases {
		var got []string
		for _, ch := range Diff(rec, Infer(decode(t, c.observed))) {
			got = append(got, ch.Severity+" "+ch.Path)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestDiffIntegerSatisfiesNumberButNotTheOtherWay(t *testing.T) {
	number := Infer(decode(t, `1.5`))
	integer := Infer(decode(t, `1`))
	if d := Diff(number, integer); len(d) != 0 {
		t.Errorf("a recorded decimal already allows an integer: %v", d)
	}
	if d := Diff(integer, number); len(d) != 1 || d[0].Severity != Breaking {
		t.Errorf("a decimal where there were only integers is breaking: %v", d)
	}
}

func TestDiffOptionalFieldsAreNotBreakingWhenAbsent(t *testing.T) {
	rec := Infer(decode(t, `[{"id":1,"tag":"a"},{"id":2}]`)) // tag is optional
	if d := Diff(rec, Infer(decode(t, `[{"id":3}]`))); len(d) != 0 {
		t.Errorf("an optional field being absent is not a change: %v", d)
	}
	strict := Infer(decode(t, `[{"id":1,"tag":"a"}]`)) // tag always present
	d := Diff(strict, Infer(decode(t, `[{"id":1,"tag":"a"},{"id":2}]`)))
	if len(d) != 1 || !strings.Contains(d[0].Message, "sometimes missing") {
		t.Errorf("a field that was always present and is now sometimes missing is breaking: %v", d)
	}
}

func snapshotOf(status int, body string, t *testing.T) *Snapshot {
	return &Snapshot{Format: Format, Flow: "f", Recorded: Provenance{Runs: 1}, Steps: map[string]Entry{
		"a": {Of: "response", Status: status, Observations: 1, Shape: Infer(decode(t, body))},
	}}
}

func TestCompareStatusStepsAndMissing(t *testing.T) {
	snap := snapshotOf(200, `{"id":1}`, t)
	snap.Steps["gone"] = Entry{Of: "response", Status: 200, Shape: Infer(decode(t, `{}`))}
	obs := map[string]Entry{
		"a":   {Of: "response", Status: 404, Shape: Infer(decode(t, `{"error":"x"}`))},
		"new": {Of: "response", Status: 200, Shape: Infer(decode(t, `{}`))},
	}
	got := map[string]string{}
	for _, c := range Compare(snap, obs) {
		got[c.Step] = c.Severity + ":" + c.Path
	}
	if got["a"] != "breaking:status" || got["gone"] != "info:" || got["new"] != "info:" {
		t.Fatalf("%v", got)
	}
	// a different status ends the comparison for that step: the body of a 404 is not "drift"
	if n := len(Compare(snap, map[string]Entry{"a": obs["a"]})); n != 2 { // status + "gone" not run
		t.Errorf("expected only the status change and the unrun step, got %d changes", n)
	}
}

func TestMergeSnapshotsWidensAndReportsConflicts(t *testing.T) {
	old := snapshotOf(200, `{"id":1,"a":"x"}`, t)
	next := snapshotOf(200, `{"id":2,"b":true}`, t)
	next.Steps["extra"] = Entry{Of: "response", Status: 200, Shape: Infer(decode(t, `{}`))}
	m, conflicts := MergeSnapshots(old, next)
	if len(conflicts) != 0 || m.Recorded.Runs != 2 || m.Steps["a"].Observations != 2 || !m.Steps["a"].Shape.Fields["a"].Optional || m.Steps["extra"].Of == "" {
		t.Fatalf("%+v %v", m, conflicts)
	}
	_, conflicts = MergeSnapshots(old, snapshotOf(500, `{"error":"x"}`, t))
	if len(conflicts) != 1 || !strings.Contains(conflicts[0], "status") {
		t.Errorf("a different status cannot be merged into one shape: %v", conflicts)
	}
}

func TestSnapshotRoundTripAndRejectsOtherFiles(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "s.json")
	s := snapshotOf(200, `{"id":1,"l":[{"x":null}]}`, t)
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	back, err := Load(p)
	if err != nil || !reflect.DeepEqual(s.Steps, back.Steps) {
		t.Fatalf("%v\n%+v\n%+v", err, s.Steps, back.Steps)
	}
	other := filepath.Join(dir, "other.json")
	os.WriteFile(other, []byte(`{"openapi":"3.1.0"}`), 0o644)
	if _, err := Load(other); err == nil {
		t.Error("an OpenAPI document is not a shape snapshot")
	}
	os.WriteFile(other, []byte(`{"format":"sleuth-shapes/99","steps":{}}`), 0o644)
	if _, err := Load(other); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("an unknown version must be refused clearly: %v", err)
	}
}
