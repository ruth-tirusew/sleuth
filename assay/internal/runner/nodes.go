package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"time"

	"conduit/internal/flow"
	"conduit/internal/registry"
	"conduit/internal/report"
)

var httpClient = &http.Client{}

func execRequest(ctx *Context, n flow.Node, step *report.Step, specs map[string]*registry.Spec) (map[string]any, error) {
	r := n.Request
	method := strings.ToUpper(r.Method)
	if method == "" {
		method = "GET"
	}
	rawURL, err := ctx.RenderString(r.URL)
	if err != nil {
		return nil, specErr(err)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, specErr(err)
	}
	if len(r.Query) > 0 {
		q := u.Query()
		for k, v := range r.Query {
			rv, err := ctx.RenderString(v)
			if err != nil {
				return nil, specErr(err)
			}
			q.Set(k, rv)
		}
		u.RawQuery = q.Encode()
	}
	headers := map[string]string{}
	for k, v := range r.Headers {
		if headers[k], err = ctx.RenderString(v); err != nil {
			return nil, specErr(err)
		}
	}
	body, err := ctx.Render(r.Body)
	if err != nil {
		return nil, specErr(err)
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, specErr(err)
		}
		reader = bytes.NewReader(b)
		if _, ok := headers["Content-Type"]; !ok {
			headers["Content-Type"] = "application/json"
		}
	}
	step.Request = &report.HTTPRequest{Method: method, URL: u.String(), Headers: headers, Body: body}

	cr := newContractRun(n.Contract, specs, method, u.Path)
	if cr != nil {
		step.Contract = cr.res
		cr.checkRequest(body)
		if cr.blocking() {
			return nil, contractErr(cr.err("request not sent"))
		}
	}

	timeout := 30 * time.Second
	if r.Timeout != "" {
		if timeout, err = time.ParseDuration(r.Timeout); err != nil {
			return nil, specErr(fmt.Errorf("bad timeout: %w", err))
		}
	}
	req, err := http.NewRequest(method, u.String(), reader)
	if err != nil {
		return nil, specErr(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := *httpClient
	client.Timeout = timeout
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, transportErr(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, transportErr(err)
	}
	var parsed any
	if json.Unmarshal(raw, &parsed) != nil {
		parsed = nil
	}
	respHeaders := map[string]string{}
	hdrAny := map[string]any{}
	for k := range resp.Header {
		respHeaders[k] = resp.Header.Get(k)
		hdrAny[strings.ToLower(k)] = resp.Header.Get(k)
	}
	evBody := parsed
	if parsed == nil {
		evBody = string(raw)
	}
	step.Response = &report.HTTPResponse{
		Status: resp.StatusCode, Headers: respHeaders, Body: evBody,
		DurationMs: time.Since(start).Milliseconds(),
	}
	response := map[string]any{
		"status": float64(resp.StatusCode), "headers": hdrAny, "raw": string(raw),
		"duration_ms": float64(step.Response.DurationMs),
	}
	if parsed != nil { // body is absent for non-JSON responses
		response["body"] = parsed
	}
	if cr != nil {
		cr.checkResponse(resp.StatusCode, parsed)
		if cr.blocking() {
			return nil, contractErr(cr.err("response"))
		}
	}
	out, err := extractOutputs(response, r.Extract)
	if err != nil {
		return nil, err
	}
	step.Output = out
	return map[string]any{"response": response, "out": out}, nil
}

// extractOutputs reads named values out of a response (paths like body.token).
func extractOutputs(response map[string]any, extract map[string]string) (map[string]any, error) {
	out := map[string]any{}
	for name, path := range extract {
		v, ok := lookup(response, path)
		if !ok {
			raw, _ := response["raw"].(string)
			return nil, assertErr(fmt.Errorf("extract %q: path %q not found in response (HTTP %.0f, body: %s)",
				name, path, response["status"], snippet([]byte(raw))))
		}
		out[name] = v
	}
	return out, nil
}

func execTransform(ctx *Context, n flow.Node, step *report.Step) (map[string]any, error) {
	out := map[string]any{}
	for k, v := range n.Transform.Set {
		r, err := ctx.Render(v)
		if err != nil {
			return nil, specErr(fmt.Errorf("set %q: %w", k, err))
		}
		out[k] = r
	}
	step.Output = out
	return map[string]any{"out": out}, nil
}

func execAssertion(ctx *Context, n flow.Node, step *report.Step) (map[string]any, error) {
	results, failed, err := evalChecks(ctx, n.Assert)
	step.Checks = results
	if err != nil {
		return nil, err
	}
	if failed > 0 {
		return nil, assertErr(fmt.Errorf("%d assertion(s) failed", failed))
	}
	return map[string]any{"passed": true}, nil
}

// evalChecks evaluates checks against the run context. A non-nil error means the
// checks themselves are broken (bad template/regex); failed counts unmet checks.
func evalChecks(ctx *Context, checks []flow.Check) (results []report.CheckResult, failed int, err error) {
	add := func(c flow.Check, op string, expected, actual any, ok bool) {
		results = append(results, report.CheckResult{
			Path: c.Path, Op: op, Expected: expected, Actual: actual, Passed: ok, Message: c.Message,
		})
		if !ok {
			failed++
		}
	}
	for _, c := range checks {
		actual, found := ctx.Lookup(c.Path)
		if c.Exists != nil {
			add(c, "exists", *c.Exists, found, found == *c.Exists)
		}
		if c.Equals != nil {
			exp, err := ctx.Render(c.Equals)
			if err != nil {
				return results, failed, specErr(err)
			}
			add(c, "equals", exp, actual, found && jsonEqual(exp, actual))
		}
		if c.Contains != nil {
			exp, err := ctx.Render(c.Contains)
			if err != nil {
				return results, failed, specErr(err)
			}
			add(c, "contains", exp, actual, found && contains(actual, exp))
		}
		if c.Matches != "" {
			re, err := regexp.Compile(c.Matches)
			if err != nil {
				return results, failed, specErr(fmt.Errorf("bad regex %q: %w", c.Matches, err))
			}
			add(c, "matches", c.Matches, actual, found && re.MatchString(fmt.Sprint(actual)))
		}
		if c.Gt != nil {
			f, isNum := toFloat(actual)
			add(c, "gt", *c.Gt, actual, found && isNum && f > *c.Gt)
		}
		if c.Lt != nil {
			f, isNum := toFloat(actual)
			add(c, "lt", *c.Lt, actual, found && isNum && f < *c.Lt)
		}
	}
	return results, failed, nil
}

// jsonEqual compares after a JSON round trip so int/float differences don't matter.
func jsonEqual(a, b any) bool {
	norm := func(v any) any {
		bs, _ := json.Marshal(v)
		var o any
		_ = json.Unmarshal(bs, &o)
		return o
	}
	return reflect.DeepEqual(norm(a), norm(b))
}

func contains(actual, want any) bool {
	switch a := actual.(type) {
	case string:
		return strings.Contains(a, fmt.Sprint(want))
	case []any:
		for _, x := range a {
			if jsonEqual(x, want) {
				return true
			}
		}
	}
	return false
}

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	}
	return 0, false
}

func snippet(b []byte) string {
	const max = 200
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
