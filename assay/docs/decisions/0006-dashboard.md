# 0006 — Local dashboard (experiment)

**Status:** experiment; kept small on purpose

## Decision
`assay serve` runs a local web page that lists flows, runs one with a chosen `--parallel`, and shows it live. It is the
same runner as the CLI: the runner gained an `Options.OnEvent` hook (run_start with the plan, step_start, step_done), and
the server streams those events to the browser over Server-Sent Events. Nothing about how a flow runs changed.

## Why this shape
- **No new dependencies, one embedded file.** Go's `net/http` and `go:embed`; the page is plain HTML/JS with no build step
  and no external resources, so it works offline and a CSP can forbid everything else.
- **Events, not polling.** The scheduler already knows when a step starts and finishes, so progress is exact, and the
  timeline can show real overlap from `--parallel`.
- **Evidence stays the source of truth.** Each run still writes the same masked JSON the CLI writes; the page is a view.

## Security (it executes flows with the process's secrets)
- Listens on loopback only; any other `-addr` is refused.
- Every API call needs a random per-start token (header, or query for the event stream), compared in constant time. It is
  printed in the URL fragment and removed from the address bar once the page has read it.
- `Host` must be a loopback name, which defeats DNS rebinding.
- The client sends a flow *file name* that must appear in the directory listing; it never supplies a path, and the env file is
  never named by the client (paired by name from the listing, else the startup default). Env files and dotfiles are not offered as flows.
- Everything sent to the browser passes through the same secret masking as evidence files (`report.MaskJSON`).
- One run at a time (409 otherwise), so a stray double click cannot start parallel runs of a flow against a real system.

## Which environment a flow uses
`foo.flow.yaml` (or `foo.yaml`) pairs with `foo.env.yaml` in the same directory; a flow with no pair uses the server's `-env`
default (or no env at all). A convention, not a flow field, so flow files stay environment-neutral and nothing in a request
can name a file. The listing reports the env each flow will use and, up front, whether it can run: the process environment is
fixed when `assay serve` starts, so an unset `${VAR}` or secret is a final answer, and the page disables Run with the reason.
Startup only rejects a `-env` that is not a readable file.

## Not done
- No run history beyond the evidence directory, no authentication beyond the token, and no editing of flows.
- Secrets that the target echoes back are masked, but values derived from them (a hash, a token the service mints) are not;
  this matches the CLI's evidence masking.
- Visual changes were checked in headless Chromium (dark, light, 390px wide); there is no automated browser test in the repo.
