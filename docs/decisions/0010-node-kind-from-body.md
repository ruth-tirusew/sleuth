# 0010 — A node's kind is its body key

**Status:** built 2026-11-23. Replaces the `kind:` field.

## Decision
Nodes no longer carry `kind:`. A node has exactly one of `request`, `poll`, `db`, `transform` or `flow` (`params`
without `flow` counts as a flow node, so the error names the missing path), and that key is its kind. A node with none
of them and an `assert:` list is an assertion node. `assert` stays a modifier on request, poll and db nodes.

    - id: login
      request: {method: POST, url: "{{ .env.base_url }}/login"}
      assert: [{path: nodes.login.response.status, equals: 200}]

    - id: login_ok
      assert: [{path: nodes.login.response.status, equals: 200}]

## Why
- **`kind: db` followed by `db:` said the same thing twice**, and the two could disagree. A `kind: request` node with a
  `db:` block loaded fine and the block was silently ignored; copying a node and changing its body but not its kind
  produced exactly that.
- **The body key is what a reader looks at anyway.** It is how GitHub Actions tells `uses:` from `run:` and how Ansible
  picks a module.

## Rules and errors (all at load)
- Two body keys: `node x has both request and db; a node does one thing`.
- No body and no assert: `node x has nothing to do: ...`.
- `kind:` still written: ``node x: remove `kind: request`; a node's kind is its body key ...``. The key is parsed only to
  give that message; it is not accepted.
- `Kind` stays on the Go struct and in evidence and dashboard JSON: `Validate` sets it, so the runner, reports and
  cleanup rules did not change. A node built in Go may set `Kind` itself; if it also has a body, they must agree.
- `schema/flow.schema.json` expresses the same rule as a `oneOf`, so editors flag two bodies or none.

## Rejected
- **Flatten the body into the node** (`kind: db` + `query:` at the top level). Removes the nesting, but fields
  collide (`poll` holds its own `request` or `db`; `assert`, `extract` and
  `contract` would need per-kind rules).
- **Kind as the list key** (`- db: {id: q, query: ...}`). No redundancy, but `id` and `needs` move inside the body,
  which hides the node's identity and dependencies.
- **Keep `kind:` as optional.** Two ways to write the same node, and the checker for "do they agree" is the thing we were
  removing. Every flow in the repo was migrated in the
  same change instead.
