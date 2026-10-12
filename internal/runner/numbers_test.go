package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"sleuth/internal/report"
)

func TestValuesEqualIsExactForLargeNumbers(t *testing.T) {
	n := func(s string) json.Number { return json.Number(s) }
	for _, c := range []struct {
		name string
		a, b any
		want bool
	}{
		{"same big int", n("9007199254740993"), n("9007199254740993"), true},
		{"adjacent big ints differ", n("9007199254740993"), n("9007199254740992"), false},
		{"big int vs int64 from YAML", n("9007199254740993"), int64(9007199254740993), true},
		{"float64 can't express it, so it must differ", n("9007199254740993"), float64(9007199254740993), false},
		{"trailing zero", n("1.50"), n("1.5"), true},
		{"exponent form", n("1e2"), n("100"), true},
		{"float literal", n("0.1"), 0.1, true},
		{"int vs float", 7, 7.0, true},
		{"different", n("1.5"), n("1.6"), false},
		{"huge exponent is not expanded", n("1e999999"), n("1e999999"), true},
		{"nested", map[string]any{"a": []any{n("12345678901234567890")}}, map[string]any{"a": []any{n("12345678901234567890")}}, true},
		{"nested differ", map[string]any{"a": []any{n("12345678901234567890")}}, map[string]any{"a": []any{n("12345678901234567891")}}, false},
		{"string vs number", "7", n("7"), false},
		{"typed slice", []string{"a"}, []any{"a"}, true},
	} {
		if got := valuesEqual(c.a, c.b); got != c.want {
			t.Errorf("%s: valuesEqual(%v, %v) = %v, want %v", c.name, c.a, c.b, got, c.want)
		}
	}
}

func TestPlainExtJSONKeepsLongsAndDecimalsExact(t *testing.T) {
	in, err := decodeJSON([]byte(`{"id":{"$numberLong":"9007199254740993"},"price":{"$numberDecimal":"10.50"},"bad":{"$numberDecimal":"NaN"},"oid":{"$oid":"abc"},"n":3}`))
	if err != nil {
		t.Fatal(err)
	}
	m := plainExtJSON(in).(map[string]any)
	if m["id"] != json.Number("9007199254740993") || m["price"] != json.Number("10.50") || m["bad"] != "NaN" || m["oid"] != "abc" || m["n"] != json.Number("3") {
		t.Fatalf("%#v", m)
	}
}

func TestDecodeJSONRejectsTrailingData(t *testing.T) {
	if _, err := decodeJSON([]byte(`{"a":1} extra`)); err == nil {
		t.Fatal("trailing data must be an error, as with json.Unmarshal")
	}
}

func TestToFloatReadsJSONNumbers(t *testing.T) {
	if f, ok := toFloat(json.Number("12.5")); !ok || f != 12.5 {
		t.Fatal("json.Number must work with gt / lt")
	}
}

// An API id beyond 2^53 must survive: asserted exactly, and carried into the next request.
func TestHTTPLargeIDsAreExactAndCarriedThrough(t *testing.T) {
	var echoed string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			b := make([]byte, 200)
			n, _ := r.Body.Read(b)
			echoed = string(b[:n])
			fmt.Fprint(w, `{"ok":true}`)
			return
		}
		fmt.Fprint(w, `{"id": 9007199254740993, "balance": 10.50}`)
	}))
	defer srv.Close()
	r := runYAML(t, srv.URL, `
version: 1
name: t
nodes:
  - id: get
    kind: request
    request: {url: "{{ .env.base }}/", extract: {id: body.id}}
  - id: exact
    kind: assertion
    assert:
      - {path: nodes.get.response.body.id, equals: 9007199254740993}
      - {path: nodes.get.response.body.balance, equals: 10.5}
      - {path: nodes.get.response.body.balance, gt: 10}
  - id: off_by_one
    kind: assertion
    assert: [{path: nodes.get.response.body.id, equals: 9007199254740992}]
  - id: send
    kind: request
    request: {method: POST, url: "{{ .env.base }}/", body: {id: "{{ .nodes.get.out.id }}"}}
`)
	if s := step(r, "exact"); s.Status != report.Passed {
		t.Fatalf("exact: %+v", s.Checks)
	}
	if step(r, "off_by_one").Status != report.Failed {
		t.Fatal("9007199254740992 must not equal 9007199254740993")
	}
	if echoed != `{"id":9007199254740993}` {
		t.Fatalf("the id was altered on the way out: %q", echoed)
	}
}

