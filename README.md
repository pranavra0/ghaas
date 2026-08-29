# ghaas

GitHub Actions as a Service: an extremely questionable serverless platform.

`ghaas` turns a small YAML manifest into deterministic GitHub Actions workflows. The
workflow is the execution substrate; the manifest describes command-backed functions.
This is a thin orchestration layer, not a durable queue, scheduler, sandbox, or
transactional cloud-functions platform.

## Quick start

Requirements:

- Go 1.23 or newer for the repository build and the default generated runtime
  installation step.
- A GitHub repository with Actions enabled and permission to commit the generated
  workflow.
- A GitHub API token in `GITHUB_TOKEN` for local API commands (see [GitHub setup](#github-setup)).

From a checkout of this repository:

```bash
go run ./cmd/ghaas init
# edit ghaas.yaml
go run ./cmd/ghaas validate
go run ./cmd/ghaas generate hello       # print generated YAML; does not write files
go run ./cmd/ghaas deploy               # write .github/workflows/ghaas-hello.yml
go run ./cmd/ghaas invoke hello         # dispatch workflow_dispatch on the default ref
go run ./cmd/ghaas status hello
go run ./cmd/ghaas logs hello
```

`deploy --check` is suitable for CI: it exits non-zero when a generated workflow is
missing, stale, or differs from the manifest.

```bash
go run ./cmd/ghaas deploy --check
```

The installed binary has the same interface:

```bash
go install ./cmd/ghaas
ghaas validate
```

## Manifest

A manifest is strict YAML. Unknown keys and multiple YAML documents are rejected.
`version: 1` is required, and at least one function is required.

```yaml
version: 1

defaults:
  timeout: 15m

functions:
  hello:
    runtime: command
    command: ["bash", "examples/hello/hello.sh"]
    timeout: 2m
    env:
      GREETING: "hello"
    secrets:
      - OPTIONAL_TOKEN
    concurrency:
      max: 1
    schedule:
      cron: "0 9 * * *"
      timezone: UTC
      execution_window:
        start: "09:00"
        end: "10:00"
    state:
      backend: branch
      branch: ghaas-state
    retry:
      max_attempts: 3
      backoff: 30s
    on_exhausted:
      issue: true
    slo:
      success_rate: 99.0
      schedule_delay: 15m
```

Supported manifest fields:

- `defaults.timeout`: positive Go duration inherited by functions without a timeout.
- `runtime`: currently only the scalar `command`.
- When neither a function nor `defaults.timeout` sets a timeout, the CLI/runtime use a
  15-minute default.
- `command`: non-empty argv array. Arguments retain their boundaries; they are not
  joined into a shell command.
- `timeout`: positive Go duration, rounded up to whole GitHub timeout minutes in the
  generated job.
- `env`: literal environment variables. Names must be valid environment identifiers.
- `secrets`: names of GitHub Actions secrets to expose to the command.
- `schedule.cron`: a validated five-field cron expression.
- `schedule.timezone`: a validated IANA timezone emitted on the GitHub schedule entry and
  used when deriving scheduled invocation IDs. Provider scheduling can still delay or
  drop runs; account for that when timing matters.
- `schedule.execution_window` (also accepted as `window` or the paired
  `window_start`/`window_end` fields): a validated wall-clock window. Scheduled runtime
  attempts outside the window are skipped; this is not a catch-up scheduler or SLA.
- `schedule.target` and `schedule.retry`: optional target-window scheduling metadata. A
  target such as `09:15 Friday` gets one weekly logical ID and opportunities are generated
  at the configured cadence.
- `concurrency.max`: only `1` (or omitted) is supported. The generated workflow uses
  a per-function concurrency group with `cancel-in-progress: false`.
- `state.backend`: `memory` or `branch`. The branch backend is a branch-shaped,
  file-backed store in the runner workspace; it does not commit or push to GitHub.
- `retry.max_attempts`: positive count including the initial attempt. `retry.backoff`
  is the delay between attempts.
- `on_exhausted.issue`: request a GitHub issue after retries exhaust. Issue creation
  is best effort and needs an Actions token with Issues write permission.
- `slo.success_rate` and `slo.schedule_delay`: validated declarations retained for
  integrations. They are not an availability guarantee or an automatic scheduler, and
  the current runtime does not enforce either target.

## Commands

| Command | Behavior |
| --- | --- |
| `init [--force]` | Create `ghaas.yaml` from the starter manifest. Refuses to overwrite unless `--force` is supplied. |
| `validate` | Parse and validate locally; no GitHub request is made. |
| `generate [FUNCTION]` | Print deterministic workflow YAML for one function or all functions. |
| `deploy [--check] [--function NAME]` | Write generated `.github/workflows/ghaas-<function>.yml`, or check committed output without writing. |
| `invoke [--ref REF] FUNCTION` | Create a pending logical invocation and dispatch the workflow with a generated UUID. |
| `status FUNCTION` | Show the newest logical state record when a state store is available, reconciling it with matching provider workflow runs; otherwise show the newest workflow run. |
| `logs FUNCTION [--invocation ID]` | Read logs for a logical invocation, numeric workflow run ID, or the newest run. A logical ID must resolve to its recorded/matching workflow run. |
| `version` | Print the CLI version. |
| `runtime invoke FUNCTION` | Internal workflow entrypoint: load and validate the manifest, set invocation environment, acquire state/lease, execute argv, and apply retry policy. |

Generated workflows contain `workflow_dispatch` and an optional schedule, checkout,
permissions, concurrency, timeout, an install step, and `ghaas runtime invoke`.
Generated files begin with a `Code generated by ghaas` marker; edit the manifest rather
than generated YAML.

The generated `permissions` block grants `contents: read` for checkout by default,
`contents: write` when the function selects the branch state backend, and `issues: write`
when `on_exhausted.issue` is enabled. The branch backend is currently local, so review
and reduce `contents: write` if no remote write integration is in use.

### Generated-runtime installation assumptions

The default generated install step is:

```bash
go install ./cmd/ghaas && echo "$(go env GOPATH)/bin" >> "$GITHUB_PATH"
```

The workflow therefore assumes all of the following:

1. The checked-out repository contains `cmd/ghaas` and its Go module files (or the
   installer has been overridden to a reviewed release).
2. The runner has the Go toolchain required by that module (Go 1.23 or newer).
3. The runner supports the GitHub Actions `GITHUB_PATH` mechanism used to expose the
   installed binary to the following `ghaas runtime invoke` step.
4. Dependencies can be downloaded during the workflow.

The installer command is trusted compiler configuration, not a function command. Use a
reviewed release or another explicit installer through compiler options when the target
repository does not contain ghaas source. A generated workflow cannot run the runtime
until its install step has made `ghaas` available.

## GitHub setup

The local CLI discovers the repository from `GITHUB_REPOSITORY` (`OWNER/NAME`) or a
GitHub `origin` remote. Set `GITHUB_TOKEN` before `invoke`, `status`, or `logs`:

```bash
export GITHUB_REPOSITORY=OWNER/REPOSITORY   # optional inside a Git checkout
export GITHUB_TOKEN=...                     # do not commit or print this value
```

For a fine-grained personal access token, grant the target repository:

- **Actions: write** for `invoke` (GitHub may also require Actions read access).
- **Actions: read** for `status` and `logs`.
- **Issues: write** if an exhausted invocation should create an issue.

The token also needs ordinary repository visibility. A classic token generally needs
`repo` for a private repository (and corresponding Actions access). Organization policy,
SSO, repository Actions policy, and fine-grained-token approval can still deny a request.
`validate` and `generate` do not need a token; `deploy` only writes local files.

The generated workflow receives a separate, run-scoped GitHub-provided `GITHUB_TOKEN`.
It is not the local developer token. The compiler wires `${{ secrets.GITHUB_TOKEN }}`
into the invocation environment when the manifest does not define `GITHUB_TOKEN`. The
workflow token remains constrained by the generated `permissions` block and is used for
checkout, optional issue creation, and any configured remote integration.

Repository secrets are configured in GitHub, not in `ghaas.yaml`:

```bash
gh secret set LASTFM_API_KEY
gh secret set DISCORD_WEBHOOK_URL
```

A manifest secret entry only wires `${{ secrets.NAME }}` into the invocation environment.
It cannot prove that the secret exists during local validation.

## Runtime environment

Every generated invocation receives:

- `GHAAS_FUNCTION`
- `GHAAS_INVOCATION_ID`
- `GHAAS_ATTEMPT`
- `GHAAS_TRIGGER` (`manual` or `schedule`)
- `GHAAS_WORKFLOW_RUN_ID`

Manual dispatch gets a UUID input. Scheduled runs derive a deterministic key using the
configured timezone (for example, a date or hour key according to the cron shape). Use
`GHAAS_INVOCATION_ID` as an idempotency key when an external API supports one; it is not
proof that arbitrary side effects happen exactly once.

Commands inherit stdout and stderr. A non-zero exit records a failed attempt. With a
positive retry policy, the runtime returns the record to `pending`, waits for the
configured backoff, and tries again until it succeeds or reaches `max_attempts`, then
marks it `exhausted`. Without a retry policy, a failed invocation remains `failed`.
Context cancellation and the configured timeout stop execution; the completion state is
written with a cancellation-independent context where possible.

Commands execute as argv through the OS process API: `command: ["sh", "-c", ...]` is an
explicit request for shell behavior and should be avoided for untrusted values.

## State, leases, and limitations

The logical invocation lifecycle is:

```text
pending -> running -> succeeded
                 \-> failed -> pending   (retry selected)
                              \-> exhausted
```

The runtime uses compare-and-swap state transitions. A configured lease prevents another
runner from claiming a running invocation until the lease expires; a later runner may
recover an expired lease. Lease recovery prevents some concurrent execution, but cannot
undo an external effect made before a crash. The lease owner and expiry are metadata, not
a transaction.

`state.backend: memory` is process-local. `state.backend: branch` currently stores JSON
under a branch-shaped directory in the checked-out workspace (the default CLI location
is `.ghaas-state`); it uses optimistic CAS and atomic file updates, but does not push or
commit that directory to a GitHub branch. Consequently a fresh hosted runner normally
starts without prior state unless an embedding application supplies a durable Store.
The Store interface is an extension seam, not evidence that the stock workflow has a
remote durable state backend.

Other important boundaries:

- GitHub Actions can delay, drop, duplicate, cancel, or rerun scheduled workflows.
- A schedule timezone is validated, emitted as `on.schedule[].timezone`, and used for ID
  derivation when the provider applies that field. GitHub may still delay or drop runs;
  convert local times to UTC when targeting providers without timezone support or when
  timing is strict, including daylight-saving changes.
- Execution windows gate scheduled attempts, but do not provide a catch-up scheduler or
  start-time SLA. SLO values are declarations for integrations, not guarantees.
- State recording can fail after an external API accepted a request. A retry or lease
  recovery can repeat that effect; use destination idempotency keys where available.
- `status` and `logs` cannot infer business completion from a workflow run alone. Logs
  remain in GitHub and are subject to retention, permissions, and API limits.
- GitHub is the runner, availability, timeout, network, and rate-limit boundary. ghaas
  does not provide a local daemon, queue, sandbox, rollback, or exactly-once effects.

Read the detailed [semantics](docs/semantics.md), [state model](docs/state.md),
[failure model](docs/failure-model.md), and [security model](docs/security.md) before
using it for production side effects.

## Examples

The examples are runnable from the repository root after copying one manifest:

```bash
cp examples/hello/ghaas.yaml ghaas.yaml
go run ./cmd/ghaas validate
go run ./cmd/ghaas deploy --function hello
```

`examples/weekly-lastfm` demonstrates a scheduled Last.fm-to-Discord command. It
requires `LASTFM_USERNAME`, `LASTFM_API_KEY`, and `DISCORD_WEBHOOK_URL`; follow its
inline comments and the setup commands in the directory manifest/script. Run it with a
dry local command only after supplying real credentials, and treat external calls as
non-idempotent unless the receiving service provides a suitable key.
