# ghaas v1 specification

GitHub Actions as a Service (`ghaas`) defines command-backed functions in a
strict YAML manifest and compiles each function into a GitHub Actions workflow.
This document is normative for manifest, invocation, state, workflow, and CLI
behavior. Version 1 is intentionally small: it does not claim exactly-once
execution or provide a general queue.

## 1. Manifest

A manifest is exactly one YAML document with a required `version: 1`, at least one
function, and no unknown fields. The accepted shape is:

```yaml
version: 1

defaults:
  timeout: 15m

functions:
  hello:
    runtime: command
    command: [bash, examples/hello/hello.sh]
    timeout: 15m
    env:
      GREETING: hello
    secrets: [API_TOKEN]
    retry:
      max_attempts: 3
      backoff: 30s
    schedule:
      cron: "15 9 * * 5"
      timezone: America/New_York
    concurrency:
      max: 1
```

Supported fields are:

- `defaults.timeout`, a positive duration inherited by functions;
- `runtime: command`, the only runtime in v1;
- `command`, a non-empty argv array whose boundaries are preserved;
- `timeout`, a positive duration for one command and its job;
- `env`, literal values with valid names;
- `secrets`, valid GitHub secret names only;
- `schedule.cron` and `schedule.timezone`, validated provider schedule inputs;
- `concurrency.max`, omitted or `1`; and
- `retry.max_attempts`, a positive integer defaulting to `1`, and
  `retry.backoff`, a positive duration defaulting to `0`.

There are no state, dead-letter, issue, execution-window, SLO, or catch-up fields.
`GHAAS_*` names are reserved. A name cannot occur in both `env` and `secrets`.
Secret values never occur in the manifest or local state.

## 2. CLI control plane

The local CLI commands are:

```text
ghaas init [--force]
ghaas validate
ghaas generate [FUNCTION]
ghaas deploy [--check] [--function FUNCTION]
ghaas invoke [--ref REF] FUNCTION
ghaas status FUNCTION [--invocation ID] [--json]
ghaas logs FUNCTION [--invocation ID]
ghaas version
ghaas completion {bash|zsh|fish}
```

`runtime invoke FUNCTION` is a hidden workflow entrypoint. `validate`,
`generate`, and local `deploy` do not contact GitHub. `deploy --check` writes
nothing and fails when generated files are absent, different, or stale.

`invoke` creates a UUID and canonical manual ID `<function>/<UUID>`. It writes a
pending durable record before calling `workflow_dispatch`, sending only the UUID
as the dispatch input. If dispatch returns an error with an unknown outcome, the
record remains pending and the CLI never silently creates a replacement ID.
`--ref` is passed through; an omitted ref resolves the repository default branch.

`status` reads durable state first when available. An explicit logical ID must be
for the selected function and is matched exactly. Human output includes durable
status, attempt count, and the exact provider run binding. `status --json` emits
one stable, newline-terminated JSON object for scripting. `logs` uses the exact
state-bound provider run and provider attempt when available; numeric provider run
IDs remain an explicit lookup form.

Unreleased binaries print `dev`. Generation from a development binary refuses an
implicit installer version. Release builds embed the exact v-prefixed release tag
through build ldflags. Completion output is generated from the same command table
as help.

## 3. Logical invocation identity

A logical invocation is not a workflow run. Multiple provider runs may be related
to one logical invocation through retries or reruns.

Manual identity:

```text
<function>/<UUID>
```

Scheduled identity:

```text
<function>/<positive-GITHUB_RUN_ID>
```

A scheduled run uses its native GitHub run ID because no intended schedule
instant is available at execution time. v1 has no intended timestamp, catch-up
scheduler, timestamp-key dedupe, or claim that a missing record proves a schedule
did not execute.

## 4. Runtime metadata and command execution

The runtime executes the configured argv directly with inherited standard streams.
It does not implicitly invoke a shell. Shell behavior must be explicit, for
example `[sh, -c, ./script]`. Context cancellation, timeout, and non-zero exit
make the command unsuccessful.

The runtime supplies these reserved values:

