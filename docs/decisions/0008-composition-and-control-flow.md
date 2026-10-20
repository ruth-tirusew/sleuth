# 0008 — Composition and control flow (proposal, not built)

**Status:** proposed 2026-10-12. Subflows (step 4 below) were built first at the user's request; see 0009. `finally` (step 2) was built 2026-10-20 as specified, with the choices below. The rest is not implemented. Reviewed against the code as of commit `af3dbdf`.

## Position
Keep the YAML small. Reuse comes from **composing flows**, branching chooses **which nodes run** (never what a node
computes), and anything that needs real computation is out of scope until a real flow demands it. Accepted from the
proposal: subflows, `finally`, node-level `if`, data-driven iteration, a small template-function set, and the list of
refusals (no mutable variables, no `while`, no `break`, no dynamic node generation). Changed or deferred: below.

## What the code looks like today (it decides what is cheap)
- A flow is a flat list of nodes; the scheduler (`runner.Run`) orders them by dependencies inferred from templates and
  `needs:`, and a node whose dependency did not pass is **skipped**. Skipped therefore always means "upstream failed".
- Templates are Go `text/template`. A string that is exactly one `{{ .path }}` keeps its native type (`pureRe` in
  `context.go`); anything else renders to a string.
- Env `vars` are `map[string]string`: **every env value is a string**. There are no lists or booleans in env.
- Evidence is a flat `[]Step`. Drift summary, coverage, JUnit, shapes, the dashboard and the e2e suite all read it flat.
- The run has one worker pool (`--parallel N`) and one run-wide timeout context.

## Review of the proposal

### Subflows — build, but it is the expensive one
Right idea, and it fits the DAG: a `kind: flow` node depends on whatever its `params` templates reference. Hidden
internals come for free if the child gets a fresh `Context` (env, secrets, params; empty `nodes`). Problems to settle:
1. **Evidence is flat everywhere.** Nesting needs `Step.Children` and step ids as paths (`auth/login`). `SummarizeDrift`,
   `AddCoverage`, JUnit, shapes, the dashboard timeline and the evidence schema must all descend. This refactor is most
   of the work; do it first and alone.
2. **Worker-pool deadlock.** A subflow holding a worker slot while its own nodes wait for slots deadlocks at `--parallel 1`.
   Decision: a subflow runs serially inside its parent's slot. Parallelism applies to the outermost flow only (state this
   in the docs; revisit if a real flow needs more).
3. **"Typed params" does not exist** in a stringly-typed env. Params keep native types through `pureRe`, so a param can
   carry a number from an upstream node. Add `required` and `default`; skip `type:` until a flow needs it.
4. **Env inheritance and secrets.** Secrets are named only in the env file; a subflow that "declares additional secrets"
   would be a second source of truth. A subflow may *list* the secrets it needs and `validate` checks they are in the env
   file. It cannot add any.
5. **Static checks.** `validate` must resolve every subflow path, check params against the child's declared params, check
   `outputs:` references, and reject cycles and a depth over 8. Paths resolve relative to the including file and must stay
   under the top-level flow's directory (no `../../`), which also keeps the dashboard's "client cannot name a path" rule true.
6. **Failure and exit code.** A subflow node fails if any child fails; its error kind is the most severe child kind
   (infrastructure over product, so a retry policy is not misled).

