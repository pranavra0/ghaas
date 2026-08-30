# v1 semantics

`ghaas` compiles a strict `version: 1` manifest into one GitHub Actions workflow
per function. The workflow is the execution boundary: it checks out the target
repository, installs the exact released `ghaas` tag selected by the compiler,
and invokes the configured command directly as an argv array.

## Workflow and permissions

Each workflow has `workflow_dispatch` and, when configured, a GitHub schedule.
The generated run name includes the logical invocation identity. The workflow
requests `contents: write` because the runtime persists durable state through the
Git Data API. It writes only `refs/heads/ghaas-state-v1` and the aggregate
`.ghaas/state/v1.json`; it does not write issues or dead-letter records. Installer
archives and `SHA256SUMS` are downloaded with bounded retries and verified before
execution. Action references are immutable pins.

Local control-plane calls use the developer's `GITHUB_TOKEN`. `invoke` creates the
pending durable record before dispatching, so it requires `Contents: write` and
`Actions: write`. State-first `status` reads the durable aggregate as its source
of truth. State-first `logs` reads that aggregate, then reads provider data; both
operations require `Contents: read` and `Actions: read`. `logs --invocation RUN_ID`
can select a provider run directly and requires `Actions: read`; it does not read a
logical invocation record first.

The local token is a bearer credential for the GitHub API, not a credential passed
to a command by ghaas. In a generated workflow, `github.token` is available to the
ghaas runtime for durable-state writes. Before starting the configured command,
the runtime removes `GITHUB_TOKEN`, `GH_TOKEN`, and `GHAAS_STATE_*` from the child
environment; declared GitHub secrets are deliberately passed through. This is a
credential boundary, not a sandbox: the command is trusted repository code with
runner network access and whatever other non-state environment the runner provides.

A development binary identifies its version as `dev`. It refuses to generate a
workflow with an implicit installer version. Release builds embed the exact
v-prefixed tag through build ldflags. Generate and commit workflows from a
released binary (or provide an explicit released compiler version in an
embedding application).

## Logical IDs and metadata

A manual invocation creates a fresh logical ID `<function>/<UUID>`. The CLI sends
only the UUID as the `workflow_dispatch` input, then records the pending state
before dispatch. A dispatch error does not erase or replace that record: its
outcome is unknown and remains pending for inspection.

A scheduled invocation uses `<function>/<positive-GITHUB_RUN_ID>`. The provider
run ID is the identity available at execution time; schedules have no intended
start timestamp, catch-up semantics, or timestamp-key deduplication claim.

The runtime exports:

- `GHAAS_FUNCTION`;
- `GHAAS_INVOCATION_ID`, the canonical logical ID;
- `GHAAS_ATTEMPT`, the logical attempt number;
- `GHAAS_TRIGGER`, `manual` or `schedule`;
- `GHAAS_WORKFLOW_RUN_ID`;
- `GHAAS_WORKFLOW_RUN_ATTEMPT`, the provider rerun attempt; and
- `GHAAS_ATTEMPT_REASON`, the provider trigger/reason metadata.

These values are correlation metadata, not exactly-once side-effect guarantees.
`GHAAS_INVOCATION_ID` can be used as an idempotency key with a destination API
that supports one.

No correlation ID, lease, retry, or state record makes command execution or
external effects exactly once. GitHub can delay, duplicate, rerun, cancel, or lose
a workflow, and a command can partially apply an external effect before its
result is recorded. Use `GHAAS_INVOCATION_ID` as an idempotency key only with a
destination API that provides idempotency.

## State and retries

Production state is a GitHub Git Data compare-and-swap store. It uses one bounded,
deterministic JSON document at `.ghaas/state/v1.json` on
`refs/heads/ghaas-state-v1`, schema version `1`. There is no local `BranchStore`
or local durable backend.

Records have the statuses `pending`, `running`, `succeeded`, `failed`, and
`exhausted`. `retry.max_attempts` is a positive integer with default `1`;
`retry.backoff` is a positive duration with default `0`. A retry retains the same
logical ID and advances its logical attempt monotonically. Lease owner, opaque
token, fence, and expiry are checked on every write, so a stale worker cannot
commit after losing its lease. CAS conflicts are re-read and rebased with a
bounded retry count.

The provider binding retains the exact `run_id` and `run_attempt`. Status reads
that durable record first and does not infer a replacement run from a provider
"latest" query. Logs use the state-bound run ID and attempt when the provider
supports attempt-specific logs.

## CLI output

Human output is concise and plain in noninteractive output. `status` reports the
durable status, attempt count, and exact provider run binding when available.
`status --json` emits one stable, newline-terminated JSON object whose field order
and names are part of the scripting surface. `logs` never chooses a new logical
ID when a selected record or dispatch has an unknown provider outcome.

The CLI makes no claims of a transaction spanning a command and an external API,
exactly-once execution, start-time SLO, queue, catch-up scheduler, dead-letter
processing, or issue notification.
