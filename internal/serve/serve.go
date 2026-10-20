// Package serve is a local web dashboard for running flows and watching them run.
//
// It executes flows with the secrets in this process's environment, so it is locked
// down: it only binds to loopback, every API call needs the token printed at startup,
// the Host header must be a loopback name, the client never names a file path (only a
// flow listed from the served directory), and everything sent to the browser has
// secret values masked.
package serve

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"sleuth/internal/flow"
	"sleuth/internal/report"
	"sleuth/internal/runner"
)

//go:embed web/index.html
var indexHTML []byte

type Config struct {
	Dir         string // flows are the .yaml/.yml files directly inside it
	EnvPath     string // default env file, for flows with no paired <name>.env.yaml (clients cannot choose one)
	EvidenceDir string // each run's masked evidence JSON is written here, like the CLI
	Token       string
}

type Server struct {
	cfg Config

	mu     sync.Mutex
	runs   map[string]*run
	order  []string // run ids, oldest first
	active *run
	nextID int
}

func New(cfg Config) *Server { return &Server{cfg: cfg, runs: map[string]*run{}} }

// CheckAddr refuses anything but a loopback listen address.
func CheckAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("refusing to listen on %q: the dashboard runs flows with your secrets, so it only binds to loopback (127.0.0.1, ::1, localhost)", addr)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/flows", s.auth(s.handleFlows))
	mux.HandleFunc("POST /api/run", s.auth(s.handleRun))
	mux.HandleFunc("GET /api/runs/{id}/events", s.auth(s.handleEvents))
	return s.hostGuard(mux)
}