func TestMongoErrClassification(t *testing.T) {
	err := mongoErr(fmt.Errorf("wrapped: %w", mongo.CommandError{Code: 2, Name: "BadValue", Message: "unknown operator: $bogus"}))
	if kindOf(err) != KindSpec || err.Error() != "mongo error 2 (BadValue): unknown operator: $bogus" {
		t.Fatalf("%v (%s)", err, kindOf(err))
	}
	if k := kindOf(mongoErr(fmt.Errorf("connection refused"))); k != KindInfra {
		t.Fatalf("a failure to reach the server is infrastructure, got %s", k)
	}
	if mongoTransport(mongo.CommandError{Code: 13}) || mongoTransport(context.DeadlineExceeded) ||
		mongoTransport(fmt.Errorf("slow: %w", context.Canceled)) || !mongoTransport(fmt.Errorf("no reachable servers")) {
		t.Fatal("only errors that are not the server's answer count as transport failures")
	}
}

func TestBrokenMongoClientIsDroppedSoTheNextUseReconnects(t *testing.T) {
	c := newDBConns(1)
	defer c.close()
	cl, reused, err := c.mongoClient("m", "mongodb://127.0.0.1:1/?serverSelectionTimeoutMS=100")
	if err != nil || reused || cl == nil {
		t.Fatalf("%v %v", err, reused)
	}
	if again, reused, _ := c.mongoClient("m", "mongodb://127.0.0.1:1/"); !reused || again != cl {
		t.Fatal("the client should be cached")
	}
	c.dropMongo("m", cl)
	if len(c.retired) != 1 {
		t.Fatal("the failed client is retired, not disconnected under other nodes")
	}
	if _, reused, _ := c.mongoClient("m", "mongodb://127.0.0.1:1/?serverSelectionTimeoutMS=100"); reused {
		t.Fatal("after dropMongo the next use must create a new client")
	}
}

func TestNumbersEqualBoundsWhatItExpands(t *testing.T) {
	// a hostile or buggy value must not be expanded into a huge integer
	if numbersEqual("1e9999", "2e9999") {
		t.Error("different huge numbers must not compare equal")
	}
	if !numbersEqual("1e00005", "100000") { // leading zeros in the exponent are harmless
		t.Error("1e00005 equals 100000")
	}
	if !numbersEqual("0.0001e+4", "1") {
		t.Error("0.0001e+4 equals 1")
	}
	long := json.Number("1" + strings.Repeat("0", 5000))
	if !numbersEqual(long, long) {
		t.Error("identical long numbers are equal")
	}
	if _, ok := exactRat(json.Number("1e9999")); ok {
		t.Error("an exponent that large must not be expanded")
	}
}

// With parallel nodes, a node whose client failed must not discard (or disconnect) the
// healthy client another node has already put in its place.
func TestDropMongoOnlyRemovesTheClientThatFailed(t *testing.T) {
	c := newDBConns(1)
	defer c.close()
	old, _, _ := c.mongoClient("m", "mongodb://127.0.0.1:1/?serverSelectionTimeoutMS=100")
	// another node already replaced the failed client with a fresh one
	c.mu.Lock()
	fresh, err := mongo.Connect(options.Client().ApplyURI("mongodb://127.0.0.1:1/?serverSelectionTimeoutMS=100"))
	if err != nil {
		t.Fatal(err)
	}
	c.mongo["m"] = fresh
	c.mu.Unlock()

	c.dropMongo("m", old) // the late failure of the old client
	if got, reused, _ := c.mongoClient("m", "mongodb://127.0.0.1:1/"); !reused || got != fresh {
		t.Fatal("dropping a stale client must leave the newer cached client in place")
	}
}

func TestNumbersEqualBeyondTheExpansionBound(t *testing.T) {
	if !numbersEqual("1e401", "10e400") || !numbersEqual("1E999", "1e999") || !numbersEqual("1.0e999", "1e999") {
		t.Error("equal huge numbers written differently must be equal")
	}
	if numbersEqual("1e401", "1e402") || numbersEqual("1.000000001e999", "1e999") {
		t.Error("different huge numbers must differ")
	}
	a := json.Number("1" + strings.Repeat("0", 1200))
	b := json.Number("1" + strings.Repeat("0", 1199) + "1")
	if numbersEqual(a, b) {
		t.Error("two long literals that differ only in the last digit must not be equal")
	}
}

// An exponent beyond big.Float's range overflows to Inf, and Inf == Inf: different numbers
// must not compare equal because of it.
func TestNumbersEqualDoesNotTrustAnOverflowedOrRoundedFloat(t *testing.T) {
	if numbersEqual("1e999999999", "2e999999999") {
		t.Error("different numbers with an overflowing exponent must differ")
	}
	if !numbersEqual("1e999999999", "1E999999999") {
		t.Error("the same number written with a different case is equal")
	}
	// a ~990-digit mantissa with a large exponent cannot be held exactly: no silent rounding
	a := json.Number(strings.Repeat("9", 990) + "e300")
	b := json.Number(strings.Repeat("9", 989) + "8e300")
	if numbersEqual(a, b) {
		t.Error("near-equal long values must not be equal")
	}
}
