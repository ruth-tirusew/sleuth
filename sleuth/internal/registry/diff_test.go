package registry

import (
	"strings"
	"testing"
)

const (
	baseUser = `{"type":"object","required":["id","name"],"properties":{
		"id":{"type":"integer"},"name":{"type":"string"},"role":{"enum":["admin","user"]},
		"tags":{"type":"array","items":{"type":"string"}}}}`
	baseNewUser = `{"type":"object","required":["name"],"properties":{
		"name":{"type":"string","maxLength":50},"age":{"type":"integer"},"kind":{"enum":["a","b","c"]}}}`
)

// spec builds a small document; the pieces are what each test varies.
func spec(user, newUser, extraPaths string, getResponses string) string {
	if getResponses == "" {
		getResponses = `"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/User"}}}},"404":{}`
	}
	return `{"openapi":"3.1.0","paths":{
	 "/users/{id}":{"get":{"parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"string"}}],"responses":{` + getResponses + `}}},
	 "/users":{"post":{"requestBody":{"required":true,"content":{"application/json":{"schema":{"$ref":"#/components/schemas/NewUser"}}}},"responses":{"201":{}}}},
	 "/legacy":{"get":{"responses":{"200":{}}}}` + extraPaths + `},
	 "components":{"schemas":{"User":` + user + `,"NewUser":` + newUser + `}}}`
}

func diff(t *testing.T, oldDoc, newDoc string) []Change {
	t.Helper()
	return Diff(loadDoc(t, oldDoc), loadDoc(t, newDoc))
}

// has reports whether a change of this severity mentions all the substrings (operation, where, path and message together).
func has(cs []Change, sev Severity, parts ...string) bool {
	for _, c := range cs {
		if c.Severity != sev {
			continue
		}
		line := c.Operation + " | " + c.Where + " | " + c.Path + " | " + c.Message
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(line, p)
		}
		if ok {
			return true
		}
	}
	return false
}

func dump(cs []Change) string {
	var b strings.Builder
	for _, c := range cs {
		b.WriteString("\n  " + string(c.Severity) + ": " + c.Operation + " [" + c.Where + "] " + c.Path + " " + c.Message)
	}
	return b.String()
}

func count(cs []Change, sev Severity) int {
	n := 0
	for _, c := range cs {
		if c.Severity == sev {
			n++
		}
	}
	return n
}

func TestDiffIdenticalDocumentsHaveNoChanges(t *testing.T) {
	d := spec(baseUser, baseNewUser, "", "")
	if cs := diff(t, d, d); len(cs) != 0 {
		t.Fatalf("%s", dump(cs))
	}
}

func TestDiffResponseLoosening(t *testing.T) {
	// what a client RECEIVES: removed fields, changed types and new enum values break it
	newUser := `{"type":"object","required":["id"],"properties":{
		"id":{"type":"string"},"role":{"enum":["admin","user","root"]},"email":{"type":"string"}}}`
	cs := diff(t, spec(baseUser, baseNewUser, "", ""), spec(newUser, baseNewUser, "", ""))
	for _, want := range [][]string{
		{"GET /users/{id}", "response", "200 $.name", "property removed"},
		{"200 $.tags", "property removed"},
		{"200 $.id", "type changed from integer to string"},
		{"200 $.role", "new enum value(s) may be returned: \"root\""},
	} {
		if !has(cs, Breaking, want...) {
			t.Errorf("missing breaking %v in:%s", want, dump(cs))
		}
	}
	if !has(cs, Info, "200 $.email", "property added") {
		t.Errorf("an added response property is compatible:%s", dump(cs))
	}
}

func TestDiffResponseFieldNoLongerGuaranteed(t *testing.T) {
	newUser := `{"type":"object","required":["id"],"properties":{
		"id":{"type":"integer"},"name":{"type":"string"},"role":{"enum":["admin","user"]},"tags":{"type":"array","items":{"type":"string"}}}}`
	cs := diff(t, spec(baseUser, baseNewUser, "", ""), spec(newUser, baseNewUser, "", ""))
	if !has(cs, Breaking, "200 $.name", "no longer guaranteed") || count(cs, Breaking) != 1 {
		t.Fatalf("%s", dump(cs))
	}
}

func TestDiffRequestTightening(t *testing.T) {
	// what a client SENDS: new required fields, tighter bounds and removed enum values break it
	newUser := `{"type":"object","required":["name","age","email"],"properties":{
		"name":{"type":"string","maxLength":20},"age":{"type":"integer"},"email":{"type":"string"},"kind":{"enum":["a","b","d"]}}}`
	cs := diff(t, spec(baseUser, baseNewUser, "", ""), spec(baseUser, newUser, "", ""))
	for _, want := range [][]string{
		{"POST /users", "request", "$.age", "now required"},
		{"$.email", "new required property"},
		{"$.name", "maxLength tightened to 20 (was 50)"},
		{"$.kind", "no longer accepted: \"c\""},
	} {
		if !has(cs, Breaking, want...) {
			t.Errorf("missing breaking %v in:%s", want, dump(cs))
		}
	}
	if !has(cs, Info, "$.kind", "enum value(s) added: \"d\"") {
		t.Errorf("an accepted enum value being added is compatible:%s", dump(cs))
	}
}

func TestDiffRequestLooseningIsCompatible(t *testing.T) {
	newUser := `{"type":"object","required":[],"properties":{
		"name":{"type":"string","maxLength":100},"age":{"type":"number"},"kind":{"enum":["a","b","c","d"]},"nick":{"type":"string"}}}`
	cs := diff(t, spec(baseUser, baseNewUser, "", ""), spec(baseUser, newUser, "", ""))
	if count(cs, Breaking) != 0 || count(cs, Review) != 0 {
		t.Fatalf("relaxing what a client may send must not break it:%s", dump(cs))
	}
	if !has(cs, Info, "$.name", "no longer required") || !has(cs, Info, "$.age", "type widened") {
		t.Fatalf("%s", dump(cs))
	}
}

