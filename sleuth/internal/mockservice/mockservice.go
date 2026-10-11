// Package mockservice is a small HTTP service for trying Sleuth and for its end-to-end tests.
// It needs nothing outside the process: authentication, a job that finishes after a few
// polls, endpoints that hang or drop connections, endpoints whose responses match their
// published OpenAPI document and endpoints that deliberately do not.
package mockservice

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Credentials the service accepts. The defaults are what examples/ use.
type Credentials struct {
	Username, Password, Token string
	UserID                    int
}

// DefaultCredentials is alice / s3cret, issuing the token tok-123 for user 42.
var DefaultCredentials = Credentials{Username: "alice", Password: "s3cret", Token: "tok-123", UserID: 42}

// Service is the mock. Create it with New and serve Handler().
type Service struct {
	creds Credentials

	mu    sync.Mutex
	jobs  map[string]*job
	flaky map[string]int
	next  int
	// profile is which version of GET /profile is served; see SetProfileVersion.
	profile int
}

type job struct {
	readyAfter int
	polls      int
}

// New returns a service that accepts the given credentials (DefaultCredentials if zero).
func New(c ...Credentials) *Service {
	cr := DefaultCredentials
	if len(c) > 0 {
		cr = c[0]
	}
	return &Service{creds: cr, jobs: map[string]*job{}, flaky: map[string]int{}, profile: 1}
}

// Handler routes every endpoint:
//
//	GET  /health              healthy, as documented
//	POST /login               {"username","password"} -> {"token","user_id"}, or 401
//	GET  /users/{id}          needs "Authorization: Bearer <token>" or "x-access-token: <token>"; 404 for other ids
//	GET  /items/{id}          as documented; id 0 is a 404 the spec does not list
//	GET  /legacy/{id}         DRIFTS: "total" is a string, the spec says number
//	GET  /teapot              418, and not in the spec at all
//	POST /jobs?ready_after=N  202 + job id; GET /jobs/{id} says PENDING until its Nth poll, then DONE
//	GET  /big                 an id beyond 2^53 and a decimal that float64 would reformat
//	POST /echo                returns the request body byte for byte
//	GET  /slow?ms=N           answers after N milliseconds
//	GET  /hang                never answers (until the client gives up)
//	GET  /flaky/{key}         drops the connection on its first two calls, then answers
//	GET  /profile             NOT in the spec. Its response structure changes with the version set by
//	                          POST /admin/profile-version?v=N (or SetProfileVersion): 1 is the original, 2 changes
//	                          several fields, 3 is a 404. This is the case a spec diff cannot see
//	GET  /orders              a list whose items sometimes lack "note" (also not in the spec)
//	GET  /openapi.json        the published contract; /openapi.v2.json is a later, breaking version
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, `{"status":"healthy","version":"1.0.0"}`)
	})
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("GET /users/{id}", s.user)
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil || id == 0 {
			writeJSON(w, 404, `{"error":"not found"}`)
			return
		}
		writeJSON(w, 200, fmt.Sprintf(`{"id":%d,"name":"item-%d"}`, id, id))
	})
	mux.HandleFunc("GET /legacy/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.Atoi(r.PathValue("id"))
		writeJSON(w, 200, fmt.Sprintf(`{"id":%d,"total":"12.50"}`, id)) // a string where the spec says number
	})
	mux.HandleFunc("GET /teapot", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 418, `{"error":"teapot"}`)
	})
	mux.HandleFunc("POST /jobs", s.createJob)
	mux.HandleFunc("GET /jobs/{id}", s.pollJob)
	mux.HandleFunc("GET /big", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, `{"id":9007199254740993,"price":10.50}`)
	})
	mux.HandleFunc("POST /echo", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		w.Header().Set("Content-Type", "application/json")
		w.Write(b)
	})
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
		select {
		case <-time.After(time.Duration(ms) * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		writeJSON(w, 200, fmt.Sprintf(`{"slept":%d}`, ms))
	})
	mux.HandleFunc("GET /hang", func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Minute):
		}
	})
	mux.HandleFunc("GET /flaky/{key}", s.flakyEndpoint)
	mux.HandleFunc("GET /profile", s.profileHandler)
	mux.HandleFunc("POST /admin/profile-version", func(w http.ResponseWriter, r *http.Request) {
		v, err := strconv.Atoi(r.URL.Query().Get("v"))
		if err != nil || v < 1 || v > 3 {
			writeJSON(w, 400, `{"error":"v must be 1, 2 or 3"}`)
			return
		}
		s.SetProfileVersion(v)
		writeJSON(w, 200, fmt.Sprintf(`{"version":%d}`, v))
	})
	mux.HandleFunc("GET /orders", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, `[{"id":1,"total":10.5,"note":"gift"},{"id":2,"total":7}]`)
	})
	mux.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, string(Spec(1))) })
	mux.HandleFunc("GET /openapi.v2.json", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, string(Spec(2))) })
	return mux
}

