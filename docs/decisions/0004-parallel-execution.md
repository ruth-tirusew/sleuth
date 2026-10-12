# 0004 — Parallel execution of independent nodes

**Status:** accepted

## Decision
`sleuth run --parallel N` runs up to N nodes at once; the default is 1.

- **Scheduling.** A node is ready when all its dependencies (`needs:` and `nodes.<id>` references) have finished. It runs
  if they all passed and is skipped otherwise, with the same message as in serial runs. Among ready nodes the earliest
  in flow order starts first, so N=1 is exactly the previous serial order. There is no fail-fast: a failure skips its
  dependents only.
- **Evidence.** Steps are reported in flow order regardless of completion order. `run.parallel` records N when N > 1.
- **Shared state.** `Context` is guarded by a read/write mutex (node outputs are immutable once published). Postgres
  uses a `pgxpool` per named database with `MaxConns = N`, because a `pgx.Conn` cannot be used concurrently; a poll's
  attempts still reuse pooled connections. The Mongo and HTTP clients were already safe to share.
- **Polls** need no special handling: anything that reads a poll's result references `nodes.<id>` and therefore already
  waits for the poll to finish.

## Why opt-in
Today file order is execution order, so a flow can rely on it without declaring `needs:`. Parallel runs would make
that a race. The default stays serial, and `sleuth validate --parallel N` prints the waves of nodes that would overlap so
missing `needs:` can be found before a run.

## Consequences
- With N > 1, ordering that is not expressed as `needs:` or a `nodes.<id>` reference is not guaranteed.
- A flow-level `max_parallel:` (to pin a flow to serial) was left out until someone needs it.
- Wave grouping is by dependency depth. It shows independence, not every pair that could overlap.
