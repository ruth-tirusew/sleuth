// Tiny mock service for trying sleuth locally: go run ./examples/mock
package main

import (
	"encoding/json"
	"log"
	"net/http"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["password"] != "s3cret" {
			http.Error(w, `{"error":"bad credentials"}`, 401)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"token": "tok-123", "user_id": 42})
	})
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-123" {
			http.Error(w, `{"error":"unauthorized"}`, 401)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": 42, "username": "alice", "balance": 100.5})
	})
	log.Fatal(http.ListenAndServe(":18099", mux))
}
