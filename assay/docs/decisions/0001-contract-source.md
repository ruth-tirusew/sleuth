# 0001 — Contract source: provider-published OpenAPI

**Status:** accepted for the pilot (Phase 2, 2026-10-10)

## Decision
Node contracts come from the **provider's published OpenAPI document**, not from consumer-driven contracts (e.g. Pact).
Typical source: a service's served `/openapi.json` (for example FastAPI-generated, OpenAPI 3.1).

## Reasoning
- **It already exists.** Every FastAPI service serves its spec; consumer-driven contracts need a consumer-side test
  suite and a broker before they produce anything. Assay has neither.
- **Assay is a flow tester, not a consumer.** It exercises many providers in sequence; the question it answers is
  "does the live service still match what it publishes?" — exactly what a provider spec states.
- **Drift is the point.** Phase 2/4 want registry-vs-live disagreement as a first-class finding. With a provider spec
  that disagreement is a clean diff; with consumer contracts it is "consumer X's expectations", which is narrower.

## Consequences / risks
- Contract quality = spec quality. A spec that documents a 200 response as `{}` (no response model) cannot be
  validated at all, so Assay reports undocumented operations/statuses and empty schemas as findings rather than
  silently passing.
- Provider specs say what the provider *offers*, not what a consumer *needs*. A flow's own assertions still carry
  consumer-specific expectations.
- Revisit consumer-driven contracts if multiple consumers start depending on fields the provider spec marks optional.

## Drift policy (Phase 2, week 4 — provisional)
A contract violation fails the step by default (`error_kind: contract`). A node can opt into `on_violation: warn`
to record violations as evidence without failing.
