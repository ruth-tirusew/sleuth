# 0005 — Registry coverage (Phase 4, first slice)

**Status:** accepted for the first slice; later slices proposed, not started

## Decision
Phase 2 reports drift for the calls a flow makes. Phase 4 widens that to the whole registry: every run that uses a
contract also reports, per source, which published operations it called and which it did not
(`drift.sources[]`: `total`, `exercised`, `untested`).

- "Exercised" means a step called the operation, whether or not the call passed. Skipped steps do not count.
- It is informational, never a failure. A flow is not obliged to cover a whole spec, and failing on it would make every
  small flow red.
- It is offline and safe: nothing is sent that the flow would not already send.

## Not in this slice (proposed)
1. **A coverage threshold** (`--min-coverage`, or per source) so a team can choose to gate on it.
2. ~~**Spec-vs-spec diff**~~ — done: `sleuth diff OLD NEW`, see the next section.
3. **Active probing**: generate calls for operations no flow covers. Only safe for read-only operations with usable
   auth and fixtures, so it needs an explicit opt-in and an allow-list. Not started.
4. **Undocumented live endpoints**: finding what the service serves that the spec omits. There is no reliable way to
   enumerate that from outside, so it is out of scope unless the provider exposes a route list.

## Consequences
- Coverage is per run: a flow that fans out across environments or runs only part of a flow reports only what ran.
- A spec with many operations makes the report long; the CLI shows the first 15 untested and the evidence JSON lists all.

## Spec-vs-spec diff (second slice, done)
`sleuth diff OLD NEW` compares two versions of an OpenAPI document offline. It is judged from the point of view of a client
written against OLD, so the direction of a schema matters: for a **request** (what the client sends) tightening breaks it;
for a **response** (what the client receives) loosening breaks it. Each change is `breaking`, `review` (cannot be
classified mechanically: composition keywords, `pattern`/`format`, security requirements) or `info` (compatible).

- Exit 1 on breaking changes by default; `-fail-on review|never` changes the threshold; load errors exit 2.
- Local `$ref`s are followed; recursive schemas are cut off rather than flooding the report. External `$ref`s are not.
- Judgement calls, deliberately conservative: a new enum value in a **response** is breaking (the client may not handle
  it); removing a documented non-2xx response is only `info`; a property dropped from a request is `info` unless the new
  schema also forbids extra properties.
- It reads the document only. It cannot tell that a service started behaving differently while the spec stayed the same;
  that is what running a flow against the live service is for.

## Baselines: the diff inside a run (third slice, done)
A contract source can name a saved copy of its document under `baselines:` in the env file. Every run that uses the source
diffs the baseline against the live document and records it as `drift.spec_drift` (counts, the changes, and for each change
whether a step of the run called that operation). `sleuth snapshot` writes the baseline.

- **Report-only by default.** A spec that moved is a finding about the provider, not about the step, so it does not fail
  a run unless asked: `run -spec-drift-fail breaking|review`. Then the run is `failed` and exits 1 (the same class as a
  contract failure), because the spec moved under the flow.
- **The threshold applies to the whole document, not only the operations the run called.** That matches `sleuth diff
  -fail-on` and keeps the rule predictable; the report separates the called operations so the relevant ones stand out.
- **A baseline that cannot be loaded is a setup error (exit 2).** Silently skipping it would make "no drift" mean "not checked".
- **`snapshot` will not overwrite** an existing baseline without `-force`, since a replaced baseline erases the drift it
  was there to catch.
