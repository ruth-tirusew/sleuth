package runner

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
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
	// Contracts maps a source name to an OpenAPI document (file path or http(s) URL).
	// Entries are raw; use ContractLocation, which expands and resolves only what is used.
	Contracts map[string]string `yaml:"contracts"`
	// Databases maps a connection name to a Postgres DSN, usually "${SOME_ENV_VAR}" so
	// credentials stay out of files. Expanded only when a db node uses it.
	Databases map[string]string `yaml:"databases"`

	path string // the env file, for resolving relative contract paths
}

var envVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expand replaces ${VAR} from the process environment. Any unset variable is an
// error, so "${HOST}/openapi.json" can't silently become "/openapi.json".
func expand(s string) (string, error) {
	var missing []string
	out := envVarRe.ReplaceAllStringFunc(s, func(m string) string {
		name := envVarRe.FindStringSubmatch(m)[1]
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("environment variable(s) not set: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

func LoadEnv(path string) (*EnvConfig, error) {
	c := EnvConfig{path: path}
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
		ev, err := expand(v)
		if err != nil {
			return nil, fmt.Errorf("%s: vars.%s: %w", path, k, err)
		}
		c.Vars[k] = ev
	}
	return &c, nil
}

// DatabaseDSN expands and returns the DSN for a named connection.
func (c *EnvConfig) DatabaseDSN(name string) (string, error) {
	raw, ok := c.Databases[name]
	if !ok {
		return "", fmt.Errorf("database connection %q is not defined under databases: in the env file", name)
	}
	v, err := expand(raw)
	if err != nil {
		return "", fmt.Errorf("databases.%s: %w", name, err)
	}
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("databases.%s is empty", name)
	}
	return v, nil
}

// ContractLocation expands and resolves the OpenAPI location for a contract source.
// Files are relative to the env file.
func (c *EnvConfig) ContractLocation(source string) (string, error) {
	raw, ok := c.Contracts[source]
	if !ok {
		return "", fmt.Errorf("contract source %q is not defined under contracts: in the env file", source)
	}
	v, err := expand(raw)
	if err != nil {
		return "", fmt.Errorf("contracts.%s: %w", source, err)
	}
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("contracts.%s is empty", source)
	}
	if !strings.Contains(v, "://") && !filepath.IsAbs(v) {
		v = filepath.Join(filepath.Dir(c.path), v)
	}
	return v, nil
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
