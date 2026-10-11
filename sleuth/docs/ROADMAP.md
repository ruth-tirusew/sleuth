# Sleuth roadmap

Status as of 2026-10-11. This replaces the original phase checklist as the working plan; decision records in
`docs/decisions/` stay the source of truth for *why* things are built the way they are.

## Where things stand

Built and tested (`go test ./...` green):

| Area | What exists |
|---|---|
| Runner | YAML flows as a DAG; `request`, `assertion`, `transform`, `poll`, `db` nodes; `--parallel N` (0004) |
| Evidence | per-step JSON evidence, CLI report, `error_kind`, exit codes 0/1/2/3, secret masking |
| Contracts | OpenAPI validation of requests and responses, contracts on request polls (0003), `scaffold` |
| Drift | per-step and run-level drift, registry coverage (0005), `diff`, baselines and `-spec-drift-fail` |
| Data checks | read-only Postgres and MongoDB queries, polling a query, connection reuse/pool |
| Dashboard | `serve`: local, token-protected live view of a run (0006, experiment) |

Not started or paused: CI placement and the team pilot (paused), UI nodes (original Phase 5), the Phase 0 flow
conversion (needs the flow), vault secrets.

## Milestone 1 — Correctness fixes from the 2026-10-11 review

**Status: implemented 2026-10-11; follow-up review done and its findings handled (below).** All ten fixes are in
with tests. For findings 1, 2, 3, 4, 7 and 9 the new tests were run against the old code (or the fix was mutated
away) and fail there. Findings 5, 6, 8 and 10 have new tests that prove the new behaviour only. `go test -race ./...`
passes, and the opt-in live tests pass against real Postgres and MongoDB.

Decisions made while fixing: (1) finding 1 reclassifies a failed attempt that the poll's own deadline cut short,
rather than imposing a minimum attempt budget; (2) finding 7 covers HTTP bodies too, since they had the same
rounding; (3) a drift block records `calls_checked`, so "no findings" cannot be mistaken for "checked".

