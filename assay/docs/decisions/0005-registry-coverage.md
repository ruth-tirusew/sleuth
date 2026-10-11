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
2. **Spec-vs-spec diff**: compare two versions of a published document and flag breaking changes (removed operations or
   statuses, newly required fields) before any flow runs.
3. **Active probing**: generate calls for operations no flow covers. Only safe for read-only operations with usable
   auth and fixtures, so it needs an explicit opt-in and an allow-list. Not started.
4. **Undocumented live endpoints**: finding what the service serves that the spec omits. There is no reliable way to
   enumerate that from outside, so it is out of scope unless the provider exposes a route list.

## Consequences
- Coverage is per run: a flow that fans out across environments or runs only part of a flow reports only what ran.
- A spec with many operations makes the report long; the CLI shows the first 15 untested and the evidence JSON lists all.
