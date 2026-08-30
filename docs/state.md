# Durable invocation state

## Production backend

The production backend is GitHub Git Data CAS only. State lives on
`refs/heads/ghaas-state-v1` as one bounded deterministic JSON aggregate:

```text
.ghaas/state/v1.json
```

The document and every record use schema version `1`. Updates read the current
ref, create a blob/tree/commit, and update the ref with `force=false`. A bounded
conflict loop re-reads the new head and rebases the mutation. The local checkout
is never used as a state backend, and there is no local `BranchStore`.

The aggregate is deliberately coordination state rather than a general-purpose
database. It has bounded size/count and deterministic record ordering. A missing
state ref is treated as an empty aggregate and is created by the first mutation.

## Access required

The Git Data API reads used by the production state store require repository
`Contents: read`; state mutations require `Contents: write`. Accordingly, a
local `invoke` needs both `Contents: write` (to create its pending record) and
`Actions: write` (to dispatch the workflow). State-first local `status` and
`logs` need `Contents: read` and `Actions: read`; a direct
`logs --invocation RUN_ID` lookup skips the logical-record read and needs
`Actions: read`.

The local CLI holds its `GITHUB_TOKEN` as a bearer credential for these
control-plane calls. In a generated workflow, the run-scoped token is available
to ghaas for state writes but is stripped from the configured command's
environment, along with `GH_TOKEN` and `GHAAS_STATE_*`. Declared secrets remain
intentionally available to that command. This is credential separation, not a
sandbox or an exactly-once boundary.

## Record identity and status

Manual CLI invocations use `<function>/<UUID>`. The CLI creates their `pending`
record before dispatch. If dispatch returns an error after GitHub may have
accepted it, that record remains pending; the CLI does not issue a blind second
ID.

Scheduled workflows use `<function>/<positive-GITHUB_RUN_ID>`. There is no
intended schedule timestamp, catch-up behavior, timestamp-key dedupe, or claim
that a missing record proves the workflow did not run.

Allowed statuses are:

```text
pending -> running -> succeeded
                 \-> failed -> pending (while attempts remain)
                              \-> exhausted
```

Retries keep the same ID and increment `attempts` monotonically. `max_attempts`
is a positive manifest policy value (default `1`). `backoff` defaults to zero
and is applied between retry attempts. v1 has no dead-letter state, issue
notification, execution window, or SLO fields.

## Leases and provider references

A running record carries a lease with owner, opaque token, monotonically
increasing fence, and expiry. Acquire, renew, bind-provider, and complete
operations verify all lease values. A stale token, fence, owner, or expired lease
is rejected; stale writers cannot overwrite a newer worker's result.

Provider metadata is retained exactly as `run_id` plus `run_attempt` (with the
provider reason where available). Status is durable-state-first. Logs use the
bound provider run and attempt rather than choosing a fresh provider latest run.

## Operational boundary

GitHub remains asynchronous: runs may be delayed, duplicated, rerun, cancelled,
or lost. Durable state records coordination and provider observations; it does
not make command side effects transactional or exactly once. A command may
partially apply an external effect before its result is recorded. Applications
should use `GHAAS_INVOCATION_ID` with an idempotency facility when external
effects require deduplication.
