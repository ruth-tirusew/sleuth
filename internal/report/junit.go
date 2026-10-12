package report

import (
	"encoding/xml"
	"fmt"
	"os"
	"strings"
)

// JUnit XML, the format most CI systems read for test results. One flow is one test suite and
// each step is a test case. A failing check, contract or poll is a <failure>; an unreachable
// system or a broken flow (infrastructure, spec) is an <error>; a skipped step is <skipped>.
// Run-level verdicts that no single step carries (contract drift against a saved spec baseline,
// behaviour drift against a shape snapshot, the run's timeout) are extra test cases, so a run that
// exits non-zero never shows as all green in CI.

type junitSuites struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Errors   int          `xml:"errors,attr"`
	Skipped  int          `xml:"skipped,attr"`
	Time     string       `xml:"time,attr"`
	Suites   []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name       string          `xml:"name,attr"`
	Tests      int             `xml:"tests,attr"`
	Failures   int             `xml:"failures,attr"`
	Errors     int             `xml:"errors,attr"`
	Skipped    int             `xml:"skipped,attr"`
	Time       string          `xml:"time,attr"`
	Timestamp  string          `xml:"timestamp,attr"`
	Properties []junitProperty `xml:"properties>property,omitempty"`
	Cases      []junitCase     `xml:"testcase"`
}

type junitProperty struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitProblem `xml:"failure,omitempty"`
	Error     *junitProblem `xml:"error,omitempty"`
	Skipped   *junitSkipped `xml:"skipped,omitempty"`
}

type junitProblem struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Body    string `xml:",chardata"`
}

type junitSkipped struct {
	Message string `xml:"message,attr"`
}

func seconds(ms int64) string { return fmt.Sprintf("%.3f", float64(ms)/1000) }

// JUnit renders the run as JUnit XML. Every text in it is masked with the run's secrets before it
// is XML-escaped (escaping would otherwise hide an "&" in a secret from the mask).
func JUnit(r *Run, secrets []string) ([]byte, error) {
	mask := func(s string) string { return MaskText(s, secrets) }
	suite := junitSuite{Name: r.Flow, Time: seconds(r.DurationMs), Timestamp: r.StartedAt.Format("2006-01-02T15:04:05Z")}
	if r.Env != "" {
		suite.Properties = append(suite.Properties, junitProperty{"env", r.Env})
	}
	if r.Parallel > 1 {
		suite.Properties = append(suite.Properties, junitProperty{"parallel", fmt.Sprint(r.Parallel)})
	}
	add := func(c junitCase) {
		suite.Tests++
		switch {
		case c.Failure != nil:
			suite.Failures++
		case c.Error != nil:
			suite.Errors++
		case c.Skipped != nil:
			suite.Skipped++
		}
		suite.Cases = append(suite.Cases, c)
	}

	for _, s := range r.Steps {
		c := junitCase{Name: s.ID, Classname: r.Flow + "." + s.Kind, Time: seconds(s.DurationMs)}
		switch s.Status {
		case Skipped:
			c.Skipped = &junitSkipped{Message: mask(s.Error)}
		case Failed:
			p := &junitProblem{Message: mask(s.Error), Type: s.ErrorKind, Body: mask(stepDetails(s))}
			if s.ErrorKind == "infrastructure" || s.ErrorKind == "spec" {
				c.Error = p
			} else {
				c.Failure = p
			}
		}
		add(c)
	}

	if d := r.Drift; d != nil {
		for _, sd := range d.SpecDrift {
			c := junitCase{Name: "spec drift: " + sd.Source, Classname: r.Flow + ".drift", Time: "0.000"}
			if sd.Failing {
				var b strings.Builder
				for _, ch := range sd.Changes {
					if ch.Severity != "info" {
						fmt.Fprintf(&b, "[%s] %s [%s] %s %s\n", ch.Severity, ch.Operation, ch.Where, ch.Path, ch.Message)
					}
				}
				c.Failure = &junitProblem{Type: "spec-drift", Message: fmt.Sprintf("%d breaking, %d to review against baseline %s", sd.Breaking, sd.Review, sd.Baseline), Body: mask(b.String())}
			}
			add(c)
		}
		if b := d.Behaviour; b != nil {
			c := junitCase{Name: "behaviour drift", Classname: r.Flow + ".drift", Time: "0.000"}
			if b.Failing {
				var sb strings.Builder
				for _, ch := range b.Changes {
					fmt.Fprintf(&sb, "[%s] %s %s: %s\n", ch.Severity, ch.Step, ch.Path, ch.Message)
				}
				c.Failure = &junitProblem{Type: "behaviour-drift", Message: fmt.Sprintf("%d breaking, %d compatible against %s", b.Breaking, b.Info, b.Snapshot), Body: sb.String()}
			}
			add(c)
		}
	}
	if r.TimedOut {
		add(junitCase{Name: "run timeout", Classname: r.Flow + ".run", Time: seconds(r.DurationMs),
			Error: &junitProblem{Type: "timeout", Message: "the run's timeout (" + r.Timeout + ") was reached before it finished"}})
	}

	doc := junitSuites{Name: "sleuth", Tests: suite.Tests, Failures: suite.Failures, Errors: suite.Errors, Skipped: suite.Skipped,
		Time: suite.Time, Suites: []junitSuite{suite}}
	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), append(out, '\n')...), nil
}

// stepDetails is the readable body of a failed test case: what was sent, what came back, and
// which checks failed.
func stepDetails(s Step) string {
	var b strings.Builder
	if s.Request != nil {
		fmt.Fprintf(&b, "%s %s\n", s.Request.Method, s.Request.URL)
	}
	if s.Response != nil {
		fmt.Fprintf(&b, "-> HTTP %d in %dms\n", s.Response.Status, s.Response.DurationMs)
	}
	for _, c := range s.Checks {
		if !c.Passed {
			fmt.Fprintf(&b, "check %s %s: expected %v, got %v", c.Path, c.Op, c.Expected, c.Actual)
			if c.Message != "" {
				fmt.Fprintf(&b, " (%s)", c.Message)
			}
			b.WriteString("\n")
		}
	}
	if c := s.Contract; c != nil {
		for _, v := range c.Violations {
			fmt.Fprintf(&b, "contract %s [%s] %s %s\n", c.Source, v.Where, v.Path, v.Message)
		}
	}
	if p := s.Poll; p != nil {
		fmt.Fprintf(&b, "poll: %d attempt(s), satisfied=%v\n", p.Attempts, p.Satisfied)
	}
	if s.Error != "" {
		b.WriteString(s.Error + "\n")
	}
	return b.String()
}

// WriteJUnit writes JUnit XML for the run to path.
func WriteJUnit(path string, r *Run, secrets []string) error {
	b, err := JUnit(r, secrets)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