func TestDiffOperationsAndStatuses(t *testing.T) {
	old := spec(baseUser, baseNewUser, "", "")
	// drop /legacy, add /fresh, replace 200 with 202 and drop the documented 404
	changed := strings.Replace(spec(baseUser, baseNewUser, `,"/fresh":{"get":{"responses":{"200":{}}}}`,
		`"202":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/User"}}}}`), `"/legacy":{"get":{"responses":{"200":{}}}},`, "", 1)
	cs := diff(t, old, changed)
	if !has(cs, Breaking, "GET /legacy", "operation removed") ||
		!has(cs, Breaking, "GET /users/{id}", "response", "200", "success response 200 removed") {
		t.Errorf("%s", dump(cs))
	}
	if !has(cs, Info, "GET /fresh", "operation added") || !has(cs, Info, "response 404 no longer documented") || !has(cs, Info, "response 202 added") {
		t.Errorf("%s", dump(cs))
	}
	if count(cs, Breaking) != 2 {
		t.Errorf("a removed 404 or an added 202 must not count as breaking:%s", dump(cs))
	}
}

func TestDiffParameters(t *testing.T) {
	op := func(params string) string {
		return `{"openapi":"3.1.0","paths":{"/items":{"get":{"parameters":[` + params + `],"responses":{"200":{}}}}}}`
	}
	old := op(`{"name":"q","in":"query","schema":{"type":"string"}},{"name":"old","in":"query","schema":{"type":"string"}}`)
	neu := op(`{"name":"q","in":"query","required":true,"schema":{"type":"integer"}},
		{"name":"limit","in":"query","required":true,"schema":{"type":"integer"}},{"name":"page","in":"query","schema":{"type":"integer"}}`)
	cs := diff(t, old, neu)
	for _, want := range [][]string{{"query:limit", "new required query parameter limit"}, {"query:q", "now required"}, {"query:q", "type changed"}} {
		if !has(cs, Breaking, want...) {
			t.Errorf("missing breaking %v in:%s", want, dump(cs))
		}
	}
	if !has(cs, Info, "query:page", "new optional") || !has(cs, Info, "query:old", "no longer documented") {
		t.Errorf("%s", dump(cs))
	}
}

func TestDiffRequestBodyBecomesRequired(t *testing.T) {
	body := func(required string) string {
		return `{"openapi":"3.1.0","paths":{"/x":{"post":{"requestBody":{` + required + `"content":{"application/json":{"schema":{"type":"object"}}}},"responses":{"200":{}}}}}}`
	}
	if cs := diff(t, body(""), body(`"required":true,`)); !has(cs, Breaking, "POST /x", "request body is now required") {
		t.Fatalf("%s", dump(cs))
	}
	if cs := diff(t, body(`"required":true,`), body("")); count(cs, Breaking) != 0 {
		t.Fatalf("%s", dump(cs))
	}
}

func TestDiffUnclassifiableChangesAreFlaggedForReview(t *testing.T) {
	mk := func(extra string) string {
		return `{"openapi":"3.1.0","paths":{"/x":{"get":{"responses":{"200":{"content":{"application/json":{"schema":{` + extra + `}}}}}}}}}`
	}
	cs := diff(t, mk(`"oneOf":[{"type":"string"},{"type":"integer"}]`), mk(`"oneOf":[{"type":"string"},{"type":"boolean"}]`))
	if !has(cs, Review, "oneOf changed") || count(cs, Breaking) != 0 {
		t.Fatalf("%s", dump(cs))
	}
	cs = diff(t, mk(`"type":"string","format":"date"`), mk(`"type":"string","format":"date-time"`))
	if !has(cs, Review, "format changed") {
		t.Fatalf("%s", dump(cs))
	}
}

func TestDiffRecursiveSchemasTerminate(t *testing.T) {
	doc := func(nameType string) string {
		return `{"openapi":"3.1.0","paths":{"/tree":{"get":{"responses":{"200":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Node"}}}}}}}},
		 "components":{"schemas":{"Node":{"type":"object","required":["name"],"properties":{"name":{"type":"` + nameType + `"},"child":{"$ref":"#/components/schemas/Node"}}}}}}`
	}
	cs := diff(t, doc("string"), doc("integer"))
	if !has(cs, Breaking, "$.name", "type changed from string to integer") {
		t.Fatalf("%s", dump(cs))
	}
	if count(cs, Breaking) > 3 { // the top-level and a bounded number of nested reports, never an unbounded flood
		t.Fatalf("recursion not bounded:%s", dump(cs))
	}
}

func TestDiffSortsBreakingFirstAndSecurityChangeNeedsReview(t *testing.T) {
	old := `{"openapi":"3.1.0","paths":{"/a":{"get":{"responses":{"200":{}}}},"/b":{"get":{"security":[{"k":[]}],"responses":{"200":{}}}}}}`
	neu := `{"openapi":"3.1.0","paths":{"/b":{"get":{"security":[{"other":[]}],"responses":{"200":{}}}},"/c":{"get":{"responses":{"200":{}}}}}}`
	cs := diff(t, old, neu)
	if len(cs) != 3 || cs[0].Severity != Breaking || cs[1].Severity != Review || cs[2].Severity != Info {
		t.Fatalf("%s", dump(cs))
	}
}
