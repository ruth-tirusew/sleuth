package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"reflect"
	"strconv"
	"strings"

	"sleuth/internal/flow"
)

// Numbers coming back from an API, Postgres or MongoDB are kept as json.Number (their exact
// text) instead of float64, so a 64-bit id such as 9007199254740993 or a decimal such as
// 0.10 is neither rounded nor reformatted on its way into an assertion or a later request.
// float64 is used only where a magnitude is genuinely wanted (gt / lt).

// decodeJSON parses one JSON value, keeping numbers exact. Trailing data is an error.
func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errTrailingJSON
	}
	return v, nil
}

var errTrailingJSON = errors.New("unexpected data after the JSON value")

// numberFromString returns s as an exact json.Number when it is a number literal.
func numberFromString(s string) (any, bool) {
	if flow.IsNumberLiteral(s) {
		return json.Number(s), true
	}
	return s, false
}

// valuesEqual compares two values structurally, with numbers compared exactly:
// 9007199254740993 equals 9007199254740993 but not 9007199254740992, and 1.50 equals 1.5.
func valuesEqual(a, b any) bool { return deepEqual(normalize(a), normalize(b)) }

// normalize round-trips through JSON so Go/YAML types (int, float64, []string, ...) and
// json.Number all land on the same representation.
func normalize(v any) any {
	bs, err := json.Marshal(v)
	if err != nil {
		return v
	}
	out, err := decodeJSON(bs)
	if err != nil {
		return v
	}
	return out
}

func deepEqual(a, b any) bool {
	switch x := a.(type) {
	case json.Number:
		y, ok := b.(json.Number)
		return ok && numbersEqual(x, y)
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, e := range x {
			f, present := y[k]
			if !present || !deepEqual(e, f) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !deepEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}

// numbersEqual compares two JSON numbers exactly (as rationals). Numbers with a huge
// exponent are compared as floats rather than expanded.
func numbersEqual(a, b json.Number) bool {
	if a == b {
		return true
	}
	ra, okA := exactRat(a)
	rb, okB := exactRat(b)
	if okA && okB {
		return ra.Cmp(rb) == 0
	}
	// Not expanded as a rational: either the literal is very long, or its exponent is huge.
	// A very long literal is compared as text, since any rounding could call different numbers equal.
	if len(a) > maxExactLen || len(b) > maxExactLen {
		return strings.EqualFold(string(a), string(b))
	}
	// A short literal with a huge exponent: compare as arbitrary-exponent floats, whose
	// precision holds every digit of a literal within maxExactLen (a Float64 would overflow
	// for both, and then equal values would look different).
	const prec = 4200 // bits, above 1000 decimal digits
	// Only trust a float that holds the value exactly: an exponent beyond big.Float's range
	// overflows to Inf (and Inf == Inf), and a long mantissa can round. Otherwise compare text.
	if fa, okA := new(big.Float).SetPrec(prec).SetString(string(a)); okA && !fa.IsInf() && fa.Acc() == big.Exact {
		if fb, okB := new(big.Float).SetPrec(prec).SetString(string(b)); okB && !fb.IsInf() && fb.Acc() == big.Exact {
			return fa.Cmp(fb) == 0
		}
	}
	return strings.EqualFold(string(a), string(b))
}

// exactRat expands a JSON number to a rational so it can be compared exactly. It refuses
// numbers that would be expensive to expand (a hostile or buggy "1e9999", or a huge literal);
// the caller then falls back to a float comparison.
func exactRat(n json.Number) (*big.Rat, bool) {
	s := string(n)
	if len(s) > maxExactLen {
		return nil, false
	}
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		exp, err := strconv.Atoi(strings.TrimPrefix(s[i+1:], "+"))
		if err != nil || exp > maxExactExp || exp < -maxExactExp {
			return nil, false
		}
	}
	return new(big.Rat).SetString(s)
}

const (
	maxExactLen = 1000 // characters
	maxExactExp = 400  // beyond float64's range anyway
)
