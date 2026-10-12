# 0003 — Contracts on poll nodes

**Status:** accepted

## Decision
`contract:` is allowed on a poll node that polls a request. Each attempt is checked in warn mode; the verdict is made
once the poll ends:

- the attempt that satisfied `until` is validated in full (request and response);
- an undocumented response status on any earlier attempt is kept as a finding;
- other findings on intermediate attempts are ignored, since "still pending" bodies legitimately differ from the final shape;
- operation-not-in-contract and request-body violations are the same on every attempt, so, as in a request node, they
  stop the poll before the request is sent (one attempt, nothing sent).

A violation is `error_kind: contract`, never `timeout` or `infrastructure`. `on_violation: warn` records instead of failing.

## Consequences
- `step.contract` holds the last attempt's result plus carried undocumented-status findings, not a per-attempt history.
- db polls have no contract.

## Amendment (2026-10-11): persistent findings survive a poll that never converges
When a contract poll gives up, response findings that appear on **every** answered attempt are kept as drift: a body that
is wrong in the same way each time is not an intermediate shape. Findings that differ between attempts are still dropped.
This needs at least two answered attempts; with a single answer a "still pending" shape cannot be told from drift, so
none of its response findings are kept. Undocumented statuses seen on any attempt are always kept.
