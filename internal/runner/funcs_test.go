package runner

import (
	"strings"
	"testing"

	"sleuth/internal/flow"
	"sleuth/internal/report"
)

func TestFuncNamesMatchWhatFlowValidates(t *testing.T) {
	m := funcMap(nil)
	for _, n := range flow.TemplateFuncNames {
		if _, ok := m[n]; !ok {
			t.Errorf("flow accepts %q but the runner does not define it", n)
		}
	}
	if len(m) != len(flow.TemplateFuncNames) {
		t.Errorf("the runner defines %d functions, flow lists %d", len(m), len(flow.TemplateFuncNames))
	}
}

func ctxWithSecret(secret string) *Context {
	c := &Context{sec: NewSecretSet([]string{secret}), data: map[string]any{
		"env": map[string]any{"name": "Bob", "ids": []any{"a", "b", "c"}, "empty": ""}, "secrets": map[string]any{"K": secret},
		"nodes": map[string]any{}, "params": map[string]any{},
	}}
	return c
}

func TestFunctionsComputeValues(t *testing.T) {
	c := ctxWithSecret("s3cretvalue")
	for tpl, want := range map[string]string{
		`{{ .env.name | lower }}`:                         `bob`,
		`{{ .env.name | upper }}`:                         `BOB`,
		`{{ join "," .env.ids }}`:                         `a,b,c`,
		`{{ .env.ids | first }}`:                          `a`,
		`{{ prefix "id-" .env.name }}`:                    `id-Bob`,
		`{{ .env.name | b64enc }}`:                        `Qm9i`,
		`{{ default "x" .env.empty }}`:                    `x`,
		`{{ default "x" .env.name }}`:                     `Bob`,
		`{{ .env.name | urlquery }}`:                      `Bob`,
		`{{ printf "%s-%d" .env.name 3 }}`:                `Bob-3`,
		`{{ if eq (lower .env.name) "bob" }}yes{{ end }}`: `yes`,
	} {
		got, err := c.RenderString(tpl)
		if err != nil || got != want {
			t.Errorf("%s = %q, %v; want %q", tpl, got, err, want)
		}
	}
	if _, err := c.RenderString(`{{ first .env.empty }}`); err == nil {
		t.Error("first of a non-list must be an error")
	}
}

// Every function that can build a new string from a secret must register the result for masking.
func TestDerivedSecretsAreMasked(t *testing.T) {
	const secret = "Pa55/word+1"
	for name, tpl := range map[string]string{
		"lower":    `{{ .secrets.K | lower }}`,
		"upper":    `{{ .secrets.K | upper }}`,
		"b64enc":   `{{ .secrets.K | b64enc }}`,
		"prefix":   `{{ prefix "Bearer " .secrets.K }}`,
		"join":     `{{ join ":" .nodes.l.list }}`,
		"urlquery": `{{ .secrets.K | urlquery }}`,
		"html":     `{{ .secrets.K | html }}`,
		"js":       `{{ .secrets.K | js }}`,
		"printf":   `{{ printf "user:%s" .secrets.K }}`,
		"print":    `{{ print "x" .secrets.K }}`,
		"chained":  `{{ .secrets.K | lower | b64enc }}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := ctxWithSecret(secret)
			c.data["nodes"].(map[string]any)["l"] = map[string]any{"list": []any{"u", secret}}
			got, err := c.RenderString(tpl)
			if err != nil {
				t.Fatal(err)
			}
			masked := report.MaskText("before "+got+" after", c.sec.Values())
			if strings.Contains(masked, got) && got != "" && !strings.Contains(got, "***") {
				// the whole derived value must be gone (not merely a prefix of it)
				t.Fatalf("derived value %q was not masked: %q", got, masked)
			}
			for _, leak := range []string{strings.ToLower(secret), strings.ToUpper(secret)} {
				if leak != secret && strings.Contains(masked, leak) {
					t.Fatalf("%q still visible in %q", leak, masked)
				}
			}
		})
	}
}

func TestNonSecretResultsAreNotMasked(t *testing.T) {
	c := ctxWithSecret("s3cretvalue")
	before := len(c.sec.Values())
	c.RenderString(`{{ .env.name | upper }}-{{ join "," .env.ids }}`)
	if len(c.sec.Values()) != before {
		t.Fatalf("a value derived from non-secrets must not be masked: %v", c.sec.Values())
	}
}

func TestDerivedSecretsReachTheEvidenceAndReturnedList(t *testing.T) {
	srv, _ := cleanupServer(false)
	defer srv.Close()
	t.Setenv("APIKEY", "Zk-9secret!")
	f := mustLoadInline(t, `version: 1
name: t
nodes:
  - id: call
    kind: request
    request:
      url: "{{ .env.base }}/things"
      headers: {X-Key: "{{ .secrets.APIKEY | upper }}", Authorization: "Basic {{ .secrets.APIKEY | b64enc }}"}
`)
	var live *SecretSet = NewSecretSet(nil)
	r, secrets, err := Run(f, &EnvConfig{Vars: map[string]string{"base": srv.URL}, Secrets: []string{"APIKEY"}}, Options{Secrets: live})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(secrets, "|")
	if !strings.Contains(joined, "ZK-9SECRET!") || len(live.Values()) != len(secrets) {
		t.Fatalf("derived values missing from the returned list: %v", secrets)
	}
	var b strings.Builder
	report.Print(&b, r)
	out := string(report.MaskJSON([]byte(`{"x":"`+r.Steps[0].Request.Headers["X-Key"]+`"}`), secrets))
	if strings.Contains(out, "ZK-9SECRET!") {
		t.Fatalf("header value not masked: %s", out)
	}
}
