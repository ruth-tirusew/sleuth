package runner

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"conduit/internal/flow"
	"conduit/internal/registry"
	"conduit/internal/report"
)

// loadContracts loads every OpenAPI source referenced by the flow. A missing or
// unloadable source is a setup error: the run's verdicts would be meaningless.
func loadContracts(f *flow.Flow, env *EnvConfig) (map[string]*registry.Spec, error) {
	specs := map[string]*registry.Spec{}
	for _, n := range f.Nodes {
		if n.Contract == nil {
			continue
		}
		src := n.Contract.Source
		if _, done := specs[src]; done {
			continue
		}
		loc, ok := env.Contracts[src]
		if !ok {
			return nil, fmt.Errorf("node %s: contract source %q is not defined under contracts: in the env file", n.ID, src)
		}
		sp, err := registry.Load(loc)
		if err != nil {
			return nil, fmt.Errorf("contract source %q: %w", src, err)
		}
		specs[src] = sp
	}
	return specs, nil
}

type contractRun struct {
	spec *registry.Spec
	op   *registry.Operation
	res  *report.ContractResult
	warn bool
}

func newContractRun(c *flow.Contract, specs map[string]*registry.Spec, method, path string) *contractRun {
	if c == nil {
		return nil
	}
	cr := &contractRun{spec: specs[c.Source], warn: c.OnViolation == "warn"}
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

func (c *contractRun) err(stage string) error {
	v := c.res.Violations[0]
	return fmt.Errorf("%s: %d contract violation(s) against %s (first: [%s] %s %s)",
		stage, len(c.res.Violations), c.res.Source, v.Where, v.Path, v.Message)
}

func (c *contractRun) checkRequest(body any) {
	if c.op == nil || c.op.RequestSchema == nil {
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
		c.violate("response", "$", fmt.Sprintf("status %s is not documented for %s (documented: %s)",
			strconv.Itoa(status), c.op.Key, strings.Join(keys, ", ")))
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

func isEmptySchema(s any) bool {
	m, ok := s.(map[string]any)
	return ok && len(m) == 0
}
