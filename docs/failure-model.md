# v1 failure model

GitHub Actions is an asynchronous execution service, not a transactional queue.
A workflow can be delayed, dropped, duplicated, rerun, cancelled, or terminated
while its command runs. Durable state records what the control and execution
planes observed; it cannot make command side effects exactly once.

## Dispatch uncertainty

`invoke` generates `<function>/<UUID>`, creates its durable `pending` record, and
only then calls `workflow_dispatch`. If the API call times out or returns an
ambiguous error, the record remains pending. Retrying the command creates another
logical invocation and is an explicit operator choice; ghaas never silently
replaces the unknown record or claims that no run exists.

## Execution transitions

The runtime acquires a fenced lease before running the command. Every write
checks owner, opaque token, fence, and expiry. A worker that loses its lease
cannot mark work complete. A failed command becomes `pending` when attempts
remain, after the configured `retry.backoff`; the final failure becomes
`exhausted`. Successful completion becomes `succeeded`. With the default
`retry.max_attempts: 1`, a failure is exhausted without a second attempt.

A retry keeps the original logical ID and increments its logical `attempts`; it is
not a new invocation. Provider reruns are separately represented by the exact
`run_id` and `run_attempt` binding. Same-workflow retries do not self-dispatch and
do not perform an Actions write.

## Schedule delivery

A scheduled invocation ID is `<function>/<positive-GITHUB_RUN_ID>`. The workflow
run ID is the native schedule identity. There is no intended timestamp, catch-up
scheduler, timestamp-key dedupe, or inference from a missing state record.
Schedule and timezone settings control provider delivery metadata, not a
start-time SLO.

## Status and logs

Status reads durable state first. It reports `pending`, `running`, `succeeded`,
`failed`, or `exhausted`, attempts, and exact provider references. `status --json`
is a stable object for scripts. Logs use the exact state-bound provider run and
attempt when available; they do not select a different run merely because a
lookup is delayed. GitHub log retention, permissions, redirects, and API rate
limits remain provider concerns.

## Guarantees and non-goals

| Property | v1 behavior |
| --- | --- |
| Command arguments | Preserved as argv; no implicit `sh -c` |
| Retry identity | Same logical ID; monotonic logical attempts |
| Durable state | GitHub Git Data CAS aggregate, production only |
| Writer fencing | Lease owner, token, fence, and expiry |
| Dispatch ordering | Pending state before dispatch |
| Unknown dispatch outcome | Record remains pending |
| Transaction across command and API | Not provided |
| Exactly-once execution/effects | Not provided |
| Catch-up, intended timestamp, or start SLO | Not provided |
| Dead-letter or issue notification | Not provided |
