package report

import (
	"bytes"
	"strings"
	"testing"
)

func nested() []Step {
	return []Step{
		{ID: "a", Kind: "request", Status: Passed},
		{ID: "sub", Kind: "flow", Status: Failed, ErrorKind: "assertion", Flow: "_s.yaml", Children: []Step{
			{ID: "x", Kind: "request", Status: Passed},
			{ID: "deep", Kind: "flow", Status: Failed, Children: []Step{
				{ID: "y", Kind: "assertion", Status: Failed, ErrorKind: "assertion", Error: "no"},
				{ID: "z", Kind: "transform", Status: Skipped},
			}},
		}},
		{ID: "out", Kind: "flow", Status: Failed, ErrorKind: "spec", Error: "output broke", Children: []Step{
			{ID: "ok", Kind: "request", Status: Passed},
		}},
	}
}

func ids(ss []Step) string {
	var out []string
	for _, s := range ss {
		out = append(out, s.ID)
	}
	return strings.Join(out, ",")
}

func TestFlattenGivesPathIDsAndNoChildren(t *testing.T) {
	got := Flatten(nested())
	if ids(got) != "a,sub,sub/x,sub/deep,sub/deep/y,sub/deep/z,out,out/ok" {
		t.Errorf("%s", ids(got))
	}
	for _, s := range got {
		if s.Children != nil {
			t.Errorf("%s still has children", s.ID)
		}
	}
	if nested()[1].Children == nil {
		t.Error("Flatten must not change its input")
	}
}

func TestLeavesCountEachFailureOnce(t *testing.T) {
	// sub and sub/deep failed only because y failed, so they are not counted again;
	// out failed although its only child passed, so it is its own result
	if got := ids(Leaves(nested())); got != "a,sub/x,sub/deep/y,sub/deep/z,out,out/ok" {
		t.Errorf("%s", got)
	}
}

func TestPrintShowsNestedStepsIndented(t *testing.T) {
	var b bytes.Buffer
	Print(&b, &Run{Flow: "t", Status: Failed, Steps: nested()})
	out := b.String()
	for _, want := range []string{"sub", "_s.yaml", "\n    ✔ x ", "\n       ✘ y ", "6 steps"} {
		if !strings.Contains(out, want) && !strings.Contains(out, strings.TrimSpace(want)) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "      ✘ y") {
		t.Errorf("y should be indented two levels:\n%s", out)
	}
}

func TestJUnitListsLeavesOnce(t *testing.T) {
	b, err := JUnit(&Run{Flow: "t", Status: Failed, Steps: nested()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	if strings.Count(out, "<testcase") != 6 || !strings.Contains(out, `name="sub/deep/y"`) || strings.Contains(out, `name="sub"`) {
		t.Errorf("junit should list leaves by path:\n%s", out)
	}
}