```text
GHAAS_FUNCTION
GHAAS_INVOCATION_ID
GHAAS_ATTEMPT
GHAAS_TRIGGER                  # manual or schedule
GHAAS_WORKFLOW_RUN_ID
GHAAS_WORKFLOW_RUN_ATTEMPT
GHAAS_ATTEMPT_REASON
```

`GHAAS_INVOCATION_ID` is the canonical logical ID. `GHAAS_ATTEMPT` is the logical
attempt number and is monotonic across retries of the same ID. Workflow run ID,
workflow run attempt, and attempt reason are provider metadata and are stored
separately from the logical attempt.

Metadata overrides manifest and ambient values for reserved names. No field makes
external effects exactly once; applications should pass the logical ID to an
idempotency facility when one is available.

## 5. Generated workflows

Compilation is deterministic for identical manifest, function, and released
version inputs. Each workflow contains a pinned checkout action, a pinned ghaas
release installer, bounded downloads, exact SHA-256 verification, manifest
environment/secrets, and the thin runtime command:

```text
ghaas runtime invoke <function>
```

The installer archive and checksum are selected from the exact release tag. Action
references are immutable commit pins. The workflow requests:

```yaml
permissions:
  contents: write
```

This permission is required for durable state commits. Same-workflow retries use
the state store and do not self-dispatch or perform an Actions write. Schedule,
timezone, timeout, retry, and concurrency settings are represented in workflow
YAML. Generated files are `.github/workflows/ghaas-<function>.yml`.

## 6. Durable state

Production state uses GitHub Git Data compare-and-swap only. There is no local
`BranchStore` or process-local durable backend. The store has one bounded,
deterministically encoded aggregate:

```text
refs/heads/ghaas-state-v1:.ghaas/state/v1.json
```

The document schema version is `1`. A mutation reads the current ref, creates a
blob, tree, and commit, then updates the ref with `force=false`. A bounded CAS
conflict loop re-reads and rebases. Missing state refs are initialized as an
empty aggregate.

Each invocation record contains schema, function, ID, status, attempts,
max-attempts, timestamps, last error, an optional exact provider binding, and an
optional lease/result. Allowed statuses are exactly:

```text
pending
running
succeeded
failed
exhausted
```

A record transitions from pending to running, then to succeeded or failed. A
failed record returns to pending when retries remain and becomes exhausted on the
final failure. Retries retain the same ID and increment attempts monotonically.
There is no dead-letter record or issue notification.

A running record has a lease with owner, opaque token, fence, and expiry. Acquire,
renew, provider binding, and completion verify every lease value. A stale or
expired writer is rejected. Provider binding stores exact `run_id`,
`run_attempt`, and provider reason; status and logs do not infer a replacement
from a provider latest-run query.

## 7. Scheduling and concurrency

A schedule is a GitHub provider trigger. Its cron and IANA timezone are validated,
but delivery and start time remain provider behavior. `concurrency.max: 1`
emits a per-function GitHub concurrency group with `cancel-in-progress: false`;
it is not a substitute for the fenced durable state lease.

A schedule run has no catch-up obligation. If GitHub drops, duplicates, reruns, or
delays it, the resulting behavior remains subject to provider semantics and the
state transitions observed by the runner.

## 8. Failure and effect boundary

GitHub may accept a dispatch while a client times out. The pending record makes
that uncertainty visible but cannot identify an unseen run. A runner can crash
after an external effect and before recording success; retrying can repeat that
effect. A state commit can fail after a command succeeds, and a provider run can
become unavailable before its final state is visible.

Therefore v1 provides neither a transaction across command and provider API nor
exactly-once process execution or external effects. It provides durable,
fenced logical state and bounded retry decisions. It provides no start-time SLO,
execution window, queue, dead-letter, issue, or reliability-metric contract.

## 9. Security and verification

Generated workflows run with ordinary repository runner privileges and are not a
sandbox. Review manifests, scripts, generated YAML, action pins, and release
references. Scope local tokens narrowly: dispatch needs Actions write, status and
logs need Actions read, and generated state needs Contents write on the
run-scoped token. Never commit or print tokens or secret values.

CI is the verification gate for formatting, vet, race tests, deterministic
compiler behavior, generated-workflow linting, and CLI smoke behavior. Release
verification reuses those CI gates and checks exact tag/version embedding and
artifact checksums; publishing is intentionally outside this workflow.
