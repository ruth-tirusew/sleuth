# 0009 — Subflows

**Status:** built 2026-10-12. First step of [0008](0008-composition-and-control-flow.md).

## Decision
A node of `kind: flow` runs another flow file: `flow:` (a path), `params:` in, `outputs:` out. The caller reads
`nodes.<id>.out.<name>` and nothing else about the inside.

## Why this shape
- **Composition, not a language.** Reuse comes from calling a flow, so no includes, templates or functions were added.
  The node depends on whatever its `params` templates reference, so the existing scheduler orders it with no new rules.
- **Hidden internals.** The subflow runs in a fresh context (env and secrets shared; its own nodes and `params`). The
  caller cannot reference an inner node: that is a load error ("unknown node"), so a subflow can be rewritten freely.
- **Everything checkable is checked at load.** The loader resolves subflows recursively and rejects unreadable files,
  self-inclusion (with the chain shown), nesting over 8, unknown or missing params, and outputs that read nodes the subflow
  does not have. `validate` therefore covers them. Not checked statically (reported at run time with the offending
  template): a `.params.x` that is not declared, and `.nodes.<sub>.out.<name>` naming an output the subflow lacks.
- **Containment.** A subflow path must be relative and stay inside the top-level flow's directory after symlinks are
  resolved, so a flow file cannot pull in arbitrary files and the dashboard's "the client never names a path" rule holds.
  The path is plain text, because nodes are written, not computed.
- **Serial inside a slot.** A subflow runs its nodes one at a time within the calling node's worker slot. Letting it
  take slots from the shared pool would deadlock at `--parallel 1` (the parent holds the only slot while waiting for its
  child). Cost: no concurrency inside a subflow. Revisit when a real flow needs it.
- **Failure.** The calling node fails with the most severe kind among its failed inner steps (flow problem, then
  assertion/contract/timeout, then infrastructure: the order the exit code uses), naming the first failed step. A failure
  of the subflow's own `outputs` is a flow problem.

## Evidence
`Step` gained `flow`, `params` and `children` (inner steps keep local ids). Consumers that look at one step at a time
read `report.Flatten` (every step, path ids such as `auth/login`) or `report.Leaves` (steps that carry their own result, so
one failure is counted once in JUnit and in the step count). Drift, coverage, behavioural snapshots and exit codes use
Flatten. A snapshot's fingerprint of "the flow" now covers the subflow files too.

## Found while building it
The end-to-end suite caught what the unit tests had missed: running `sleuth run flow.yaml` with a relative path made
every sibling subflow look "outside the directory" (the path was resolved relative, then compared with an absolute
root). Fixed by resolving to an absolute path first, with a unit test that runs from the flow's directory.

## Not included (see 0008 for the order)
`finally`, `if:`, `foreach`, template functions, a `-param` flag to run a flow that has required params, and subflow
events in the dashboard timeline (the detail panel shows the nested steps; the timeline shows the calling node).
