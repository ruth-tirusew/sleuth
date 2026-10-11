# 0007 — Behavioural snapshots (strengthening baselines)

**Status:** accepted; slices 1 (behavioural snapshots) and 2 (provenance, replace-with-diff, backup, merge) are implemented; auto-baseline is proposed

## Problem
A baseline (decision 0005) is a saved copy of a provider's OpenAPI document. For a project that has been under test for a
while that is a weak guard:

1. **It only sees what the spec says.** A response the spec leaves undocumented (an empty schema) can change shape and the
   spec diff will never know. That is where "someone changed the DTO" goes unnoticed.
2. **It keeps no history.** `snapshot -force` overwrites it, so what the contract used to be is lost.
3. **It has no provenance.** A bare document does not say when it was taken, from where, or by what.
4. **Replacing it is silent.** Updating a baseline means accepting the drift since it was taken, but nothing shows what is being accepted.

## Decision
Add a second kind of snapshot that records **how the system actually behaved**: the *shape* of the response of each step of
a flow, taken from a known-good run, and compare later runs against it. It sits beside the spec baseline and does not replace it.

### What is recorded: shapes, never values
A shape is the structure of a JSON value: its types, an object's fields (each marked optional if it was missing in some
observation), an array's item shape. **No values are stored**, so a snapshot holds no customer data and is safe to commit.
Integers and decimals are told apart (`integer` / `number`), `null` is a type, and a field that can be several types
records the set.

### What a snapshot is keyed by: the flow's step ids
A response is attributed to the **step** that received it (`profile`, `wait_done`), together with its HTTP status. Without a
contract there is no reliable way to name an operation (`/users/42` and `/users/7` are the same operation), and a flow is
what Sleuth runs, so the step id is the stable key. When the step is bound to a contract its operation is stored as
information. Database steps record the shape of their rows. A snapshot belongs to one flow; it stores the flow's name and a
hash of the flow file, and a changed flow is reported as a note, not an error.

### How shapes are compared: from the client's side, as `diff` does for specs
The question is what would break a client written against the snapshot, so only what a response can *do to a reader* counts:

| Observed now, against the snapshot | Severity |
|---|---|
| a field the snapshot always had is missing | breaking |
| a field the snapshot always had is now sometimes missing (inside an array) | breaking |
| a field's type changed, or a type was added to it (including `null`, or a decimal where there were only integers) | breaking |
| the HTTP status is different | breaking (the body is then not compared; it is a different response) |
| an object field was added | compatible (`info`) |
| a type seen in the snapshot did not appear this time | not reported: it is variety in the data, not drift |
| a step in the snapshot did not run, or a step has no entry | `info` |

`-shape-drift-fail breaking` fails the run (exit 1, the same class as a contract failure) on any breaking change;
`-shape-drift-fail any` also fails on compatible ones, for a team that wants every change reviewed.

### Recording
`sleuth run -record-shapes FILE flow.yaml` runs the flow and writes the snapshot. Rules:

- **Only from a passing run.** A snapshot taken from a failing run would bless the failure; `-force` overrides.
- **An existing snapshot is never replaced silently.** The run prints what changed between the old snapshot and this
  run and exits 2 unless `-force` is given. With `-force` the old file is kept as `FILE.prev` and the changes accepted are printed.
  Accepting drift is a decision; the tool shows what is being decided.
- **`-merge` widens instead of replacing.** One run cannot tell an optional field from a required one, so several
  known-good runs (or runs against several environments) can be merged: types are unioned, a field missing in any
  observation becomes optional, and the run count is recorded.

### Provenance
Each file records the format version, when it was recorded, the tool version, the environment name, the flow name and hash,
the number of runs merged, and per step the status, the operation (if any) and the number of observations. A run that
compares against it prints how old the snapshot is. History beyond one backup is git's job: snapshots are meant to be committed.

### Comparing
`sleuth run -shapes FILE flow.yaml` compares and reports a **Behaviour drift** section (evidence: `drift.behaviour`):
every change with its step and path, whether it is failing, and when the snapshot was recorded. It needs no contract. The
spec baseline and the behavioural snapshot are independent: a change can show in one, the other, or both, and the report keeps them apart.

## Consequences and limits
- It sees only what the flow exercises: a response field the flow's request never provokes is not in the snapshot.
- Shapes do not capture value changes (a status code string that changes meaning, units, an enum gaining a value). Assertions
  and contracts cover those.
- Variety inside arrays is learned from the items of one response; variety across runs needs `-merge`.
- A snapshot is only as good as the run it came from. The "passing run only" rule and the printed diff on replacement are the guards.

## Not in this slice
- **Auto-baseline** (record a snapshot the first time a flow is seen). Proposed: it needs a policy for where the file goes and for CI, where a silent first snapshot would hide drift rather than catch it.
- **Per-environment snapshots** beyond naming the environment in the provenance (use one file per environment today).
- **Recording from an existing evidence file**, and shapes for non-JSON bodies.