func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, 400, `{"error":"bad request"}`)
		return
	}
	if body["username"] != s.creds.Username || body["password"] != s.creds.Password {
		writeJSON(w, 401, `{"error":"bad credentials"}`)
		return
	}
	writeJSON(w, 200, fmt.Sprintf(`{"token":%q,"user_id":%d}`, s.creds.Token, s.creds.UserID))
}

func (s *Service) authorized(r *http.Request) bool {
	return r.Header.Get("Authorization") == "Bearer "+s.creds.Token || r.Header.Get("x-access-token") == s.creds.Token
}

func (s *Service) user(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, 401, `{"error":"unauthorized"}`)
		return
	}
	if r.PathValue("id") != strconv.Itoa(s.creds.UserID) {
		writeJSON(w, 404, `{"error":"no such user"}`)
		return
	}
	writeJSON(w, 200, fmt.Sprintf(`{"id":%d,"username":%q,"balance":100.5}`, s.creds.UserID, s.creds.Username))
}

func (s *Service) createJob(w http.ResponseWriter, r *http.Request) {
	ready := 3
	if v, err := strconv.Atoi(r.URL.Query().Get("ready_after")); err == nil && v > 0 {
		ready = v
	}
	s.mu.Lock()
	s.next++
	id := fmt.Sprintf("job-%d", s.next)
	s.jobs[id] = &job{readyAfter: ready}
	s.mu.Unlock()
	writeJSON(w, 202, fmt.Sprintf(`{"id":%q,"status":"PENDING"}`, id))
}

func (s *Service) pollJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	j, ok := s.jobs[id]
	var done bool
	if ok {
		j.polls++
		done = j.polls >= j.readyAfter
	}
	s.mu.Unlock()
	switch {
	case !ok:
		writeJSON(w, 404, `{"error":"no such job"}`)
	case done:
		writeJSON(w, 200, fmt.Sprintf(`{"id":%q,"status":"DONE","result":"ok"}`, id))
	default:
		writeJSON(w, 200, fmt.Sprintf(`{"id":%q,"status":"PENDING"}`, id))
	}
}

func (s *Service) flakyEndpoint(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	s.mu.Lock()
	s.flaky[key]++
	n := s.flaky[key]
	s.mu.Unlock()
	if n <= 2 {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close() // an instant connection error, not a slow one
				return
			}
		}
	}
	writeJSON(w, 200, fmt.Sprintf(`{"ok":true,"calls":%d}`, n))
}

// SetProfileVersion changes what GET /profile returns, as if someone changed the DTO mid-testing.
func (s *Service) SetProfileVersion(v int) {
	s.mu.Lock()
	s.profile = v
	s.mu.Unlock()
}

func (s *Service) profileHandler(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	v := s.profile
	s.mu.Unlock()
	switch v {
	case 2: // id is now a string, balance is gone, tags can be null, address lost zip, nick is new
		writeJSON(w, 200, `{"id":"42","name":"alice","tags":null,"address":{"city":"Addis"},"nick":"al"}`)
	case 3:
		writeJSON(w, 404, `{"error":"gone"}`)
	default:
		writeJSON(w, 200, `{"id":42,"name":"alice","balance":100.5,"tags":["a","b"],"address":{"city":"Addis","zip":"1000"}}`)
	}
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	io.WriteString(w, strings.TrimSpace(body))
}
