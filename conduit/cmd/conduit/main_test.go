package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mock() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"token": "tok-123", "user_id": 42})
	})
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-123" {
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"username": "alice", "balance": 100.5})
	})
	return httptest.NewServer(mux)
}

func setup(t *testing.T, balance string) (flow, env, out string) {
	srv := mock()
	t.Cleanup(srv.Close)
	t.Setenv("APP_PASSWORD", "s3cret")
	dir := t.TempDir()
	flow = filepath.Join(dir, "f.yaml")
	src, _ := os.ReadFile("../../examples/login-and-profile.flow.yaml")
	os.WriteFile(flow, []byte(strings.Replace(string(src), "gt: 0", "gt: "+balance, 1)), 0o644)
	env = filepath.Join(dir, "e.yaml")
	os.WriteFile(env, []byte("vars:\n  base_url: "+srv.URL+"\n  username: alice\nsecrets: [APP_PASSWORD]\n"), 0o644)
	return flow, env, filepath.Join(dir, "out.json")
}

func TestPass(t *testing.T) {
	f, e, out := setup(t, "0")
	if code := run([]string{"run", "-env", e, "-out", out, f}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	b, _ := os.ReadFile(out)
	if strings.Contains(string(b), "s3cret") {
		t.Fatal("secret leaked into evidence")
	}
}

func TestAssertionFailureExit1AndSkips(t *testing.T) {
	f, e, out := setup(t, "1000")
	if code := run([]string{"run", "-env", e, "-out", out, f}); code != 1 {
		t.Fatalf("want exit 1, got %d", code)
	}
}

func TestMissingSecretExit2(t *testing.T) {
	f, e, out := setup(t, "0")
	os.Unsetenv("APP_PASSWORD")
	if code := run([]string{"run", "-env", e, "-out", out, f}); code != 2 {
		t.Fatalf("want exit 2, got %d", code)
	}
}

func TestCycleExit2(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte("version: 1\nname: c\nnodes:\n- {id: a, kind: transform, needs: [b], transform: {set: {x: 1}}}\n- {id: b, kind: transform, needs: [a], transform: {set: {x: 1}}}\n"), 0o644)
	if code := run([]string{"validate", p}); code != 2 {
		t.Fatalf("want exit 2, got %d", code)
	}
}
