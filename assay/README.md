# Assay

Assay runs **flows**: ordered API calls, assertions and database checks described in YAML, with an
evidence report of exactly what was sent, what came back, and why a step passed or failed.

- **Flows are data.** A flow is a small DAG of nodes; values from earlier nodes feed later ones.
- **Contract-aware.** Calls can be validated against a provider's published OpenAPI document, and disagreement
  with the live service is reported as drift.
- **Looks behind the API.** Read-only Postgres and MongoDB checks verify what the API actually did.
- **CI-friendly.** Distinct exit codes separate a wrong response from a flow mistake from an unreachable system.

## Quick start

    go build -o assay ./cmd/assay
    go run ./examples/mock &                      # a tiny local service to try it against
    ./assay validate examples/login-and-profile.flow.yaml
    APP_PASSWORD=s3cret ./assay run -env examples/local.env.yaml examples/login-and-profile.flow.yaml

Commands:

| Command | Purpose |
|---|---|
| `assay validate [--parallel N] <flow>` | Check a flow and print its execution order; with `--parallel`, also which nodes would run together |
| `assay run [-env env.yaml] [-out evidence.json] [--parallel N] <flow>` | Run a flow; `--parallel` runs up to N independent nodes at once (default 1) |
| `assay serve [-dir flows] [-env default.env.yaml] [-addr 127.0.0.1:8787]` | Local web dashboard: pick a flow, run it (optionally in parallel), watch it live; each flow uses its paired `<name>.env.yaml` |
| `assay scaffold -source NAME (-env env.yaml \| -spec openapi.json) [-list] ["GET /path"]` | Generate a starter node from an OpenAPI operation |

## Writing a flow

    version: 1
    name: login-and-profile
    nodes:
      - id: login
        kind: request
        request:
          method: POST
          url: "{{ .env.base_url }}/login"
          body: {username: "{{ .env.username }}", password: "{{ .secrets.APP_PASSWORD }}"}
          extract: {token: body.token}

      - id: login_ok
        kind: assertion
        assert:
          - {path: nodes.login.response.status, equals: 200}

      - id: profile
        kind: request
        request:
          url: "{{ .env.base_url }}/me"
          headers: {Authorization: "Bearer {{ .nodes.login.out.token }}"}

The flow format is described by `schema/flow.schema.json`. Add
`# yaml-language-server: $schema=../schema/flow.schema.json` to the top of a flow for editor validation.

### Ordering

Edges come from explicit `needs: [id, ...]` plus any `nodes.<id>` reference found in a node, so the example above
runs `login → login_ok → profile` without declaring dependencies. Cycles and unknown ids are rejected. If a node
fails, everything that depends on it is `skipped`.

### The run context

Templates (`{{ ... }}`) can read:

| Path | Contents |
|---|---|
| `.env.<name>` | `vars:` from the env file |
| `.secrets.<NAME>` | process environment variables listed under `secrets:` (masked in evidence) |
| `.nodes.<id>.response.{status,headers,body,raw}` | a request node's response (`body` is absent for non-JSON) |
| `.nodes.<id>.rows`, `.row_count` | a db node's result |
| `.nodes.<id>.out.<name>` | values a node `extract`ed or `transform`ed |

A string that is exactly one reference (`"{{ .nodes.a.out.count }}"`) keeps its native type; anything else is
rendered as text. An unresolved reference is an error, never an empty string.

### Environment file

Per-environment values live outside the flow:

    vars:
      base_url: ${BASE_URL}            # ${VAR} is expanded from the process environment; unset is an error
    secrets: [APP_PASSWORD]            # names of env vars; values are never written to evidence
    contracts:
      api: ${API_OPENAPI_URL}          # OpenAPI documents (URL, or a file path relative to this file)
    databases:
      ledger: ${LEDGER_DATABASE_URL}   # connection strings, used only by nodes that name them
      events: ${EVENTS_MONGO_URL}

## Node kinds

### `request`

`method` (default GET), `url`, `headers`, `query`, `body` (JSON), `timeout` (default 30s), and `extract`, a map of
output name → path in the response (`token: body.token`). Optional `contract:` (see below).

### `assertion`

A list of checks, each with a `path` into the context and one or more operators: `equals`, `exists`, `contains`,
`matches` (regex), `gt`, `lt`, plus an optional `message`. All checks are evaluated and reported.

### `transform`

`set` maps output names to values (templates allowed), available as `nodes.<id>.out.<name>`.

### `poll`

Repeat a request or a database query until a condition holds, for asynchronous work:

    - id: wait_done
      kind: poll
      poll:
        interval: 2s                       # default 1s
        backoff: {factor: 2, max: 10s, jitter: 0.2}   # optional: delay = interval * factor^(n-1), capped, +/- jitter
        timeout: 60s                       # default 30s, for the whole poll
        request:                           # or `db:` to poll a query
          url: "{{ .env.base_url }}/jobs/{{ .nodes.start.out.id }}"
          extract: {result: body.result}   # runs once, on the final response
        until:
          - {path: nodes.wait_done.response.body.status, equals: DONE}

