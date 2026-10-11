# 0003 — Contracts on poll nodes

**Status:** accepted

## Decision
`contract:` is allowed on a poll node that polls a request. Each attempt is checked in warn mode; the verdict is made
once the poll ends:

- the attempt that satisfied `until` is validated in full (request and response);
- an undocumented response status on any earlier attempt is kept as a finding;
- other findings on intermediate attempts are ignored, since "still pending" bodies legitimately differ from the final shape;
- operation-not-in-contract and request-body violations are the same on every attempt, so they stop the poll at once.

A violation is `error_kind: contract`, never `timeout` or `infrastructure`. `on_violation: warn` records instead of failing.

## Consequences
- A poll request that fails the contract is still sent once before the poll stops (a request node never sends it).
- `step.contract` holds the last attempt's result plus carried undocumented-status findings, not a per-attempt history.
- db polls have no contract.
