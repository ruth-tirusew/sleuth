package runner

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

// EnvConfig is the environment file: per-environment values kept out of the flow spec.
type EnvConfig struct {
	Vars    map[string]string `yaml:"vars"`
	Secrets []string          `yaml:"secrets"` // names of OS env vars; values are never stored in files
}

var envVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func LoadEnv(path string) (*EnvConfig, error) {
	var c EnvConfig
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(b, &c); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	for k, v := range c.Vars {
		c.Vars[k] = envVarRe.ReplaceAllStringFunc(v, func(m string) string {
			return os.Getenv(envVarRe.FindStringSubmatch(m)[1])
		})
	}
	return &c, nil
}

// Context is the run context: env, secrets and the outputs of completed nodes.
type Context struct {
	data map[string]any
}

func NewContext(env *EnvConfig) (*Context, []string, error) {
	vars := map[string]any{}
	for k, v := range env.Vars {
		vars[k] = v
	}
	secrets := map[string]any{}
	var secretValues []string
	for _, name := range env.Secrets {
		v, ok := os.LookupEnv(name)
		if !ok {
			return nil, nil, fmt.Errorf("secret %q is not set in the process environment", name)
		}
		secrets[name] = v
		if v != "" {
			secretValues = append(secretValues, v)
		}
	}
	return &Context{data: map[string]any{
		"env": vars, "secrets": secrets, "nodes": map[string]any{},
	}}, secretValues, nil
}

func (c *Context) SetNode(id string, out map[string]any) {
	c.data["nodes"].(map[string]any)[id] = out
}

// Lookup resolves a dotted path (map keys and numeric list indices).
func (c *Context) Lookup(path string) (any, bool) { return lookup(c.data, path) }

func lookup(root any, path string) (any, bool) {
	path = strings.TrimPrefix(path, ".")
	cur := root
	if path == "" {
		return cur, true
	}
	for _, part := range strings.Split(path, ".") {
		switch v := cur.(type) {
		case map[string]any:
			x, ok := v[part]
			if !ok {
				return nil, false
			}
			cur = x
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(v) {
				return nil, false
			}
			cur = v[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

var pureRe = regexp.MustCompile(`^\{\{\s*\.([\w.]+)\s*\}\}$`)

// Render expands templates in v recursively. A string that is exactly one
// `{{ .path }}` keeps the native type of the referenced value.
func (c *Context) Render(v any) (any, error) {
	switch t := v.(type) {
	case string:
		if m := pureRe.FindStringSubmatch(t); m != nil {
			x, ok := c.Lookup(m[1])
			if !ok {
				return nil, fmt.Errorf("template %q: path not found", t)
			}
			return x, nil
		}
		if !strings.Contains(t, "{{") {
			return t, nil
		}
		tpl, err := template.New("").Option("missingkey=error").Parse(t)
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := tpl.Execute(&buf, c.data); err != nil {
			return nil, fmt.Errorf("template %q: %w", t, err)
		}
		return buf.String(), nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			r, err := c.Render(x)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			r, err := c.Render(x)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

func (c *Context) RenderString(s string) (string, error) {
	r, err := c.Render(s)
	if err != nil {
		return "", err
	}
	return fmt.Sprint(r), nil
}