**Follow-up review (10 findings on the fixes above).** Eight were real and are fixed, each with a test that failed first:
the "cut short" test treated every failure in a short poll as the window's fault (now only deadline errors count);
dropping a Mongo client could discard a healthy one another parallel node had put in its place, and a slow query
evicted it (now only the client that failed, and never for the node's own deadline); a poll that gave up discarded
real, persistent schema drift (now findings present on every answered attempt are kept, see 0003); dependency
inference missed `(index .nodes "id")` and treated any body field called `path` as a reference (now templates are
parsed with Go's parser, `path` counts only in `assert` / `until`, and a template reading `.nodes` untraceably must
list `needs:`); `equals` read its expected number through a YAML float (now exact for checks); the exact-number bound
used string length (now the exponent's value); and a Postgres decode error message printed `<nil>`.
Two were considered and left as they are: an empty `drift` block when no contract step ran (a skipped contract node
means an upstream step failed, so the run is already failed, and `calls_checked: 0` plus the report line say nothing
was checked); and the retry on a stale Mongo client sharing the node's deadline (shortening the first try would
punish slow but healthy queries, and the driver reconnects on its own).

**Third review (10 findings on the follow-up fixes).** Seven were real and are fixed, each with a test: a node
reference through the template root (`$.nodes.a`) was missed; a check using a YAML merge key (`<<: *base`) was rejected
(plain aliases were never broken: the claim was half wrong, shown by mutation); check paths are now taken from the
typed checks instead of key names; two equal huge numbers written differently compared unequal (now compared as
arbitrary-exponent floats, or as text when over-long, never rounded); a failed Mongo client is now retired and closed at
the end of the run, not disconnected under other parallel nodes; the fragile "context deadline exceeded" text match is
gone (the driver wraps the context error, checked); a poll with a single answered attempt now says in a note that it held
a finding back instead of dropping it silently; and the duplicate number-literal definition is now one exported helper.
Three were declined: node references in map keys (keys are never rendered, so there is nothing to order); evicting a Mongo
client on a context deadline (the driver monitors and reconnects on its own, and a dead host stays dead for a new client
too); and the `truncated` flag being overwritten per attempt (it is read immediately after each attempt, which is correct).

Every item below was checked against the code; the first was reproduced. Each fix lands with a test that fails
before it.

| # | Problem | Fix | Test |
|---|---|---|---|
| 1 | **A starved last poll attempt turns "never converged" into "infrastructure".** After the final sleep the last attempt can get a few ms; if it times out, the poll exits 3 ("safe to retry") although every earlier attempt answered. (`runner/poll.go`) | Track whether any attempt got an answer: if so the verdict is `timeout` (exit 1); `infrastructure` only when no attempt ever answered. Also don't start an attempt with less than a minimum budget. | Fake server answering PENDING with ~300 ms latency, timeout chosen so the last attempt starves → expect `timeout`. |
| 2 | **Spec-drift gate is bypassed when no contract step ran.** If every contract node is skipped (e.g. login failed), `Drift` stays nil, `AddSpecDrift`/`AddCoverage` return early, and `-spec-drift-fail breaking` is not enforced. (`report/drift.go`) | Create the drift block when the flow *references* a contract source (or a baseline is configured), not only when a step produced contract evidence. | Flow whose contract node is skipped + breaking baseline + `-spec-drift-fail breaking` → `spec_drift` present, run failed. |
| 3 | **Timed-out contract polls report intermediate shapes as failing drift** and drop undocumented statuses carried from earlier attempts (only `finish` merges them). (`runner/poll.go`) | On the not-satisfied path, discard deferred response findings from the last attempt and merge the carried ones, as `finish` does. | Poll that never converges with a PENDING body off-schema and an undocumented 202 earlier → no shape findings, the 202 is reported. |
| 4 | **A valid flow is rejected:** a URL like `https://nodes.cluster.local/api` (or a message mentioning `nodes.x`) is read as a dependency on node `cluster`. (`flow/flow.go`, `Deps`) | Infer dependencies only from template expressions (`{{ ... .nodes.<id> ... }}`) and from check `path:` fields, not from arbitrary text. | The reproduced flow validates; an existing template reference still creates the edge. |
| 5 | **Carried poll findings de-duplicated by message only**, so different findings with the same text are lost. (`runner/poll.go`) | Key on where + path + message. | Two findings, same message, different paths → both kept. |
| 6 | **Mongo client lifecycle.** The cached client's server-selection timeout is fixed by whichever call created it (possibly a starved poll attempt), and a broken client is never evicted or retried, unlike Postgres. (`runner/dbmongo.go`) | Create clients with the node's full timeout, apply per-operation deadlines via context, and drop/reconnect once on a topology error. | Unit test of the eviction path; live test (opt-in) still green. |
| 7 | **Large integers lose precision.** Values above 2^53 (Mongo NumberLong, Postgres bigint) and decimals are decoded to float64. (`runner/dbmongo.go`, `runner/db.go`) | Decode with `UseNumber` and keep integers exact; render decimals as strings; compare numbers exactly in `equals`. | `9007199254740993` round-trips and compares equal. |
| 8 | **Mongo server errors read "mongo error false"** (a bool printed as the code). (`runner/dbmongo.go`) | Report the real error code/name from `mongo.CommandError`. | Bad operator → message contains the code. |
| 9 | **The CLI printer reorders evidence.** `printDrift` sorts the run's findings in place. (`report/drift.go`) | Sort a copy. | Print, then check findings are still in step order. |
| 10 | Tidy: `snapshot` parses the spec twice (`mustOps`); dead `desc` variable in the Mongo path. | Reuse the parsed spec; delete dead code. | Existing tests. |

**Exit criteria:** all ten fixed, each with its test; `go vet` and `go test ./...` green; a follow-up review of the
changed files finds nothing at the same severity.

## Milestone 2 — A self-contained end-to-end suite

The live end-to-end battery went away with the service-specific files, so today nothing exercises a whole run
against a running service. Replace it with one that needs nothing outside the repo.

- Grow `examples/mock` into a test service with: a health check, an authenticated endpoint, a hanging endpoint, a
  job that completes after N polls, a served `/openapi.json` plus a deliberately drifted copy, and an endpoint whose
  response differs from its spec.
- `make e2e` (or `go test -tags e2e ./e2e`) starts the mock, runs a battery of flows and checks exit codes,
  `error_kind`s, drift/coverage/spec-drift blocks and secret masking, the way the old self-test did.
- Database scenarios stay opt-in via `SLEUTH_TEST_PG_DSN` / `SLEUTH_TEST_MONGO_DSN` (scratch databases only).
- Run it with `--parallel 1` and `--parallel 4` to catch ordering bugs.

**Exit criteria:** one command, no external services, covers every exit code and node kind, and runs in under a
minute.

## Milestone 3 — Close the known limits

In rough order of value:

1. **Contracts on db polls** are not meaningful (there is no HTTP contract), so instead make the README say so
   explicitly rather than listing it as a gap.
2. **Coverage threshold** (`--min-coverage`, 0005 item 1): opt-in gate on registry coverage.
3. **Secrets from a file or a vault** behind one interface (`env` stays the default). Pick one backend to start.
4. **Other SQL databases** only if a real flow needs one; the subquery/read-only-transaction approach is
   Postgres-specific and has to be re-proven per engine.

## Milestone 4 — Real use (resumes when CI is unpaused)

The original Phase 3, unchanged in intent: trust comes from real incidents, not coverage.

- Convert the Phase 0 flow (needs the flow itself) and run it headless.
- Place one flow in a pipeline after unit tests, on PRs or nightly, not on every commit; retry on exit 3 only.
- Let it catch or explain one real regression before showing it to anyone; share that evidence report with someone
  who was in a past diagnosis loop.
- Add the next most painful flow, not the easiest. Log each one here: date, flow, why.

**Prerequisite:** Milestones 1 and 2 done, so the first real failures are product failures, not Sleuth bugs.

## Milestone 5 — UI nodes with traced evidence (the differentiator)

Start only after Milestones 1–4 have been stable in real use for a while.

1. **Decision records first:** browser layer (Playwright, for network interception); UI as a node kind via a plugin
   interface, not a runner change; the AI-locator boundary (AI may propose a selector diff, a human approves, AI
   never writes or runs the test).
2. **Browser node:** navigate / click / type / assert.
3. **Network capture** during a UI step, each captured call validated against the registry like a request node.
4. **Correlation ID** threaded from the UI step through every call it causes.
5. **One evidence chain:** UI step → triggered API calls → contract findings → db state.
6. **Scale-readiness:** a separate browser worker pool (bulkhead), infra-vs-product failure separation for browser
   launch problems (circuit breaker), and a retention policy for screenshots/video/DOM snapshots.

## Decisions needed

| Decision | Options | Needed by |
|---|---|---|
| Phase 0 flow | provide it, or pick one real flow as the pilot instead | Milestone 4 |
| CI | which system and repo, which environment it can reach | Milestone 4 |
| Secrets backend | file only, or a specific vault | Milestone 3.3 |
| Dashboard | keep as an experiment, promote, or drop | before Milestone 5 |
| Parent folder | the repo lives in `condiut/` (typo); rename or leave | any time |

## Suggested order

Milestone 1 → Milestone 2 → Milestone 3.1–3.2 → (unpause) Milestone 4 → Milestone 5. Milestone 3.3–3.4 slot in when
a real flow needs them.
