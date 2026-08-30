# State boundary

## v0.1 is stateless

The generated workflow does not maintain a ghaas invocation store. `ghaas runtime invoke`
loads and validates `ghaas.yaml`, exports invocation metadata, runs one command, and
returns its process result. It does not create state records, acquire leases, retry a
failed command, write a branch, commit files, or call an issue API.

The runner workspace is ephemeral from ghaas's point of view. A `.ghaas-state` directory,
a branch-shaped folder, or a local file created by a command is not a ghaas backend and is
not durable across hosted runners. Examples do not imply local or remote branch
persistence.

## Workflow-run view

`status` and `logs` query GitHub's workflow-run API. A run can fail before the command
starts, and a command can affect an external service before its run result is observed.
Consequently a provider result is an execution observation, not a transactional business
record.

The provider-neutral workflow-run model includes fields such as:

```text
ID
Name
DisplayTitle
Workflow
Status
Conclusion
CreatedAt / StartedAt / UpdatedAt
```

Generated workflows set `Name` to `ghaas: <function>` and set the run-name expression
exactly to:

```yaml
run-name: 'ghaas: <function>/${{ inputs.ghaas_invocation_id || github.run_id }}'
```

GitHub exposes that value as `display_title`, represented by `DisplayTitle`. When a
caller asks for an explicit logical ID, matching must use that title exactly. It must not
fall back to timing, run order, or the newest workflow. A dispatch without `--ref` uses
the target repository's default branch; an explicit ref overrides it.

A run's status may be `queued`, `in_progress`, or `completed` with a conclusion such as
`success`, `failure`, `cancelled`, or `timed_out`. GitHub can delay API visibility,
redact/expire logs, or cancel a run. None of these fields records a durable command
outcome or rolls back an external side effect.

Manual dispatches carry a function-scoped `<function>/<canonical UUID>` logical ID, with
the UUID passed as the workflow input. A scheduled run has no dispatch input and uses the
provider workflow run ID fallback. The runtime exports that input or fallback as
`GHAAS_INVOCATION_ID`; it is useful for tracing and for a destination idempotency key,
but it is not a stored claim and does not establish exactly-once execution.

The generated run name is a correlation aid. More than one workflow run can exist for an
opportunity, and a workflow run may exist without a matching durable invocation record.
Callers that need business completion must query the destination system or maintain an
application-owned record.

## Deferred state work

The following are explicitly roadmap features, not v0.1 behavior:

- a typed invocation record and durable `Get`/create/compare-and-swap store;
- persistent state shared by independent runners or repositories;
- retry and backoff scheduling, attempt history, or dead-letter/exhaustion handling;
- leases and recovery for abandoned running work;
- remote Git branch persistence with protected refs and conflict handling;
- execution windows, SLO declarations, and catch-up scheduling; and
- APIs that infer business completion from workflow state.

Until those features are designed and implemented, do not infer durability from a branch
name, a workflow permission, a run ID, or the presence of `GHAAS_INVOCATION_ID`. If an
application needs durable coordination today, it must own that store and its idempotency
policy outside the generated ghaas workflow.