// hostGuard rejects requests whose Host is not a loopback name (DNS rebinding).
func (s *Server) hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, "bad host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("X-Sleuth-Token")
		if got == "" {
			got = r.URL.Query().Get("token") // EventSource cannot set headers
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) != 1 {
			http.Error(w, "missing or wrong token", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

type flowInfo struct {
	File string `json:"file"`
	// Env is the env file this flow runs with: its paired <name>.env.yaml if one sits next to
	// it, else the server's default. EnvError says why a run cannot start (unset variable or
	// secret, unreadable file); the process environment is fixed at startup, so it is final.
	Env         string             `json:"env,omitempty"`
	EnvPaired   bool               `json:"env_paired,omitempty"`
	EnvError    string             `json:"env_error,omitempty"`
	Name        string             `json:"name,omitempty"`
	Description string             `json:"description,omitempty"`
	Nodes       []runner.EventNode `json:"nodes,omitempty"`
	Waves       [][]string         `json:"waves,omitempty"`
	Error       string             `json:"error,omitempty"`
}

// flowFiles lists the candidate flow files by base name. Env files are not flows.
func (s *Server) flowFiles() ([]string, error) {
	ents, err := os.ReadDir(s.cfg.Dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		ext := filepath.Ext(n)
		if e.IsDir() || (ext != ".yaml" && ext != ".yml") || strings.Contains(n, ".env.") || strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_") { // _name.yaml is a subflow, run through the flows that call it
			continue
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// envFor picks the env file for a flow: foo.flow.yaml (or foo.yaml) pairs with foo.env.yaml in the
// same directory; otherwise the default. paired says which. The result is empty if there is neither.
func (s *Server) envFor(file string) (path string, paired bool) {
	stem := strings.TrimSuffix(strings.TrimSuffix(file, filepath.Ext(file)), ".flow")
	for _, ext := range []string{".env.yaml", ".env.yml"} {
		p := filepath.Join(s.cfg.Dir, stem+ext)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, true
		}
	}
	return s.cfg.EnvPath, false
}

// checkEnv loads an env file the way a run would and names anything that would stop it.
func checkEnv(path string) string {
	env, err := runner.LoadEnv(path)
	if err != nil {
		return err.Error()
	}
	var missing []string
	for _, name := range env.Secrets {
		if _, ok := os.LookupEnv(name); !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return "secret(s) not set in the environment of `sleuth serve`: " + strings.Join(missing, ", ")
	}
	return ""
}

func (s *Server) handleFlows(w http.ResponseWriter, r *http.Request) {
	files, err := s.flowFiles()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	out := []flowInfo{}
	for _, f := range files {
		fi := flowInfo{File: f}
		fl, err := flow.Load(filepath.Join(s.cfg.Dir, f))
		if err != nil {
			fi.Error = err.Error()
			out = append(out, fi)
			continue
		}
		fi.Name, fi.Description = fl.Name, fl.Description
		envPath, paired := s.envFor(f)
		fi.EnvPaired = paired
		if envPath == "" {
			fi.Env = "none"
		} else {
			fi.Env = filepath.Base(envPath)
			fi.EnvError = checkEnv(envPath)
		}
		order, _ := fl.Order()
		for _, n := range order {
			fi.Nodes = append(fi.Nodes, runner.EventNode{ID: n.ID, Kind: n.Kind, Needs: n.Deps()})
		}
		fi.Waves, _ = fl.Waves()
		out = append(out, fi)
	}
	writeJSON(w, 200, out)
}

type runRequest struct {
	Flow     string `json:"flow"`
	Parallel int    `json:"parallel"`
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req runRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad request"})
		return
	}
	if req.Parallel < 1 {
		req.Parallel = 1
	}
	if req.Parallel > 64 {
		req.Parallel = 64
	}
	files, err := s.flowFiles()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	var file string
	for _, f := range files { // only a name from the listing is ever turned into a path
		if f == req.Flow {
			file = f
		}
	}
	if file == "" {
		writeJSON(w, 404, map[string]string{"error": "no such flow"})
		return
	}

	s.mu.Lock()
	if s.active != nil {
		s.mu.Unlock()
		writeJSON(w, 409, map[string]string{"error": "a run is already in progress"})
		return
	}
	s.nextID++
	ru := newRun(fmt.Sprintf("r%d", s.nextID))
	s.active = ru
	s.runs[ru.id] = ru
	s.order = append(s.order, ru.id)
	for len(s.order) > 20 {
		delete(s.runs, s.order[0])
		s.order = s.order[1:]
	}
	s.mu.Unlock()

	envPath, _ := s.envFor(file)
	go s.execute(ru, filepath.Join(s.cfg.Dir, file), envPath, req.Parallel)
	writeJSON(w, 202, map[string]string{"id": ru.id})
}

func (s *Server) execute(ru *run, path, envPath string, parallel int) {
	defer func() {
		s.mu.Lock()
		s.active = nil
		s.mu.Unlock()
	}()
	fail := func(err error) {
		ru.add(map[string]any{"type": "run_error", "error": err.Error()}, true)
	}
	fl, err := flow.Load(path)
	if err != nil {
		fail(err)
		return
	}
	env, err := runner.LoadEnv(envPath)
	if err != nil {
		fail(err)
		return
	}
	ru.secrets = runner.SecretValues(env)
	envName := strings.TrimSuffix(filepath.Base(envPath), filepath.Ext(envPath))
	var t0 time.Time
	rep, secrets, err := runner.Run(fl, env, runner.Options{EnvName: envName, Parallel: parallel, OnEvent: func(e runner.Event) {
		if e.Type == "run_start" {
			t0 = e.At
		}
		m := map[string]any{"type": e.Type, "t": e.At.Sub(t0).Milliseconds()}
		switch e.Type {
		case "run_start":
			m["flow"], m["parallel"], m["nodes"], m["env"] = fl.Name, parallel, e.Nodes, filepath.Base(envPath)
		case "step_start":
			m["id"], m["kind"] = e.ID, e.Kind
		case "step_done":
			m["id"], m["step"] = e.ID, e.Step
		}
		ru.add(m, false)
	}})
	if err != nil {
		fail(err)
		return
	}
	ru.secrets = secrets
	m := map[string]any{"type": "run_done", "t": rep.DurationMs, "run": rep}
	if s.cfg.EvidenceDir != "" {
		out := filepath.Join(s.cfg.EvidenceDir, fmt.Sprintf("%s-%s.json", fl.Name, time.Now().UTC().Format("20060102T150405Z")))
		if err := os.MkdirAll(s.cfg.EvidenceDir, 0o755); err == nil {
			if err = report.WriteJSON(out, rep, secrets); err == nil {
				m["evidence"] = out
			}
		}
	}
	ru.add(m, true)
}

// run is one execution: an append-only list of masked events that any number of
// browsers can replay and follow.
type run struct {
	id      string
	secrets []string

	mu     sync.Mutex
	events [][]byte
	done   bool
	wake   chan struct{}
}

func newRun(id string) *run { return &run{id: id, wake: make(chan struct{})} }

func (ru *run) add(v any, final bool) {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"type": "run_error", "error": err.Error()})
		final = true
	}
	b = report.MaskJSON(b, ru.secrets)
	ru.mu.Lock()
	ru.events = append(ru.events, b)
	ru.done = ru.done || final
	close(ru.wake)
	ru.wake = make(chan struct{})
	ru.mu.Unlock()
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ru := s.runs[r.PathValue("id")]
	s.mu.Unlock()
	if ru == nil {
		http.Error(w, "no such run", 404)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	fl.Flush()
	for i := 0; ; {
		ru.mu.Lock()
		batch, done, wake := ru.events[i:], ru.done, ru.wake
		ru.mu.Unlock()
		for _, e := range batch {
			fmt.Fprintf(w, "data: %s\n\n", e)
		}
		i += len(batch)
		fl.Flush()
		if done && len(batch) == 0 {
			return
		}
		if len(batch) > 0 {
			continue
		}
		select {
		case <-wake:
		case <-r.Context().Done():
			return
		}
	}
}

// Serve listens on addr and blocks.
func (s *Server) Serve(addr string) error {
	if err := CheckAddr(addr); err != nil {
		return err
	}
	if s.cfg.Token == "" {
		return errors.New("no token configured")
	}
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	return srv.ListenAndServe()
}
