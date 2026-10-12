# 0002 — How contract drift is reported

**Status:** accepted (Phase 2, 2026-10-10)

## Decision
Registry-vs-live disagreement is a **first-class evidence entry and a failure by default**.

| Situation | Reported as | Default |
|---|---|---|
| Response body violates the documented schema | `contract` violation, `where: response` | fail |
| Response status not documented for the operation | `contract` violation, `where: response` | fail |
| Request/path not in the contract at all | `contract` violation, `where: operation` | fail (request not sent) |
| Request body violates the schema | `contract` violation, `where: request` | fail (request **not sent**) |
| Documented response has no/empty schema | `notes` entry | pass (nothing could be checked) |
| Contract can't be loaded / source undefined | setup error | exit 2 |

A node opts into recording without failing with `contract: {source: X, on_violation: warn}` — for known, accepted
drift and for deliberate negative-path tests (e.g. a 401 the provider does not document).

## Reasoning
- A warning nobody reads is not drift detection; the cost of a false failure is one `warn` line in the flow, and the
  flow file then documents the accepted gap.
- "Spec has no schema" is deliberately **not** a violation: that is a provider-quality issue, surfaced as a note so it
  is visible without making every flow red until the provider fixes it.
- Contract failures exit **1** (same class as assertion failures): the system under test is off-spec. Infrastructure
  (3) and flow-authoring (2) problems stay distinct.

## Verified behaviour (tests + live self-test)
Response drift, request drift (not sent), undocumented status, unknown operation, empty schema, warn mode,
and a deliberately drifted copy of a spec (unit tests in `internal/runner` and `internal/registry`).
