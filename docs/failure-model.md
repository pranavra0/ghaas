# Failure model

GitHub Actions is an asynchronous execution service, not a transactional queue. A v0.1
workflow can be delayed, dropped, duplicated, rerun, cancelled, or terminated while its
command is running. ghaas does not promise exactly-once execution or exactly-once
external effects.

The safe application assumption is **at least one attempt, possibly more**. Treat a
command's side effects as ambiguous when the runner or network fails. Use
`GHAAS_INVOCATION_ID` as an idempotency key with the destination API when it supports
one; otherwise implement deduplication in the command or destination.

## Failure cases

### Schedule delivery

GitHub can start a cron workflow late or omit an occurrence. `schedule.timezone` is
validated and emitted to the workflow, but it is not a start-time SLA. ghaas has no
independent scheduler or catch-up loop. A later run is a new provider opportunity, not
proof that an earlier one ran.

### Duplicate or concurrent runs

A manual dispatch, scheduled delivery, operator rerun, or an API retry can create more
than one run. `concurrency.max: 1` asks GitHub to serialize ordinary runs for a function
and does not cancel an in-progress run. It is not a distributed lock and does not cover
every provider race or an external dispatch path.

If more than one runner reaches the command, both can produce an external effect. A
logical ID helps an application recognize duplicates, but ghaas v0.1 has no durable store
that can atomically claim an ID across fresh hosted runners.

### Cancellation, timeout, or runner loss

The runtime passes cancellation and its effective timeout to the child process. The
process may stop before completion, or it may continue briefly while the runner is being
terminated. A command can be accepted by an external service immediately before the
runner disappears. A later attempt or manual rerun can repeat that request.

### Process and installation errors

A missing executable, permission error, non-zero exit, signal, timeout, or cancellation
makes the invocation unsuccessful. The checkout, Go toolchain, module download, or
pinned installer can fail before the command starts. These failures are provider/workflow
failures, not evidence that an external command never ran.

### GitHub API uncertainty

`invoke` can time out after GitHub accepted a dispatch. Retrying blindly may create a
duplicate run. `status` and `logs` can be unavailable, rate-limited, or delayed. Inspect
GitHub using the logical ID and exact run title before retrying an operation whose outcome
is unknown.

When an explicit logical ID is supplied, matching uses the exact generated run title and
the workflow-run `DisplayTitle` field. ghaas does not infer a match from timestamps,
creation order, or the newest run. When `--ref` is omitted, dispatch targets the target
repository's default branch; an explicit ref is authoritative.

### Side effect succeeds before observation

The final process result or workflow result can be lost after an API accepted a request.
A red, cancelled, or missing workflow result therefore cannot prove that the destination
rolled back the request. Query the destination when possible, and use the invocation ID
in its idempotency or deduplication record.

## What v0.1 guarantees

| Property | v0.1 behavior |
| --- | --- |
| Command arguments | Preserved as argv; no implicit `sh -c` |
| Timeout/cancellation | Passed through the runtime context to the child |
| Workflow generation | Deterministic for the same manifest and options |
| Invocation metadata | Function, logical ID, trigger, attempt, and run ID are exported |
| Scheduling | Validated cron/timezone and GitHub trigger emission |
| Concurrency | GitHub per-function group when `max: 1` is configured |
| Cross-run deduplication | Not provided by the hosted-runner workflow |
| Durable invocation state | Not provided |
| Exactly-once execution/effects | Never promised |
| Transaction across command and API | Not provided |
| Start-time, catch-up, or success SLO | Not provided |

A non-zero process exit is observable in the workflow result and logs. It does not make
an external operation reversible. Logs remain subject to GitHub retention, permissions,
redaction behavior, and API availability; do not use their absence as proof of no effect.

## Roadmap, not current behavior

Durable invocation state, retry and backoff scheduling, leases, remote branch storage,
dead-letter or exhaustion handling, execution windows, and SLO declarations are future
features. v0.1 has no state transitions or retries. Adding a local file, branch-shaped
directory, or custom API around a command is an application decision and is not supplied
by the generated workflow.
