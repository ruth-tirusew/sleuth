// A small service for trying sleuth locally: go run ./examples/mock
//
// It listens on :18099 and accepts alice / s3cret (the credentials examples/local.env.yaml
// expects). See internal/mockservice for every endpoint, including the published OpenAPI
// document at /openapi.json.
package main

import (
	"log"
	"net/http"

	"sleuth/internal/mockservice"
)

func main() {
	log.Println("mock service on http://localhost:18099 (login: alice / s3cret)")
	log.Fatal(http.ListenAndServe(":18099", mockservice.New().Handler()))
}
