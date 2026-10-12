package serve

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const tok = "test-token"

// fixture: a target service, a flows dir (one good flow, one broken, one env file), and a server.
func fixture(t *testing.T, target http.HandlerFunc) (*httptest.Server, *Server, string) {
	t.Helper()
	tgt := httptest.NewServer(target)
	t.Cleanup(tgt.Close)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ok.flow.yaml"), []byte(`
version: 1
name: ok
description: two independent calls then a join
nodes:
  - {id: a, kind: request, request: {url: "{{ .env.base }}/a", headers: {X-Key: "{{ .secrets.SLEUTH_UI_TEST_SECRET }}"}}}
  - {id: b, kind: request, request: {url: "{{ .env.base }}/b"}}
  - {id: join, kind: transform, needs: [a, b], transform: {set: {k: 1}}}
`), 0o644)
	os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte("version: 1\nname: broken\nnodes: []\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "local.env.yaml"), []byte("vars: {base: "+tgt.URL+"}\nsecrets: [SLEUTH_UI_TEST_SECRET]\n"), 0o644)
	t.Setenv("SLEUTH_UI_TEST_SECRET", "hunter2-very-secret")
	s := New(Config{Dir: dir, EnvPath: filepath.Join(dir, "local.env.yaml"), EvidenceDir: filepath.Join(dir, "evidence"), Token: tok})
	ui := httptest.NewServer(s.Handler())
	t.Cleanup(ui.Close)
	return ui, s, dir
}

func call(t *testing.T, method, url, body string, hdr map[string]string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

var auth = map[string]string{"X-Sleuth-Token": tok}

func TestAuthAndHostGuard(t *testing.T) {
	ui, _, _ := fixture(t, func(w http.ResponseWriter, _ *http.Request) {})
	if c, _ := call(t, "GET", ui.URL+"/", "", nil); c != 200 {
		t.Fatalf("page must load without a token: %d", c)
	}
	for _, h := range []map[string]string{nil, {"X-Sleuth-Token": "wrong"}} {
		if c, _ := call(t, "GET", ui.URL+"/api/flows", "", h); c != 401 {
			t.Fatalf("flows without a valid token: %d", c)
		}
		if c, _ := call(t, "POST", ui.URL+"/api/run", `{"flow":"ok.flow.yaml"}`, h); c != 401 {
			t.Fatalf("run without a valid token: %d", c)
		}
	}
	if c, _ := call(t, "GET", ui.URL+"/api/flows?token="+tok, "", nil); c != 200 {
		t.Fatalf("query token (EventSource): %d", c)
	}
	// DNS rebinding: a loopback socket reached through a foreign Host name
	req, _ := http.NewRequest("GET", ui.URL+"/api/flows", nil)
	req.Host = "evil.example.com"
	req.Header.Set("X-Sleuth-Token", tok)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 403 {
		t.Fatalf("foreign Host must be rejected: %d", resp.StatusCode)
	}
}

func TestFlowListingAndNoPathChosenByClient(t *testing.T) {
	ui, _, dir := fixture(t, func(w http.ResponseWriter, _ *http.Request) {})
	c, b := call(t, "GET", ui.URL+"/api/flows", "", auth)
	var flows []flowInfo
	json.Unmarshal(b, &flows)
	if c != 200 || len(flows) != 2 || flows[0].File != "broken.yaml" || flows[0].Error == "" || flows[1].File != "ok.flow.yaml" ||
		flows[1].Name != "ok" || len(flows[1].Nodes) != 3 || len(flows[1].Waves) != 2 || len(flows[1].Waves[0]) != 2 {
		t.Fatalf("%d %s", c, b)
	}
	outside := filepath.Join(filepath.Dir(dir), "outside.yaml")
	os.WriteFile(outside, []byte("version: 1\nname: x\nnodes: [{id: a, kind: transform, transform: {set: {a: 1}}}]\n"), 0o644)
	for _, name := range []string{"../outside.yaml", outside, "local.env.yaml", "nope.yaml", ""} {
		body, _ := json.Marshal(runRequest{Flow: name})
		if c, _ := call(t, "POST", ui.URL+"/api/run", string(body), auth); c != 404 {
			t.Fatalf("%q must not run: %d", name, c)
		}
	}
}

// readEvents follows a run's stream until it ends and returns the raw event payloads.
func readEvents(t *testing.T, url string) []string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data: ") {
			out = append(out, strings.TrimPrefix(line, "data: "))
		}
	}
	return out
}

func TestRunStreamsEventsMasksSecretsAndWritesEvidence(t *testing.T) {
	var peak, cur int32
	ui, _, dir := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		c := atomic.AddInt32(&cur, 1)
		for p := atomic.LoadInt32(&peak); c > p && !atomic.CompareAndSwapInt32(&peak, p, c); p = atomic.LoadInt32(&peak) {
		}
		time.Sleep(150 * time.Millisecond)
		atomic.AddInt32(&cur, -1)
		// the target echoes the secret back, as a leaky service might
		fmt.Fprintf(w, `{"echo":%q}`, r.Header.Get("X-Key"))
	})
	c, b := call(t, "POST", ui.URL+"/api/run", `{"flow":"ok.flow.yaml","parallel":2}`, auth)
	var started struct{ ID string }
	json.Unmarshal(b, &started)
	if c != 202 || started.ID == "" {
		t.Fatalf("%d %s", c, b)
	}
	events := readEvents(t, ui.URL+"/api/runs/"+started.ID+"/events?token="+tok)

	var types []string
	for _, e := range events {
		var m map[string]any
		json.Unmarshal([]byte(e), &m)
		types = append(types, m["type"].(string))
	}
	if types[0] != "run_start" || types[len(types)-1] != "run_done" || strings.Count(strings.Join(types, " "), "step_done") != 3 || strings.Count(strings.Join(types, " "), "step_start") != 3 {
		t.Fatalf("event sequence: %v", types)
	}
	if atomic.LoadInt32(&peak) != 2 {
		t.Fatalf("parallel 2 should overlap a and b: peak %d", peak)
	}
	all := strings.Join(events, "\n")
	if strings.Contains(all, "hunter2-very-secret") || !strings.Contains(all, "***") {
		t.Fatalf("secret leaked or not masked in the stream")
	}
	var done struct {
		Run      struct{ Status string }
		Evidence string
	}
	json.Unmarshal([]byte(events[len(events)-1]), &done)
	if done.Run.Status != "passed" || done.Evidence == "" {
		t.Fatalf("%s", events[len(events)-1])
	}
	ev, err := os.ReadFile(done.Evidence)
	if err != nil || strings.Contains(string(ev), "hunter2-very-secret") || !strings.HasPrefix(done.Evidence, filepath.Join(dir, "evidence")) {
		t.Fatalf("evidence file: %v", err)
	}

	// a second client can replay the finished run
	if again := readEvents(t, ui.URL+"/api/runs/"+started.ID+"/events?token="+tok); len(again) != len(events) {
		t.Fatalf("replay got %d events, want %d", len(again), len(events))
	}
}

func TestOnlyOneRunAtATimeAndSetupErrorsAreEvents(t *testing.T) {
	gate := make(chan struct{})
	ui, _, _ := fixture(t, func(w http.ResponseWriter, _ *http.Request) { <-gate; fmt.Fprint(w, `{}`) })
	_, b := call(t, "POST", ui.URL+"/api/run", `{"flow":"ok.flow.yaml"}`, auth)
	var first struct{ ID string }
	json.Unmarshal(b, &first)
	if c, _ := call(t, "POST", ui.URL+"/api/run", `{"flow":"ok.flow.yaml"}`, auth); c != 409 {
		t.Fatalf("second concurrent run: %d", c)
	}
	close(gate)
	readEvents(t, ui.URL+"/api/runs/"+first.ID+"/events?token="+tok) // wait for it to finish
	time.Sleep(50 * time.Millisecond)

	// a missing secret is a setup error: reported as an event, not a hang
	os.Unsetenv("SLEUTH_UI_TEST_SECRET")
	_, b = call(t, "POST", ui.URL+"/api/run", `{"flow":"ok.flow.yaml"}`, auth)
	var second struct{ ID string }
	json.Unmarshal(b, &second)
	ev := readEvents(t, ui.URL+"/api/runs/"+second.ID+"/events?token="+tok)
	if len(ev) == 0 || !strings.Contains(ev[len(ev)-1], `"run_error"`) || !strings.Contains(ev[len(ev)-1], "SLEUTH_UI_TEST_SECRET") {
		t.Fatalf("%v", ev)
	}
}

func TestCheckAddrOnlyLoopback(t *testing.T) {
	for addr, ok := range map[string]bool{"127.0.0.1:8787": true, "localhost:1": true, "[::1]:9": true, "0.0.0.0:8787": false, ":8787": false, "192.168.1.5:80": false, "example.com:80": false} {
		if err := CheckAddr(addr); (err == nil) != ok {
			t.Errorf("%s: err=%v want ok=%v", addr, err, ok)
		}
	}
}

func TestFlowsPairWithTheirEnvFile(t *testing.T) {
	var hitDefault, hitPaired int32
	ui, s, dir := fixture(t, func(w http.ResponseWriter, _ *http.Request) { atomic.AddInt32(&hitDefault, 1); fmt.Fprint(w, `{}`) })
	paired := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { atomic.AddInt32(&hitPaired, 1); fmt.Fprint(w, `{}`) }))
	defer paired.Close()
	flow := "version: 1\nname: %s\nnodes:\n  - {id: a, kind: request, request: {url: \"{{ .env.base }}/a\"}}\n"
	os.WriteFile(filepath.Join(dir, "mine.flow.yaml"), []byte(fmt.Sprintf(flow, "mine")), 0o644)
	os.WriteFile(filepath.Join(dir, "mine.env.yaml"), []byte("vars: {base: "+paired.URL+"}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "plain.yml"), []byte(fmt.Sprintf(flow, "plain")), 0o644) // no paired file
	os.WriteFile(filepath.Join(dir, "needs.flow.yaml"), []byte(fmt.Sprintf(flow, "needs")), 0o644)
	os.WriteFile(filepath.Join(dir, "needs.env.yaml"), []byte("vars: {base: \"${SLEUTH_UI_UNSET_VAR}\"}\nsecrets: [SLEUTH_UI_UNSET_SECRET]\n"), 0o644)

	if p, ok := s.envFor("mine.flow.yaml"); !ok || filepath.Base(p) != "mine.env.yaml" {
		t.Fatalf("%q %v", p, ok)
	}
	if p, ok := s.envFor("plain.yml"); ok || filepath.Base(p) != "local.env.yaml" {
		t.Fatalf("a flow without a paired file uses the default: %q %v", p, ok)
	}

	_, b := call(t, "GET", ui.URL+"/api/flows", "", auth)
	var flows []flowInfo
	json.Unmarshal(b, &flows)
	by := map[string]flowInfo{}
	for _, f := range flows {
		by[f.File] = f
	}
	if f := by["mine.flow.yaml"]; f.Env != "mine.env.yaml" || !f.EnvPaired || f.EnvError != "" {
		t.Fatalf("%+v", f)
	}
	if f := by["plain.yml"]; f.Env != "local.env.yaml" || f.EnvPaired || f.EnvError != "" {
		t.Fatalf("%+v", f)
	}
	if f := by["needs.flow.yaml"]; !f.EnvPaired || !strings.Contains(f.EnvError, "SLEUTH_UI_UNSET_VAR") {
		t.Fatalf("an unset variable must be reported up front: %+v", f)
	}
	for _, name := range []string{"mine.env.yaml", "needs.env.yaml", "local.env.yaml"} {
		if _, listed := by[name]; listed {
			t.Fatalf("%s is an env file, not a flow", name)
		}
	}

	// a run uses the paired env: it hits the paired target, not the default one
	_, b = call(t, "POST", ui.URL+"/api/run", `{"flow":"mine.flow.yaml"}`, auth)
	var started struct{ ID string }
	json.Unmarshal(b, &started)
	ev := readEvents(t, ui.URL+"/api/runs/"+started.ID+"/events?token="+tok)
	if atomic.LoadInt32(&hitPaired) != 1 || atomic.LoadInt32(&hitDefault) != 0 || !strings.Contains(ev[0], `"env":"mine.env.yaml"`) {
		t.Fatalf("paired=%d default=%d first=%s", hitPaired, hitDefault, ev[0])
	}
	time.Sleep(50 * time.Millisecond)

	// and the default env still serves the flows that have no pair
	_, b = call(t, "POST", ui.URL+"/api/run", `{"flow":"plain.yml"}`, auth)
	json.Unmarshal(b, &started)
	readEvents(t, ui.URL+"/api/runs/"+started.ID+"/events?token="+tok)
	if atomic.LoadInt32(&hitDefault) != 1 {
		t.Fatalf("default env not used: default=%d", hitDefault)
	}
}

func TestNoDefaultEnvIsFineWhenFlowsArePaired(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x.flow.yaml"), []byte("version: 1\nname: x\nnodes:\n  - {id: a, kind: transform, transform: {set: {a: 1}}}\n"), 0o644)
	ui := httptest.NewServer(New(Config{Dir: dir, Token: tok}).Handler())
	defer ui.Close()
	_, b := call(t, "GET", ui.URL+"/api/flows", "", auth)
	var flows []flowInfo
	json.Unmarshal(b, &flows)
	if len(flows) != 1 || flows[0].Env != "none" || flows[0].EnvError != "" {
		t.Fatalf("%s", b)
	}
	_, b = call(t, "POST", ui.URL+"/api/run", `{"flow":"x.flow.yaml"}`, auth)
	var st struct{ ID string }
	json.Unmarshal(b, &st)
	ev := readEvents(t, ui.URL+"/api/runs/"+st.ID+"/events?token="+tok)
	if !strings.Contains(ev[len(ev)-1], `"status":"passed"`) {
		t.Fatalf("%v", ev)
	}
}
