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

The runtime acquires a fenced lease before running the command. Lease owner,
token, fence, and expiry are checked against the read snapshot before building
the GitHub ref update. For a changed lease renewal, provider binding, or
completion, expiry is checked again after blob/tree/commit creation and
immediately before the provider ref operation, so expiry during that work
prevents publication. This final pre-CAS check is still not atomic with
`UpdateRef`: the lease can expire after the check while the provider request is
in flight, and `UpdateRef` has no lease clock. Its
non-force CAS detects a competing ref history, not an expired lease, so an
operation may still be accepted after wall-clock expiry. Fencing prevents a
stale operation from overwriting a newer commit that won the CAS race, but v1
has no atomic expiry cutoff. Acquire can recover a lease it observes as expired
by assigning a new token and higher fence. An unexpired lease returns
`ErrLeaseHeld`, and the runtime returns that error immediately rather than
waiting for recovery. A worker whose lease check is rejected cannot mark work
complete. A failed command becomes `pending` when attempts remain, after the
configured `retry.backoff`; the final failure becomes `exhausted`. Successful
completion becomes `succeeded`. With the default `retry.max_attempts: 1`, a
failure is exhausted without a second attempt.

State bounds and terminal retention are separate concerns. On every changed
mutation, the store compacts terminal records across the aggregate, retaining
the newest configured set of `succeeded` and `exhausted` records. The default
cap is 5,000; `WithTerminalRetention(max)` accepts a different positive cap.
Oldest terminals are removed first by completion time (falling back to creation
time when absent), with function and invocation ID as deterministic tie-breakers.
`pending`, `running`, and retryable `failed` records are never compacted.
Compaction is write-triggered, not a background cleanup or read-time eviction.
The serialized state is still limited to 1 MiB and decoded state to 10,000
invocations, so active/nonterminal records, retained terminal records, and large
record contents can still exhaust the limits and cause a mutation or read to fail.
This is not a retention window or a deletion API.

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
| Durable state | GitHub Git Data CAS aggregate; bounded serialized size/read count, with write-triggered retention of the newest terminal records (default cap 5,000); nonterminal records are not compacted |
| Writer fencing | Snapshot lease checks, a final pre-CAS expiry check, and GitHub non-force CAS; no atomic expiry cutoff |
| Transaction across command and API | Not provided |
| Exactly-once execution/effects | Not provided |
| Catch-up, intended timestamp, or start SLO | Not provided |
| Dead-letter or issue notification | Not provided |