`until` takes the same checks as an assertion and reads the latest result. Dropped connections are retried until
the window closes, and one slow attempt can't outlive it. The evidence lists every attempt.

### `db`

Read-only queries against **Postgres** or **MongoDB**, named in the env file's `databases:`.

Postgres (`type: postgres`, the default) takes static SQL with values bound as `$1, $2…`, never interpolated:

    - id: orphans
      kind: db
      db:
        connection: ledger
        query: SELECT count(*) AS n FROM attempts a LEFT JOIN actions r ON r.id = a.action_id WHERE r.id IS NULL
        extract: {n: rows.0.n}
      assert:
        - {path: nodes.orphans.rows.0.n, equals: 0}

MongoDB (`type: mongo`) takes a `find` (`filter`, ordered `sort`, `projection`) or an aggregation (`pipeline`).
Filters and pipelines are extended JSON (`{_id: {$oid: "..."}}`) and strings may be templates:

    - id: duplicate_keys
      kind: db
      db:
        type: mongo
        connection: events
        database: app
        collection: payments
        pipeline:
          - {$group: {_id: "$idempotency_key", n: {$sum: 1}}}
          - {$match: {n: {$gt: 1}}}
      assert:
        - {path: nodes.duplicate_keys.row_count, equals: 0}

Results are `nodes.<id>.rows` (a list of objects), `row_count` and `out.*`. Postgres types (uuid, timestamptz,
numeric, jsonb) become JSON; Mongo ObjectIds and dates become plain strings. `max_rows` (default 1000) guards against
huge results and `timeout` defaults to 10s.

**Read-only by construction.** Postgres queries run in a READ ONLY transaction that is always rolled back, wrapped
as a subquery so only `SELECT`/`WITH` can execute; a write smuggled behind a `SELECT` is rejected. For Mongo only
`find`/`aggregate` are issued, and pipelines containing `$out` or `$merge` anywhere are refused. One connection per
database is kept for the whole run and reused by every db node and poll attempt.

## Contracts and drift

Bind a request node to a provider's OpenAPI 3.x document:

    - id: get_user
      kind: request
      contract: api                        # or {source: api, on_violation: warn}
      request: {url: "{{ .env.base_url }}/users/42"}

Assay finds the operation by method and path template, then validates the outgoing JSON body (a violating
request is **not sent**) and the response (documented status, JSON Schema). Schemas are checked by a full JSON
Schema 2020-12 implementation, the dialect of OpenAPI 3.1; OpenAPI 3.0 `nullable` is upgraded automatically.

- A violation fails the step with `error_kind: contract`, unless the node sets `on_violation: warn`, which records
  the finding and lets the step pass (for accepted drift and deliberate negative tests).
- On a poll node the contract is judged once: the attempt that satisfied `until`, plus any undocumented status seen on
  earlier attempts. Intermediate bodies (`PENDING`) are not held to the final schema. See
  [`docs/decisions/0003-poll-contracts.md`](docs/decisions/0003-poll-contracts.md).
- Things the contract is too thin to check, such as a documented response with an empty schema, are recorded as
  `notes` / coverage gaps, not violations.
- Every run that used a contract gets a top-level `drift` block in the evidence and a "Contract drift" section in the
  CLI report: all findings (failing vs accepted), with step, operation, where, path and message, plus coverage gaps.
  Drift never crashes a run; the remaining steps still execute.
- The same block has a registry-wide view, `drift.sources`: for each contract source, how many of the operations the
  spec publishes this run actually called, and which ones it did not ("Registry coverage" in the CLI report). An
  operation no flow calls is one nobody checks against the live service, so it can drift unnoticed. This is
  informational: it never fails a run. `assay scaffold -list` shows what a spec publishes; scaffold a node for each gap.

`assay scaffold` prints a starter request node and a status assertion for one operation, with path parameters as
`{{ .env.<param> }}`, required query/header parameters, and a body skeleton built from the required properties.
Optional auth headers are listed as comments rather than emitted.

Design notes: [`docs/decisions/0001-contract-source.md`](docs/decisions/0001-contract-source.md) (why provider
published OpenAPI) and [`docs/decisions/0002-drift-reporting.md`](docs/decisions/0002-drift-reporting.md) (how drift
is reported) and [`docs/decisions/0005-registry-coverage.md`](docs/decisions/0005-registry-coverage.md) (Phase 4 scope).

## Evidence and results

Each run writes a JSON file (`evidence/<flow>-<timestamp>.json`, or `-out`) with, per step: request, response,
timing, check results, contract/poll/db details, output, and error. Secret values are replaced with `***`, and
database evidence records the connection *name*, never the connection string. A readable summary is printed too.

Failed steps carry an `error_kind`:

