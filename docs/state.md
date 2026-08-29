# Invocation state

State is the coordination record behind a logical invocation. It is separate from
GitHub's workflow-run record: one invocation may be attempted by more than one run,
and a workflow run may end before it creates or updates state.

## Record format

A record has this shape (pointer fields are omitted when unset):

```json
{
  "schema_version": 1,
  "function": "weekly-lastfm",
  "invocation_id": "weekly-lastfm/2026-08-28",
  "status": "succeeded",
  "attempts": 2,
  "created_at": "2026-08-28T13:15:00Z",
  "started_at": "2026-08-28T13:20:02Z",
  "completed_at": "2026-08-28T13:20:10Z",
  "workflow_run_id": 123456789,
  "trigger": "schedule",
  "result": { "exit_code": 0 }
}
```

A running record may also contain an advisory lease:

```json
"lease": {
  "owner": "workflow-run-123",
  "expires_at": "2026-08-28T13:40:00Z"
}
```

Fields:

- `schema_version` is `1` for records written by the current stores.
- `function` and `invocation_id` identify the record. IDs must belong to the function
  and use the path-safe canonical form described in [semantics](semantics.md).
- `status` is one of `pending`, `running`, `succeeded`, `failed`, or `exhausted`.
- `attempts` increments when a runner claims an attempt, including recovered attempts.
- Timestamps are UTC. `created_at` is set at creation; `started_at` and `completed_at`
  describe lifecycle transitions.
- `workflow_run_id` and `trigger` correlate state with GitHub when available.
- `lease` identifies the current owner and expiration. An expired lease may be recovered.
- `result` stores the exit code and, for an execution error, its error text. Command
  output is not stored in state; use GitHub Actions logs.

Secrets and complete process environments MUST NOT be written to this record.

## Store contract

Backends implement four operations:

```go
type Store interface {
    Get(ctx context.Context, function string, id InvocationID) (*Invocation, error)
    Create(ctx context.Context, invocation Invocation) error
    CompareAndSwap(ctx context.Context, previous Invocation, next Invocation) error
    List(ctx context.Context, function string, limit int) ([]Invocation, error)
}
```

`Get` returns `ErrNotFound` when absent. `Create` rejects duplicate keys with a
conflict. `CompareAndSwap` updates only if the stored record still equals `previous`;
otherwise it returns `ErrConflict`. Callers must reread after a conflict, not overwrite
the newer record. `List` is scoped to a function when one is provided; a positive limit
truncates the result and zero means no limit.

Stores validate schema, function/ID ownership, status, timestamps, and lifecycle
invariants. They copy records crossing the API boundary so callers cannot mutate stored
pointers by accident. Malformed records and path/identity mismatches fail closed.

The record transition helpers enforce these edges:

```text
pending -> running
running -> succeeded | failed
failed  -> pending | exhausted
```

A retrying runtime uses the `failed -> pending` edge before its next attempt. A runtime
with no retry policy leaves the failed record in `failed`.

## Runtime algorithm

For one `runtime.Invoke` call:

1. Read the record, creating `pending` if it does not exist.
2. Return a terminal record unchanged.
3. Refuse an active lease (or a running record with no recoverable lease) as busy.
4. CAS `pending` to `running`, incrementing `attempts`; when leases are enabled, record
   the owner and finite expiry. An expired running lease may be recovered through CAS.
5. Execute the argv command with the caller's context and invocation environment.
6. CAS the running record to `succeeded` for exit code 0, or `failed` otherwise, clearing
   the lease.
7. If `retry.max_attempts` permits another attempt, CAS `failed` to `pending`, wait for
   `retry.backoff`, and repeat. Otherwise transition `failed` to `exhausted`.

Completion and retry state writes use a context without cancellation where possible, so
a command timeout does not automatically prevent recording the failed attempt. A CAS
conflict is returned or causes a reread at the claim boundary; it is never silently
merged. A lease limits concurrent claims but cannot undo an external effect made before
a process crash.

## Current backends and durability boundary

The repository provides:

- `state.Memory`, a mutex-protected process-local store; and
- `state.BranchStore`, a branch-shaped file store with atomic optimistic updates.

The CLI's explicit `state.backend: memory` choice creates process-local state. The default
branch backend writes JSON below `.ghaas-state/<branch>` in the current checkout, using a
branch namespace and a `functions/<function>/` subtree (the branch defaults to
`ghaas-state`). It validates path components and protects concurrent writers within one
process, but does **not** commit or push those files to a GitHub branch. Independent
processes/runners require a remote Store implementation for cross-process coordination.
The Store interface lets an embedding application provide that implementation.

Do not infer GitHub durability from the `branch` name or from the workflow's `contents:
write` permission. A remote branch integration must explicitly read the branch, perform
an optimistic update, and push a new commit/ref; that integration is outside the stock
CLI runtime.

## Leases and recovery

The runtime's generated entrypoint sets the lease TTL to the effective function timeout.
A live lease blocks another owner. After expiration, another runner may CAS-acquire a
new lease and execute a recovery attempt. Recovery is useful for abandoned `running`
records, but lease expiry is not evidence that the first runner stopped: a partitioned or
slow runner can still produce an external effect. Use destination idempotency keys and
keep lease durations appropriate for the command.
