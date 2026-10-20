package runner

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"sleuth/internal/flow"
	"sleuth/internal/registry"
	"sleuth/internal/report"
)

// loadContracts loads every OpenAPI source referenced by the flow. A missing or
// unloadable source is a setup error: the run's verdicts would be meaningless.
func loadContracts(f *flow.Flow, env *EnvConfig) (map[string]*registry.Spec, error) {
	specs := map[string]*registry.Spec{}
	return specs, addContracts(f, env, specs)
}

// addContracts loads the contract sources a flow uses, and those of its subflows.
func addContracts(f *flow.Flow, env *EnvConfig, specs map[string]*registry.Spec) error {
	for _, n := range f.Nodes {
		if n.Sub != nil {
			if err := addContracts(n.Sub, env, specs); err != nil {
				return fmt.Errorf("node %s: %w", n.ID, err)
			}
		}
		if n.Contract == nil {
			continue
		}
		src := n.Contract.Source
		if _, done := specs[src]; done {
			continue
		}
		sp, err := LoadContract(env, src)
		if err != nil {
			return fmt.Errorf("node %s: %w", n.ID, err)
		}
		specs[src] = sp
	}
	return nil
}

// LoadContract resolves a contract source through the env file and loads it.
func LoadContract(env *EnvConfig, source string) (*registry.Spec, error) {
	loc, err := env.ContractLocation(source)
	if err != nil {
		return nil, err
	}
	sp, err := registry.Load(loc)
	if err != nil {
		return nil, fmt.Errorf("contract source %q: %w", source, err)
	}
	return sp, nil
}

type contractRun struct {
	spec *registry.Spec
	op   *registry.Operation
	res  *report.ContractResult
	warn bool
	// deferResponse: response findings are recorded but never block (see flow.Contract).
	deferResponse bool
}

func newContractRun(c *flow.Contract, specs map[string]*registry.Spec, method, path string) *contractRun {
	if c == nil {
		return nil
	}
	cr := &contractRun{spec: specs[c.Source], warn: c.OnViolation == "warn", deferResponse: c.DeferResponse}
	mode := "fail"
	if cr.warn {
		mode = "warn"
	}
	cr.res = &report.ContractResult{Source: c.Source, Mode: mode}
	op, ok := cr.spec.Find(method, path)
	if !ok {
		cr.violate("operation", "", fmt.Sprintf("%s %s is not in the contract", method, path))
		return cr
	}
	cr.op = op
	cr.res.Operation = op.Key
	return cr
}

func (c *contractRun) violate(where, path, msg string) {
	c.res.Violations = append(c.res.Violations, report.Violation{Where: where, Path: path, Message: msg})
}

func (c *contractRun) blocking() bool { return !c.warn && len(c.res.Violations) > 0 }

// blockingResponse is blocking() for the response stage.
func (c *contractRun) blockingResponse() bool { return !c.deferResponse && c.blocking() }

func (c *contractRun) err(stage string) error {
	v := c.res.Violations[0]
	return fmt.Errorf("%s: %d contract violation(s) against %s (first: [%s] %s %s)",
		stage, len(c.res.Violations), c.res.Source, v.Where, v.Path, v.Message)
}

func (c *contractRun) checkRequest(body any) {
	if c.op == nil {
		return
	}
	if c.op.RequestSchema == nil {
		if body != nil && len(c.op.RequestOther) > 0 {
			c.res.Notes = append(c.res.Notes, fmt.Sprintf("contract documents %s only for the %s request body; not validated",
				strings.Join(c.op.RequestOther, ", "), c.op.Key))
		}
		return
	}
	if body == nil {
		if c.op.RequestRequired {
			c.violate("request", "$", "request body is required by the contract but none was sent")
		}
		return
	}
	c.validate("request", c.op.RequestPtr, body)
}

func (c *contractRun) validate(where, ptr string, value any) {
	issues, err := c.spec.Validate(ptr, value)
	if err != nil { // the contract itself is unusable for this operation
		c.violate(where, "$", "cannot validate: "+err.Error())
		return
	}
	for _, is := range issues {
		c.violate(where, is.Path, is.Message)
	}
}

func (c *contractRun) checkResponse(status int, body any) {
	if c.op == nil {
		return
	}
	schema, documented := c.op.ResponseFor(status)
	if !documented {
		keys := make([]string, 0, len(c.op.Responses))
		for k := range c.op.Responses {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		c.violate("response", "$", fmt.Sprintf(undocumentedFmt,
			strconv.Itoa(status), c.op.Key, strings.Join(keys, ", ")))
		return
	}
	if other := c.op.ResponseOther[matchedStatus(c.op, status)]; schema == nil && len(other) > 0 {
		c.res.Notes = append(c.res.Notes, fmt.Sprintf("contract documents %s only for %s %d; response body not validated",
			strings.Join(other, ", "), c.op.Key, status))
		return
	}
	if schema == nil || isEmptySchema(schema) {
		c.res.Notes = append(c.res.Notes, fmt.Sprintf("contract has no schema for %s %d; response body not validated", c.op.Key, status))
		return
	}
	if body == nil {
		c.violate("response", "$", "contract expects a JSON body but the response had none")
		return
	}
	c.validate("response", c.op.ResponsePtrFor(status), body)
}

// matchedStatus is the Responses key ResponseFor would use for a status.
func matchedStatus(o *registry.Operation, status int) string {
	s := strconv.Itoa(status)
	for _, k := range []string{s, s[:1] + "XX", "default"} {
		if _, ok := o.Responses[k]; ok {
			return k
		}
	}
	return ""
}

const undocumentedFmt = "status %s is not documented for %s (documented: %s)"

// undocumented reports whether v is an undocumented-status finding, which a poll keeps
// from every attempt (other response findings only count on the satisfying attempt).
func undocumented(v report.Violation) bool {
	return v.Where == "response" && strings.Contains(v.Message, " is not documented for ")
}

func isEmptySchema(s any) bool {
	m, ok := s.(map[string]any)
	return ok && len(m) == 0
}
