package mockservice

import "encoding/json"

// Spec returns the OpenAPI 3.1 document the service publishes.
//
// Version 1 is what the service actually does, with two deliberate exceptions: GET /legacy/{id}
// documents "total" as a number but the service sends a string, and GET /items/{id} and
// GET /users/{id} do not list the 404 they can return.
//
// Version 2 is a later document with breaking changes, for testing drift against a saved
// baseline: GET /users/{id} loses "balance" in its response, GET /legacy/{id} is removed,
// POST /login now requires a "currency" in its request, and "id" in GET /items/{id} becomes a
// string. It also adds a compatible operation, GET /v2only.
func Spec(version int) []byte {
	b, _ := json.MarshalIndent(buildSpec(version), "", "  ")
	return b
}

type m = map[string]any

func obj(required []string, props m) m {
	o := m{"type": "object", "properties": props}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

func jsonResp(desc string, schema m) m {
	return m{"description": desc, "content": m{"application/json": m{"schema": schema}}}
}

func errResp() m { return jsonResp("error", obj([]string{"error"}, m{"error": m{"type": "string"}})) }

func buildSpec(v int) m {
	str, integer, number := m{"type": "string"}, m{"type": "integer"}, m{"type": "number"}

	userProps := m{"id": integer, "username": str, "balance": number}
	userRequired := []string{"id", "username", "balance"}
	loginRequired := []string{"username", "password"}
	loginProps := m{"username": str, "password": str}
	itemID := m(integer)
	if v >= 2 {
		delete(userProps, "balance")
		userRequired = []string{"id", "username"}
		loginRequired = []string{"username", "password", "currency"}
		loginProps["currency"] = str
		itemID = str
	}

	paths := m{
		"/health": m{"get": m{"summary": "Health check", "responses": m{
			"200": jsonResp("healthy", obj([]string{"status"}, m{"status": m{"enum": []string{"healthy"}}, "version": str}))}}},
		"/login": m{"post": m{"summary": "Log in",
			"requestBody": m{"required": true, "content": m{"application/json": m{"schema": func() m {
				o := obj(loginRequired, loginProps)
				o["additionalProperties"] = false
				return o
			}()}}},
			"responses": m{"200": jsonResp("ok", obj([]string{"token", "user_id"}, m{"token": str, "user_id": integer})), "401": errResp()}}},
		"/users/{id}": m{"get": m{"summary": "Get a user",
			"parameters": []m{
				{"name": "id", "in": "path", "required": true, "schema": integer},
				{"name": "x-access-token", "in": "header", "required": false, "schema": str},
				{"name": "Authorization", "in": "header", "required": false, "schema": str}},
			"responses": m{"200": jsonResp("the user", obj(userRequired, userProps)), "401": errResp()}}},
		"/items/{id}": m{"get": m{"summary": "Get an item",
			"parameters": []m{{"name": "id", "in": "path", "required": true, "schema": integer}},
			"responses":  m{"200": jsonResp("the item", obj([]string{"id", "name"}, m{"id": itemID, "name": str}))}}},
		"/jobs": m{"post": m{"summary": "Start a job",
			"parameters": []m{{"name": "ready_after", "in": "query", "required": false, "schema": integer}},
			"responses":  m{"202": jsonResp("accepted", obj([]string{"id", "status"}, m{"id": str, "status": str}))}}},
		"/jobs/{id}": m{"get": m{"summary": "Get a job",
			"parameters": []m{{"name": "id", "in": "path", "required": true, "schema": str}},
			"responses": m{"200": jsonResp("the job", obj([]string{"id", "status"},
				m{"id": str, "status": m{"enum": []string{"PENDING", "DONE"}}, "result": str}))}}},
		"/big": m{"get": m{"summary": "A large id and a decimal", "responses": m{
			"200": jsonResp("numbers", obj([]string{"id", "price"}, m{"id": integer, "price": number}))}}},
		"/echo": m{"post": m{"summary": "Echo the body",
			"requestBody": m{"required": true, "content": m{"application/json": m{"schema": m{}}}},
			"responses":   m{"200": jsonResp("the same body", m{})}}},
	}
	if v < 2 {
		paths["/legacy/{id}"] = m{"get": m{"summary": "Get a legacy record",
			"parameters": []m{{"name": "id", "in": "path", "required": true, "schema": integer}},
			"responses":  m{"200": jsonResp("the record", obj([]string{"id", "total"}, m{"id": integer, "total": number}))}}}
	} else {
		paths["/v2only"] = m{"get": m{"summary": "Added in v2", "responses": m{"200": jsonResp("ok", obj(nil, m{}))}}}
	}
	return m{"openapi": "3.1.0", "info": m{"title": "Sleuth mock service", "version": map[int]string{1: "1.0.0", 2: "2.0.0"}[v]}, "paths": paths}
}