### `foreach` — narrower than proposed
- **Env cannot hold a list**, so `foreach: "{{ .env.test_order_ids }}"` (the proposal's example) cannot work today. Sources
  for v1: an inline YAML list, or an array produced by an earlier node. A list-valued env var is a separate, later change
  to `EnvConfig.Vars`.
- **Only on `kind: flow` nodes.** Then one iteration is one subflow call: evidence nests as already designed, and we avoid
  defining what iteration means for `poll`, `db` and contract nodes. Flow-level `foreach` is the same thing written as a
  parent flow, so it is dropped (one mechanism, not two). Request-level `foreach` can be sugar later.
- `on_iteration_failure: fail|collect` (default `fail`); `parallel: N` is bounded by the shared pool and counted against it.
- Evidence for iterations is capped, with "… and N more" as the dashboard coverage list already does.

### `if:` / `switch` — `if` yes, `switch` later
- **Syntax.** The proposal's `{{ .env.env_type == 'prod' }}` is not Go template syntax. Use what already parses:
  `{{ eq .env.env_type "prod" }}`, with `and`, `or`, `not`. No second expression language. The "two conditions maximum"
  rule is arbitrary and unenforceable; drop it.
- **Truthiness trap.** Env values are strings, and in Go templates the string `"false"` is truthy. Define it explicitly:
  empty, `"false"` and `"0"` are false (case-insensitive); everything else true. Test it.
- **A new status.** Today skipped means "upstream failed" and cascades. A node skipped by its own condition is not a
  failure: add status `not_run` (reason recorded), shown separately in CLI, JUnit (skipped) and the dashboard. Dependents
  that read a `not_run` node's output get a clear error unless they use `default`; a dependent that only *orders* after it
  proceeds. Evaluate the condition when the node becomes ready, so it can read upstream outputs.
- **`switch`** only if every branch declares the same `outputs:` (checked at validate time); otherwise downstream
  references are unsound. Two `if` nodes cover the first real cases, so wait.

### `finally` — build early (already on the roadmap as "teardown nodes")
- Runs after all main nodes, serially, even when the main flow failed. A finally node whose dependencies did not pass is
  skipped (nothing to clean up).
- **The run timeout must not cancel cleanup.** Cleanup runs under its own short grace context (default 30s,
  configurable). Otherwise teardown never runs exactly when a run hung.
- **Reported separately**, kind `cleanup`: a failed cleanup fails the run (state leaked) but never replaces the primary
  failure or changes its exit code reason.
- Same scoping inside subflows: a subflow's `finally` runs when that subflow ends.
- Cleanup is API-only; `db` nodes stay read-only.
- Interrupt handling (Ctrl-C) is not wired anywhere today; decide whether SIGINT also triggers `finally` before relying on it.

### Template functions — yes, with a masking rule the proposal misses
- Syntax is Go's: `{{ .x | urlquery }}`, `{{ join "," .ids }}`; the proposal's `| join: ","` is Liquid, not Go. Register
  with `template.FuncMap`. Start with `urlquery` (built in), `join`, `prefix`, `first`, `default`, `lower`, `upper`,
  `b64enc`.
- **Derived secrets leak.** A function applied to a secret produces a new string that the masker has never seen; this
  already happened once (secrets leaking percent-encoded into evidence, fixed 2026-10-12). Any function result computed
  from a secret-tainted input must be registered with the masker. Simplest rule: if an argument equals a secret value, its
  output is added to the masked set. Needs a test per function.
- **Native types.** A pipeline falls outside `pureRe`, so a function returning a number would become a string. Make a
  single-action template keep the function's native result.
- `map`/`filter`/`sort`/`group_by`/`reduce`: refused.

### Script node — deferred
Not needed by any real flow yet, adds a runtime dependency, and the proposal offers three languages. If it ever comes:
one language (CEL), no I/O, wall-clock and memory limits, inputs and outputs recorded and masked like any step. Until then
the answer is `transform` plus template functions, or generating the YAML outside Sleuth.

### Refusals — keep all, plus two
Keep: no mutable variables (`transform` already is the macro mechanism, so no `vars:` is needed), no `while`, no
`break`/`continue`, no I/O in scripts, no computed node generation. Add: no subflow path outside the flow's directory;
no condition language beyond Go template boolean functions.

## Build order and why
1. **Template functions + masking rule.** Small, independent, and closes a leak class before it grows.
2. **`finally`.** Small, uses the existing scheduler, delivers the biggest practical gap (stateful tests) without touching evidence shape.
3. **Evidence nesting** (`Step.Children`, path ids, all consumers, schema). A refactor with no feature; it ships alone.
4. **Subflows** on top of it, with static validation and depth/cycle limits.
5. **`if`** with the `not_run` status.
6. **`foreach` on flow nodes.**
7. Later, only on demand: `switch`, request-level `foreach`, list-valued env, scripts.

The proposal builds subflows first. They are the highest-value feature but depend on step 3, and steps 1–2 are cheap
and independent, so they go first.

## Method (same as the earlier milestones)
Each step lands with a test that fails before it; unit tests in `flow` and `runner`, plus scenarios added to the e2e suite
against `internal/mockservice` at `--parallel 1` and `4`. Each step gets a review pass before the next starts. Docs and
`schema/flow.schema.json` are updated with each, and the README gets a "when not to use this" section as the roadmap already lists.

## Update 2026-10-20: `finally` as built
Top-level `finally:` list per flow (also inside subflows). Cleanup nodes run serially after the main nodes under their own
grace context (`-cleanup-timeout`, default 30s) and are marked `cleanup` in evidence. Failed ones get error kind `cleanup`:
the run fails, the exit code of a main failure is unchanged, and a cleanup-only failure exits 1. A failed cleanup inside a
subflow fails the run but does not fail the calling node, so later nodes still run. Allowed kinds: request, transform, poll
on a request. SIGINT/SIGTERM stop the run like the timeout (cause is recorded, steps say "interrupted"), then run cleanup, exit 130; `{{ .run.id }}` (eight random digits, `-run-id` to pin) keeps data unique per run without a wrapper script. Not done: a node-level `cleanup:` next to the creating request (queued
automatically, run in reverse) is the likely next step for resource-style cleanup.
