# ghaas semantics

This document describes what a function and an invocation mean independently of
GitHub's workflow-run vocabulary. ghaas gives workflow runs a logical identity and a
state machine, but it does not turn GitHub Actions into a durable queue or a
transactional scheduler.

## Functions and workflow runs

A function is a named command in `ghaas.yaml`. `ghaas generate` compiles it to
`.github/workflows/ghaas-<function>.yml`. A workflow run is an infrastructure event;
it is not automatically a new semantic invocation. Schedules, dispatch retries, and
operator re-runs can produce multiple workflow runs for one logical ID, and a run can
fail before the command starts.

The logical invocation is one expected execution of one function. A scheduled
invocation may therefore look like `weekly-lastfm/2026-08-29`. Multiple runs can
inspect or attempt the same ID. A shared state store can let the first successful
attempt suppress later terminal attempts; the stock hosted-runner workflow does not
provide a remote state store by itself.

## Invocation IDs

IDs have the path-safe shape `function/key`:

- Manual invocation: `function/<canonical UUID>`.
- Scheduled invocation: `function/<schedule key>`, such as
  `weekly-lastfm/2026-W35`, `daily-backup/2026-08-29`, or
  `hourly-sync/2026-08-29T14`.

Function and key components must be non-empty and cannot contain `/`, `\\`, `.`, `..`,
or control characters. Manual UUIDs use the canonical 8-4-4-4-12 hexadecimal form and
are normalized to lowercase. IDs are unique within a function, not globally.

The public `invoke` command creates a pending record when a state store is configured,
generates a UUID, and passes it as the `ghaas_invocation_id` workflow-dispatch input.
The runtime accepts that value from `GHAAS_INVOCATION_ID` after validating that it
belongs to the selected function. A scheduled run without an input derives a stable key
from the current time and cron shape in the configured timezone.

## Lifecycle, attempts, and retry

The lifecycle statuses are:

```text
pending -> running -> succeeded
                 \-> failed -> pending   (when retry is selected)
                              \-> exhausted
```

`pending` means the invocation exists but no runner has claimed its current attempt.
`running` means a runner has claimed it and is executing. `succeeded` and `exhausted`
are terminal. `failed` records a completed unsuccessful attempt. A positive
`retry.max_attempts` includes the initial attempt: after a failure, the runtime waits
for `retry.backoff`, returns the record to `pending`, and tries again while
`attempts < max_attempts`. Once the count is reached it transitions to `exhausted`.
Without a configured retry policy, a failed invocation remains `failed`.

A zero exit code with no execution error produces `succeeded`. A non-zero exit or
execution error produces `failed` and records the exit code/error text. A terminal record
is returned unchanged and is not run again. An already-running record with an active
lease is busy rather than silently executed concurrently. State transitions are checked
against the lifecycle graph; pending cannot jump directly to success and terminal records
cannot be moved back to running.

## Attempts, leases, and triggers

An attempt is a command execution claim, not a workflow-run count. The runtime increments
`attempts` when it changes `pending` to `running`. When a lease duration is configured
(the generated runtime uses the function timeout), it records an owner and expiry. A
runner cannot claim an active lease. A later runner may recover a running record after
expiry, acquire a new lease, and execute another attempt. Recovery is compare-and-swap
protected and does not prove that the prior process had no external effect.

Every command receives:

```text
GHAAS_FUNCTION
GHAAS_INVOCATION_ID
GHAAS_ATTEMPT
GHAAS_TRIGGER
GHAAS_WORKFLOW_RUN_ID
```

`GHAAS_TRIGGER` is `manual` for workflow dispatch and `schedule` for scheduled runs.
`GHAAS_WORKFLOW_RUN_ID` is empty when GitHub does not provide a run ID (for example, a
local invocation). Applications may use the invocation ID as an idempotency key and the
workflow ID as a tracing field; neither is proof of exactly-once effects.

## Scheduling and timezone

The manifest accepts standard five-field cron and validates IANA timezone names. The
compiler emits a GitHub `schedule` trigger and always emits `workflow_dispatch` as well.
It preserves the timezone as a `timezone` field in the generated schedule and also
passes it as runtime metadata for ID derivation.

The timezone field is passed to the provider; it does not eliminate delayed or dropped
scheduled runs. When targeting a provider that ignores timezone, convert desired local
times to UTC and account for daylight-saving changes.

Execution windows (`execution_window`/`window`) gate scheduled runtime attempts outside
the configured local wall-clock interval. Target schedules also check the requested
weekday. This is not a catch-up scheduler and does not guarantee a start-time SLO.

## Concurrency

`concurrency.max: 1` maps to a GitHub Actions concurrency group named for the function,
with `cancel-in-progress: false`. This reduces overlap between ordinary workflow runs,
but it is not a distributed lock and does not establish exactly-once behavior. State CAS
and, for long work, an expiring lease coordinate runners that escape or outlive Actions'
concurrency mechanism.

## Command execution and generated workflow

`runtime: command` executes the first `command` element as the program and passes the
remaining elements as separate arguments. stdout and stderr are inherited, and context
cancellation/timeout terminates the child. No implicit shell parsing, quoting, globbing,
or interpolation occurs. If shell syntax is desired, make it explicit with an argv such
as `["sh", "-c", "..."]`; never place untrusted manifest values into that shell string.

Generated YAML is deterministic for the same manifest, compiler version, and options.
The workflow is deliberately thin: checkout, install the `ghaas` executable, then invoke
`ghaas runtime invoke <function>`. The default install step is
`go install ./cmd/ghaas && echo "$(go env GOPATH)/bin" >> "$GITHUB_PATH"`.
The checked-out repository must contain the ghaas source and Go module (or use a reviewed
installer override), the runner must have Go 1.23+, dependencies must be fetchable, and
the runner must support `GITHUB_PATH`. Function behavior belongs in the command or
checked-in application code.

## State and current boundary

The state package exposes memory and branch-shaped file stores with `Get`, `Create`,
compare-and-swap, and `List`. `memory` is process-local. The CLI's default branch backend
writes JSON below `.ghaas-state/<branch>/functions/<function>/` in the checked-out
workspace, using a branch namespace and optimistic CAS; it does not commit or push those
files to a GitHub branch. An embedding application may provide a truly durable Store, but
the stock workflow should be treated as at-least-once across fresh hosted runners.

`on_exhausted.issue` asks the runtime to create a GitHub issue after exhaustion. It needs
the run-scoped `GITHUB_TOKEN` with Issues write permission, and issue creation is best
effort; failure to create the issue does not undo the exhausted state.

SLO fields are validated declarations for reporting/integration. They do not enforce a
success-rate target or schedule-delay SLA. See [state](state.md), [failure model](failure-model.md),
and [security](security.md) for operational details.
