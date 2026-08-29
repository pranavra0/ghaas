# Failure model

GitHub Actions is an asynchronous, at-least-once substrate. ghaas makes invocation
identity, state transitions, leases, and retries explicit, but it cannot turn a hosted
runner and an external network into a transaction. The safe default is:

> at-least-once attempts; logical-invocation deduplication only while attempts share a
> durable Store; exactly-once external effects only when the destination cooperates.

The runtime supports retry and lease transitions in one process. The stock generated
workflow's default branch backend is file-backed in the runner workspace and is not
pushed to a remote GitHub branch, so fresh hosted runners do not share those records.
Use an embedding application's durable Store if cross-run coordination is required.

## Failure cases

### Delayed workflow scheduling

GitHub may start a scheduled workflow late. A schedule key or configured execution
window is not a start-time SLA. `schedule.timezone` is validated and used for logical-ID
derivation, while the generated cron is still subject to the provider's scheduler and
UTC behavior. Measure delay from the schedule key or application timestamp if it matters,
and make the command safe to run late.

### Dropped scheduled runs

A scheduled run can be delayed or not created. ghaas has no independent scheduler or
catch-up loop. A later schedule/manual dispatch is a separate opportunity and can have a
different logical ID. If the work is important, use an external scheduler or implement
durable catch-up policy in the application rather than assuming every cron tick exists.

### Duplicate workflow execution

Manual dispatch, schedule delivery, retries, and operator re-runs can produce more than
one workflow run. The same logical ID is the deduplication key. A shared durable Store
can let one runner claim the record, mark it terminal, and let later runs exit. The
stock memory/branch-shaped stores do not coordinate separate fresh hosted runners, so
duplicate runs may execute the command.

### Concurrent runners

GitHub concurrency groups (`max: 1`) reduce overlap per function and do not cancel an
in-progress run. They are not a distributed transaction and cannot cover every race or
a run started through another path. Store compare-and-swap must protect claims. An
active lease blocks another owner; a stale lease can be recovered only after expiration.
A CAS conflict is a coordination signal; never overwrite the newer record.

### Retry and backoff

For `retry.max_attempts: N`, N includes the initial execution. A failed attempt is
recorded, returned to `pending`, and retried after `retry.backoff` until it succeeds or
reaches N, then becomes `exhausted`. Backoff is an in-process delay, not a durable job
queue: cancellation, process exit, or runner loss can prevent the next attempt. Without
a positive retry policy, a failure remains `failed`. `on_exhausted.issue` is a best-effort
notification and does not change the state outcome.

### Runner termination

A runner can be evicted, cancelled, or lose its network connection while a command is
running. It may have produced an external effect without writing `succeeded`. A later
runner can recover only when a shared Store contains an expired lease; the stock hosted
workflow normally has no shared record across fresh runners. Recovery can repeat the
effect.

### Process crash

A crash before the command starts leaves no completion record or may leave `pending`.
A crash during or after the command leaves an ambiguous outcome and may leave `running`
until its lease expires. Treat a failed state write as "outcome unknown", not proof that
the command did not run. Application operations should be idempotent or use an external
idempotency key.

### GitHub API failure

Dispatch can fail before a workflow exists, or the API can time out after GitHub accepted
the request. Retrying dispatch can create duplicates. Listing, state operations, and logs
can be temporarily unavailable or rate-limited; `status` and `logs` then cannot establish
business completion. Record the generated invocation ID outside the API response when
coordinating a manual dispatch, and inspect GitHub before retrying a request with unknown
outcome. Local `invoke`, `status`, and `logs` require a suitable `GITHUB_TOKEN` and
repository discovery; `validate`, `generate`, and local `deploy` do not.

### Git state and local state conflicts

`BranchStore` performs optimistic file updates and detects stale records. Two writers
that share its root can race; reread after `ErrConflict`. The stock store does not commit
or push the state directory to GitHub. A future/embedding remote backend must similarly
reread after a ref conflict and never force-push a stale snapshot.

### State corruption

Malformed JSON, an unsupported schema, an impossible status edge, or a path/identity
mismatch must fail closed: do not execute based on an untrusted or ambiguous record.
Preserve the corrupt object for diagnosis, restore a known-good copy, and repair through
a controlled operation. Never "fix" corruption by creating a fresh pending record with
the same ID; that can repeat side effects.

### Network failure during state commit

The command may succeed while the final state update times out. The caller cannot know
whether the update reached the Store or GitHub. A retry may execute the command again.
This is why logical deduplication is not external-effect exactly-once. Use an idempotency
key on the external operation and make the state transition retryable.

### Effect succeeds but state commit fails

Example: a Discord message is accepted, then the runner crashes before storing
`succeeded`. A recovery or retry can send the message again. ghaas cannot roll back a
message or infer that it was accepted. Use the invocation ID as the receiving API's
idempotency key where supported, or persist an application-level outbox/deduplication
record at the destination.

### State commit succeeds but the workflow appears failed

The workflow can lose its final log upload, be cancelled after the state write, or receive
an API error after the remote commit succeeded. GitHub's red/unknown run is not
authoritative for a durable invocation record. Query state and the external system before
retrying. `status` prefers a state record and reconciles pending/running records with
matching provider runs when metadata permits; otherwise it reports the newest workflow
run. Neither path can infer business completion from logs alone.

## What is and is not guaranteed

| Property | Current guarantee |
| --- | --- |
| Command argv boundaries | Preserved; no implicit `sh -c` |
| Process timeout/cancellation | Context cancellation is propagated to the child |
| Invocation lifecycle validation | Enforced by stores and invocation helpers |
| CAS protection | Provided by Memory and BranchStore for records they share |
| Lease behavior | Active leases block claims; expired leases may be recovered |
| Retry/backoff | Applied by one runtime process when configured; not a durable scheduler |
| Cross-run deduplication | Not provided by the stock fresh-runner workflow |
| Remote GitHub state branch | Not provided by the stock CLI BranchStore |
| Exactly-once process execution | Never promised |
| Exactly-once external effects | Only when the destination cooperates |
| Scheduled start time/timezone | Not guaranteed by GitHub Actions; cron remains provider-controlled |
| Execution-window enforcement | Not provided; window is validation/metadata |
| SLO success/delay target | Not enforced by the runtime |
| Transaction across command + state + API | Not provided |

Do not describe a function as exactly once merely because its workflow has a concurrency
group, because a lease exists, or because one invocation ID is visible in its environment.