| Kind | Meaning |
|---|---|
| `assertion` | the system answered, but not as the flow expected |
| `contract` | the call disagrees with the published OpenAPI contract |
| `timeout` | a poll's condition never became true within its window |
| `infrastructure` | no usable answer: timeout, connection refused, DNS, unreachable database |
| `spec` | the flow itself is broken: bad template, regex, URL, SQL, unknown connection |

### Exit codes

| Code | Meaning |
|---|---|
| 0 | everything passed |
| 1 | an assertion, contract or poll-timeout failure |
| 2 | usage, config, secret or flow error (results can't be trusted) |
| 3 | infrastructure failure only (safe for CI to retry) |

When a run has several kinds of failure, 2 wins over 1, and 1 over 3.

## Development

    go build ./...
    go vet ./...
    go test ./...

Some tests run against real servers and are skipped unless pointed at one. Use a scratch database; the tests create
and drop their own data:

    ASSAY_TEST_PG_DSN=postgres://user:pw@localhost:5432/scratch?sslmode=disable go test ./internal/runner -run LivePG
    ASSAY_TEST_MONGO_DSN=mongodb://127.0.0.1:27017/ go test ./internal/runner -run LiveMongo
    ASSAY_TEST_OPENAPI=/path/or/url/openapi.json go test ./internal/registry -run Skeletons

Layout:

    cmd/assay/        CLI (run, validate, scaffold) and exit codes
    internal/flow/      flow spec types, validation, DAG ordering
    internal/runner/    execution: request, assertion, transform, poll, db nodes; context and env
    internal/registry/  OpenAPI loading, operation matching, schema validation, scaffolding
    internal/report/    evidence model, drift summary, CLI report
    internal/serve/     the local dashboard (server and embedded page)
    schema/             JSON Schema for flow files
    examples/           a sample flow, env file and mock service
    docs/decisions/     design decisions
    .github/workflows/  a manually triggered workflow that runs a flow headless

## Dashboard (experiment)

    APP_PASSWORD=s3cret assay serve -dir examples -env examples/local.env.yaml

prints a link such as `http://127.0.0.1:8787/#token=...`. Each flow runs with its own environment: `foo.flow.yaml`
(or `foo.yaml`) in `-dir` pairs with `foo.env.yaml` next to it, and a flow with no pair uses the `-env` default. The page
shows which one a flow will use, and if that environment cannot work (an `${VAR}` or a secret that is not set in the
environment `assay serve` was started with) it says so and disables Run instead of failing mid-run. The page lists the flows in `-dir`, runs one with the
parallelism you choose, and shows it live: a timeline with one bar per step (so overlap is visible), the status of
each step as it finishes, and for any step its request, response, checks, contract findings, poll attempts or query.
After a run it shows contract drift and registry coverage. Each run's evidence JSON is still written to `-evidence`
(default `evidence/`), same as `assay run`.

It runs flows with this process's secrets, so it is locked down: it only listens on loopback (any other `-addr`
is refused), every API call needs the token, requests with a non-loopback `Host` are rejected, the browser can only
pick a flow from the directory listing (never a path), secret values are masked in everything sent to the browser, and
only one run goes at a time. A client never chooses an env file either: the pairing is decided from the file listing. It is a single embedded page with no external resources. See
[`docs/decisions/0006-dashboard.md`](docs/decisions/0006-dashboard.md).

## Parallel execution

`--parallel N` runs up to N independent nodes at once. A node starts when everything it depends on (`needs:` plus any
`nodes.<id>` it references) has finished, and runs only if all of that passed; a failure skips just its dependents,
and unrelated branches carry on. Evidence lists steps in flow order whatever the timing, and each step's `started_at`
and `duration_ms` show the overlap.

**File order is no longer an ordering guarantee.** With `--parallel 1` (the default) nodes run in flow order, so a flow
that relies on that without declaring `needs:` still works. With `--parallel 4` it may not: if one node must finish
before another (reset data, then call the API), say so with `needs:`. `assay validate --parallel 4 flow.yaml`
lists the waves of nodes that would run together, so a missing `needs:` shows up before a run. Postgres nodes share a
connection pool sized to N. See [`docs/decisions/0004-parallel-execution.md`](docs/decisions/0004-parallel-execution.md).

## Limitations

- Postgres is the only SQL database supported.
- Parallel execution (`--parallel N`) is opt-in, and only `needs:` and `nodes.<id>` references order nodes. See below.
- `contract:` works on poll nodes that poll a request, not on db polls.
- Secrets come from environment variables only; there is no vault integration.
- Only `application/json` (and `+json`) bodies are validated against contracts. A body documented only as another type
  (XML, text, form) is recorded as a note on the step, not validated.
- OpenAPI `servers` base paths (`/api/v1`, with variable defaults) are stripped when matching a call to an operation;
  a request made through a path the spec never mentions still matches as-is.
