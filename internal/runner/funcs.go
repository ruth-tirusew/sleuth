package runner

import (
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"text/template"

	"sleuth/internal/flow"
)

// SecretSet holds every value that must be masked: the secrets the env file names, plus any
// value a template function computed from one of them. Masking only knows the strings it is
// given, so a transformed secret (lower-cased, base64, joined into a longer string) would
// otherwise reach the evidence unmasked. It is safe for concurrent use.
type SecretSet struct {
	mu   sync.Mutex
	seen map[string]bool
	vals []string
}

func NewSecretSet(initial []string) *SecretSet {
	s := &SecretSet{seen: map[string]bool{}}
	s.Add(initial...)
	return s
}

// Add registers values to mask; empty values are ignored.
func (s *SecretSet) Add(vals ...string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range vals {
		if v != "" && !s.seen[v] {
			s.seen[v] = true
			s.vals = append(s.vals, v)
		}
	}
}

// Values returns a copy of everything to mask, secrets first.
func (s *SecretSet) Values() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.vals...)
}

// touches reports whether any argument (a string, or a list of them) contains a masked value.
func (s *SecretSet) touches(args ...any) bool {
	vals := s.Values()
	if len(vals) == 0 {
		return false
	}
	var texts []string
	var collect func(a any)
	collect = func(a any) {
		switch x := a.(type) {
		case []any:
			for _, e := range x {
				collect(e)
			}
		case []string:
			texts = append(texts, x...)
		case nil:
		default:
			texts = append(texts, fmt.Sprint(x))
		}
	}
	for _, a := range args {
		collect(a)
	}
	for _, t := range texts {
		for _, v := range vals {
			if strings.Contains(t, v) {
				return true
			}
		}
	}
	return false
}

// derive returns out, first registering it for masking when any input held a secret.
func (s *SecretSet) derive(out string, args ...any) string {
	if s.touches(args...) {
		s.Add(out)
	}
	return out
}

func asList(v any) ([]any, error) {
	switch x := v.(type) {
	case []any:
		return x, nil
	case []string:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = e
		}
		return out, nil
	}
	return nil, fmt.Errorf("want a list, got %T", v)
}

// funcMap is the closed set of template functions (names in flow.TemplateFuncNames). Anything
// that builds a new string from its arguments registers it with sec when an argument held a
// secret. The built-in escapers are replaced by the same wrappers for the same reason.
func funcMap(sec *SecretSet) template.FuncMap {
	return template.FuncMap{
		"lower": func(s string) string { return sec.derive(strings.ToLower(s), s) },
		"upper": func(s string) string { return sec.derive(strings.ToUpper(s), s) },
		"b64enc": func(s string) string {
			return sec.derive(base64.StdEncoding.EncodeToString([]byte(s)), s)
		},
		"prefix": func(p, s string) string { return sec.derive(p+s, p, s) },
		"join": func(sep string, list any) (string, error) {
			l, err := asList(list)
			if err != nil {
				return "", fmt.Errorf("join: %w", err)
			}
			parts := make([]string, len(l))
			for i, e := range l {
				parts[i] = fmt.Sprint(e)
			}
			return sec.derive(strings.Join(parts, sep), sep, l), nil
		},
		"first": func(list any) (any, error) {
			l, err := asList(list)
			if err != nil {
				return nil, fmt.Errorf("first: %w", err)
			}
			if len(l) == 0 {
				return nil, fmt.Errorf("first: the list is empty")
			}
			return l[0], nil
		},
		"default": func(def, v any) any {
			switch x := v.(type) {
			case nil:
				return def
			case string:
				if x == "" {
					return def
				}
			case bool:
				if !x {
					return def
				}
			case []any:
				if len(x) == 0 {
					return def
				}
			}
			return v
		},
		"urlquery": func(args ...any) string { return sec.derive(template.URLQueryEscaper(args...), args...) },
		"html":     func(args ...any) string { return sec.derive(template.HTMLEscaper(args...), args...) },
		"js":       func(args ...any) string { return sec.derive(template.JSEscaper(args...), args...) },
		"print":    func(args ...any) string { return sec.derive(fmt.Sprint(args...), args...) },
		"println":  func(args ...any) string { return sec.derive(fmt.Sprintln(args...), args...) },
		"printf": func(format string, args ...any) string {
			return sec.derive(fmt.Sprintf(format, args...), append([]any{format}, args...)...)
		},
	}
}

var _ = flow.TemplateFuncNames // the names flow validates against; funcs_test checks they match
