# ghaas

GitHub Actions as a Service.

An intentionally questionable serverless runtime built on top of GitHub Actions.

`ghaas` lets users define scheduled or manually invoked functions using a small declarative manifest. It compiles those function definitions into GitHub Actions workflows and provides a local CLI for deployment, invocation, status inspection, logs, state management, and eventually reliability reporting.

The project is implemented in Go.

The joke is that GitHub Actions is being used as a serverless platform.

The engineering goal is serious: define useful execution semantics on top of an unreliable, ephemeral, workflow-oriented substrate.

---

# 1. Motivation

GitHub Actions already provides many primitives associated with serverless computing:

* ephemeral compute
* secret injection
* scheduled execution
* manual/event-driven invocation
* concurrency controls
* execution logs
* timeouts
* repository-scoped identity
* API-accessible execution history
* zero owned servers

However, GitHub Actions does not expose a coherent function abstraction.

A user instead manages:

* workflow YAML
* cron syntax
* workflow runs
* caches
* artifacts
* concurrency groups
* GitHub API calls
* retry logic
* state persistence
* duplicate execution prevention

`ghaas` attempts to collapse those implementation details into a semantic object:

```text
Function {
    code
    schedule
    inputs
    secrets
    state
    retry policy
    invocation history
}
```

The user reasons about a function and its logical invocations.

GitHub workflow runs are an implementation detail.

---

# 2. Non-goals

`ghaas` is not intended to be:

* a replacement for AWS Lambda, Cloud Run, or Kubernetes
* an HTTP request/response compute platform
* a low-latency execution system
* a strongly consistent distributed database
* a generic container orchestrator
* a production SLA-backed compute service
* an attempt to conceal GitHub Actions' limitations

The system should make its guarantees explicit.

Where GitHub Actions cannot provide a guarantee, `ghaas` should document that fact rather than pretending otherwise.

---

# 3. Initial user experience

A repository contains:

```text
ghaas.yaml
```

Example:

```yaml
version: 1

functions:
  weekly-lastfm:
    runtime: command

    command:
      - uv
      - run
      - fmdiscordbot
      - --once

    schedule:
      timezone: America/New_York
      cron: "15 9 * * 5"

    timeout: 20m

    secrets:
      - DISCORD_TOKEN
      - LASTFM_API_KEY
      - LASTFM_API_SECRET

    env:
      DISCORD_CHANNEL_ID: "${{ vars.DISCORD_CHANNEL_ID }}"

    concurrency:
      max: 1
```

The user runs:

```bash
ghaas deploy
```

Output:

```text
compiling weekly-lastfm
writing .github/workflows/ghaas-weekly-lastfm.yml

1 function deployed
```

Manual invocation:

```bash
ghaas invoke weekly-lastfm
```

Status:

```bash
ghaas status weekly-lastfm
```

Example:

```text
Function: weekly-lastfm

Last logical invocation:
  ID:       weekly-lastfm/2026-W35
  Status:   succeeded
  Started:  2026-08-28 09:20:12 -0400
  Duration: 8.4s

Recent reliability:
  successful: 31
  failed:      1
  skipped:     42
```

---

# 4. Architecture

The project consists of two conceptual components.

## 4.1 Local control plane

The `ghaas` CLI runs locally or in CI.

Responsibilities:

* parse configuration
* validate configuration
* compile manifests into workflow YAML
* write generated workflows
* invoke functions through the GitHub API
* inspect workflow runs
* inspect logical invocation state
* display logs
* compute reliability information

The local CLI should remain stateless wherever possible.

## 4.2 GitHub-hosted execution plane

Generated GitHub workflows execute functions.

Responsibilities:

* derive logical invocation IDs
* acquire invocation state
* decide whether execution should occur
* execute the configured command
* record success/failure
* persist state
* expose metadata for later inspection

The workflow should use a small `ghaas` runtime binary rather than reproducing complicated logic in shell.

Conceptually:

```text
                    ghaas CLI
                       |
                       |
                 compile/deploy
                       |
                       v
              GitHub workflow YAML
                       |
                       |
                   scheduled
                       |
                       v
               ephemeral runner
                       |
                       v
              ghaas runtime entry
                       |
             +---------+---------+
             |                   |
          state                 command
             |                   |
             +---------+---------+
                       |
                 invocation result
```

---

# 5. Repository structure

Suggested initial structure:

```text
ghaas/
├── cmd/
│   └── ghaas/
│       └── main.go
│
├── internal/
│   ├── config/
│   │   ├── config.go
│   │   └── validate.go
│   │
│   ├── compiler/
│   │   ├── compiler.go
│   │   └── workflow.go
│   │
│   ├── invocation/
│   │   ├── invocation.go
│   │   ├── id.go
│   │   └── executor.go
│   │
│   ├── state/
│   │   ├── state.go
│   │   ├── memory.go
│   │   └── branch.go
│   │
│   ├── github/
│   │   ├── client.go
│   │   ├── workflows.go
│   │   └── logs.go
│   │
│   ├── status/
│   │   └── status.go
│   │
│   └── render/
│       └── yaml.go
│
├── pkg/
│   └── manifest/
│       └── manifest.go
│
├── examples/
│   ├── hello/
│   └── weekly-lastfm/
│
├── docs/
│   ├── semantics.md
│   ├── state.md
│   └── failure-model.md
│
├── go.mod
├── go.sum
├── README.md
└── LICENSE
```

Prefer `internal/` for implementation details.

Only export APIs under `pkg/` if there is a real external-use case.

---

# 6. CLI

Initial command structure:

```text
ghaas
├── init
├── validate
├── deploy
├── generate
├── invoke
├── status
├── logs
└── version
```

Later:

```text
ghaas
├── state
│   ├── inspect
│   └── repair
│
├── invocations
│   └── list
│
└── doctor
```

---

# 7. Commands

## 7.1 `ghaas init`

Creates:

```text
ghaas.yaml
```

Optionally creates an example function.

Example:

```bash
ghaas init
```

---

## 7.2 `ghaas validate`

Parses and validates the manifest.

Checks:

* duplicate function names
* malformed cron expressions
* malformed durations
* invalid timezone names
* unsupported runtime types
* invalid concurrency values
* invalid state configuration
* invalid retry configuration

No GitHub API access required.

---

## 7.3 `ghaas generate`

Compiles function definitions into GitHub workflow YAML without modifying files.

Example:

```bash
ghaas generate weekly-lastfm
```

Useful for debugging.

---

## 7.4 `ghaas deploy`

Generates workflow files.

For v0.1, deployment means writing deterministic generated files:

```text
.github/workflows/ghaas-<function>.yml
```

Example:

```bash
ghaas deploy
```

Flags:

```text
--check
--function NAME
```

`--check` exits nonzero if generated workflows differ from committed ones.

This supports CI enforcement.

---

## 7.5 `ghaas invoke`

Triggers a `workflow_dispatch` execution.

Example:

```bash
ghaas invoke weekly-lastfm
```

Possible later support:

```bash
ghaas invoke resize-image --input file=test.png
```

v0.1 does not need arbitrary structured input.

---

## 7.6 `ghaas status`

Displays logical function status.

Example:

```text
Function: weekly-lastfm

Schedule:
  Friday 09:15 America/New_York

Last invocation:
  ID:       weekly-lastfm/2026-W35
  Status:   succeeded
  Attempts: 2
  Duration: 8.4s
```

---

## 7.7 `ghaas logs`

Displays logs from the GitHub workflow run associated with a logical invocation.

```bash
ghaas logs weekly-lastfm
```

Optional:

```bash
ghaas logs weekly-lastfm --invocation weekly-lastfm/2026-W35
```

---

# 8. Manifest format

Top-level format:

```yaml
version: 1

defaults:
  timeout: 15m

functions:
  <name>:
    ...
```

Go representation:

```go
type Manifest struct {
    Version   int                 `yaml:"version"`
    Defaults  Defaults            `yaml:"defaults,omitempty"`
    Functions map[string]Function `yaml:"functions"`
}
```

Example function:

```go
type Function struct {
    Runtime     RuntimeConfig     `yaml:"runtime"`
    Command     []string          `yaml:"command"`
    Schedule    *ScheduleConfig   `yaml:"schedule,omitempty"`
    Timeout     Duration          `yaml:"timeout,omitempty"`
    Environment map[string]string `yaml:"env,omitempty"`
    Secrets     []string          `yaml:"secrets,omitempty"`
    Concurrency ConcurrencyConfig `yaml:"concurrency,omitempty"`
    State       *StateConfig      `yaml:"state,omitempty"`
    Retry       *RetryConfig      `yaml:"retry,omitempty"`
}
```

The exact Go structure may evolve.

Backward compatibility should begin only after `version: 1` is declared stable.

---

# 9. Runtime model

v0.1 supports one runtime:

```yaml
runtime: command
```

The command executes directly on the GitHub-hosted runner.

Example:

```yaml
runtime: command

command:
  - go
  - run
  - ./cmd/report
```

Do not initially create language-specific abstractions such as:

```text
runtime: python
runtime: node
runtime: go
```

Those mostly become dependency installation policy.

Keep `ghaas` concerned with invocation semantics, not package managers.

Language-specific conveniences can be added later.

---

# 10. Logical invocations

This is the central abstraction.

A GitHub workflow run is not equivalent to a function invocation.

A logical invocation represents the semantic event the user expects.

For example:

```text
weekly-lastfm/2026-W35
```

Multiple workflow runs may correspond to this one logical invocation.

Example:

```text
09:15    scheduled workflow run
09:16    runner unavailable

09:20    retry workflow run
09:20    function executes successfully

09:25    retry workflow run
09:25    sees completed invocation
09:25    exits without executing function
```

The user sees:

```text
weekly-lastfm/2026-W35
status: succeeded
attempts: 2
```

not three unrelated workflow runs.

---

# 11. Invocation IDs

Invocation IDs must be deterministic.

Manual invocation:

```text
<function>/<uuid>
```

Example:

```text
weekly-lastfm/0198fb37-...
```

Scheduled invocation:

```text
<function>/<schedule-key>
```

Examples:

```text
weekly-lastfm/2026-W35
daily-backup/2026-08-29
hourly-sync/2026-08-29T14
```

The schedule implementation determines the key.

Invocation IDs should be:

* stable
* human-readable where possible
* unique within a function
* safe as filesystem path components after encoding

Represent internally as:

```go
type InvocationID string
```

---

# 12. Invocation state machine

A logical invocation has the following states:

```text
pending
running
succeeded
failed
exhausted
```

Potential later states:

```text
cancelled
expired
unknown
```

Primary transition:

```text
pending
   |
   v
running
  |   \
  |    \
  v     v
success failed
          |
       retry?
       /    \
     yes     no
      |       |
      v       v
   pending  exhausted
```

A skipped duplicate workflow run does not create a new invocation state.

It increments attempt metadata if appropriate.

---

# 13. State representation

Example:

```json
{
  "schema_version": 1,
  "function": "weekly-lastfm",
  "invocation_id": "weekly-lastfm/2026-W35",
  "status": "succeeded",
  "attempts": 2,
  "created_at": "2026-08-28T13:15:00Z",
  "started_at": "2026-08-28T13:20:02Z",
  "completed_at": "2026-08-28T13:20:10Z",
  "workflow_run_id": 123456789,
  "result": {
    "exit_code": 0
  }
}
```

Go:

```go
type Invocation struct {
    SchemaVersion int              `json:"schema_version"`
    Function      string           `json:"function"`
    ID            InvocationID     `json:"invocation_id"`
    Status        InvocationStatus `json:"status"`
    Attempts      int              `json:"attempts"`

    CreatedAt   time.Time  `json:"created_at"`
    StartedAt   *time.Time `json:"started_at,omitempty"`
    CompletedAt *time.Time `json:"completed_at,omitempty"`

    WorkflowRunID int64             `json:"workflow_run_id,omitempty"`
    Result        *InvocationResult `json:"result,omitempty"`
}
```

---

# 14. State backend interface

State storage must be abstracted.

```go
type Store interface {
    Get(
        ctx context.Context,
        function string,
        id InvocationID,
    ) (*Invocation, error)

    Create(
        ctx context.Context,
        invocation Invocation,
    ) error

    CompareAndSwap(
        ctx context.Context,
        previous Invocation,
        next Invocation,
    ) error

    List(
        ctx context.Context,
        function string,
        limit int,
    ) ([]Invocation, error)
}
```

Required error types:

```go
var (
    ErrNotFound = errors.New("state not found")
    ErrConflict = errors.New("state conflict")
)
```

`CompareAndSwap` is deliberately part of the interface from the beginning.

Concurrency is not an implementation detail.

---

# 15. State backends

## v0.1

Memory backend for tests.

Potentially no durable invocation state yet.

## v0.2

Git branch backend.

Example:

```text
ghaas-state branch

functions/
├── weekly-lastfm/
│   ├── 2026-W34.json
│   └── 2026-W35.json
└── cleanup/
    └── 2026-08-28.json
```

The branch has no relationship to application source history.

Example:

```text
refs/heads/ghaas-state
```

State updates use optimistic concurrency.

Conceptually:

```text
read HEAD
   |
modify tree
   |
create commit
   |
attempt ref update
   |
   +-- success
   |
   +-- conflict --> reread and retry
```

This creates a useful primitive for coordinating multiple runners.

---

# 16. Concurrency

Manifest:

```yaml
concurrency:
  max: 1
```

For v0.1, this maps to GitHub Actions concurrency groups.

Generated workflow:

```yaml
concurrency:
  group: ghaas-weekly-lastfm
  cancel-in-progress: false
```

However, GitHub concurrency groups are not sufficient for exactly-once execution.

State-level deduplication remains necessary.

This distinction must be documented.

---

# 17. Scheduling v0.1

Standard cron:

```yaml
schedule:
  cron: "15 9 * * 5"
  timezone: America/New_York
```

This generates a GitHub Actions scheduled workflow.

Manual dispatch should always also be enabled.

---

# 18. Scheduling v0.2: execution windows

The more interesting abstraction:

```yaml
schedule:
  target: "09:15 Friday"
  timezone: America/New_York

  retry:
    every: 5m
    until: "19:00"
```

Meaning:

```text
Execute this logical invocation successfully at least once
after 09:15 and before 19:00.
```

`ghaas` may implement this by generating frequent GitHub scheduled runs.

Each runner derives the same logical invocation ID.

Example:

```text
weekly-lastfm/2026-W35
```

Runners that observe a completed invocation exit immediately.

---

# 19. Failure semantics

`ghaas` should explicitly document its guarantees.

Initial goal:

```text
at-least-once attempt semantics
+
logical invocation deduplication
```

This does NOT imply exactly-once external effects.

Consider:

```text
1. function sends Discord message
2. process crashes
3. ghaas never records success
4. invocation retries
5. Discord message is sent again
```

The system cannot generically prevent this.

The correct API is to expose an idempotency key:

```text
GHAAS_INVOCATION_ID=weekly-lastfm/2026-W35
```

Functions may use that ID when interacting with systems supporting idempotency.

This limitation should be a major part of the project's documentation.

---

# 20. Environment exposed to functions

Every invocation receives:

```text
GHAAS_FUNCTION
GHAAS_INVOCATION_ID
GHAAS_ATTEMPT
GHAAS_TRIGGER
GHAAS_WORKFLOW_RUN_ID
```

Example:

```text
GHAAS_FUNCTION=weekly-lastfm
GHAAS_INVOCATION_ID=weekly-lastfm/2026-W35
GHAAS_ATTEMPT=2
GHAAS_TRIGGER=schedule
GHAAS_WORKFLOW_RUN_ID=123456
```

This lets applications implement idempotency or tracing.

---

# 21. Retries

v0.2 manifest:

```yaml
retry:
  max_attempts: 3
  backoff: 5m
```

Scheduled execution windows are related but separate.

Execution retry:

```text
function executed
function failed
run again
```

Scheduling retry:

```text
runner did not execute logical invocation yet
try another workflow run
```

These should remain distinct concepts.

---

# 22. Leases

v0.3 introduces invocation leases.

State:

```json
{
  "status": "running",
  "lease": {
    "owner": "workflow-run-123",
    "expires_at": "2026-08-28T13:40:00Z"
  }
}
```

Runner algorithm:

```text
load invocation
      |
      v
completed? -------- yes ------> exit
      |
      no
      |
      v
active lease? ------ yes ------> exit
      |
      no
      |
      v
CAS acquire lease
      |
      +-- conflict --> retry
      |
      v
execute function
```

If a runner dies, the lease eventually expires.

Another runner may recover the invocation.

---

# 23. Effects and exactly-once behavior

`ghaas` must never claim generic exactly-once execution.

There are at least three distinct concepts:

```text
exactly-once logical completion
exactly-once process execution
exactly-once external effects
```

The first may be approximated with durable state.

The second cannot always be guaranteed.

The third requires cooperation from the external system.

Document examples.

If an external API supports idempotency keys:

```text
Idempotency-Key: ${GHAAS_INVOCATION_ID}
```

the function may achieve effectively-once behavior for that effect.

---

# 24. Dead-letter behavior

v0.3:

```yaml
retry:
  max_attempts: 4

on_exhausted:
  issue: true
```

If an invocation exhausts retries, `ghaas` may create:

```text
[ghaas] weekly-lastfm/2026-W35 exhausted retries
```

Issue body:

```text
Function: weekly-lastfm
Invocation: weekly-lastfm/2026-W35
Attempts: 4
Last workflow: ...
Last exit code: 1
```

This is intentionally both useful and somewhat absurd.

---

# 25. Workflow generation

Generated workflows should be deterministic.

Given identical:

```text
ghaas version
manifest
repository configuration
```

the generated YAML should be byte-for-byte identical.

Generated workflows should contain:

```yaml
# Code generated by ghaas. DO NOT EDIT.
```

Generation should be handled through Go templates or structured YAML generation.

Prefer structured generation over large handwritten templates once the format becomes complex.

---

# 26. Generated workflow shape

Conceptual output:

```yaml
name: ghaas: weekly-lastfm

on:
  schedule:
    - cron: "15 9 * * 5"
  workflow_dispatch:

permissions:
  contents: write

concurrency:
  group: ghaas-weekly-lastfm
  cancel-in-progress: false

jobs:
  invoke:
    runs-on: ubuntu-latest
    timeout-minutes: 20

    steps:
      - uses: actions/checkout@v4

      - name: Install ghaas
        ...

      - name: Invoke function
        env:
          GHAAS_FUNCTION: weekly-lastfm
          DISCORD_TOKEN: ${{ secrets.DISCORD_TOKEN }}
        run: ghaas runtime invoke weekly-lastfm
```

The generated workflow should remain intentionally thin.

Business logic belongs inside the Go runtime.

---

# 27. Internal runtime command

The public CLI and workflow runtime may share one binary.

Internal command:

```bash
ghaas runtime invoke weekly-lastfm
```

This command:

1. loads manifest
2. determines trigger
3. determines logical invocation ID
4. loads state
5. decides whether invocation should execute
6. acquires lease if required
7. executes command
8. records result
9. exits with appropriate status

This command does not need to be advertised prominently as public API.

---

# 28. Command execution

Use `os/exec`.

Important behavior:

* preserve argument boundaries
* inherit stdout/stderr
* propagate process exit code
* support context cancellation
* terminate child when timeout occurs
* eventually forward termination signals

Example:

```go
cmd := exec.CommandContext(ctx, args[0], args[1:]...)
cmd.Stdout = os.Stdout
cmd.Stderr = os.Stderr
cmd.Stdin = os.Stdin
```

Avoid invoking commands through:

```text
sh -c
```

unless explicitly requested.

---

# 29. Secrets

Manifest:

```yaml
secrets:
  - DISCORD_TOKEN
  - LASTFM_API_KEY
```

Generated workflow:

```yaml
env:
  DISCORD_TOKEN: ${{ secrets.DISCORD_TOKEN }}
  LASTFM_API_KEY: ${{ secrets.LASTFM_API_KEY }}
```

`ghaas` must:

* never print secret values
* never store secret values in invocation state
* never include them in generated metadata
* avoid dumping complete environments during debugging

The CLI may validate that secret names are syntactically valid, but cannot necessarily verify their existence without GitHub API permissions.

---

# 30. Observability

v0.1 uses GitHub workflow logs.

v0.2 introduces semantic status.

v0.4 introduces structured metrics.

Metrics should describe logical invocations rather than raw workflow runs.

Examples:

```text
ghaas_invocations_total
ghaas_invocation_failures_total
ghaas_invocation_duration_seconds
ghaas_invocation_attempts
ghaas_schedule_delay_seconds
ghaas_duplicate_runner_total
ghaas_lease_recovery_total
```

Dimensions:

```text
function
trigger
status
```

Avoid high-cardinality invocation IDs as metric labels.

---

# 31. OpenTelemetry

v0.4 may emit one root span per logical invocation:

```text
ghaas.invoke
```

Attributes:

```text
ghaas.function
ghaas.invocation.id
ghaas.trigger
ghaas.attempt
github.workflow_run_id
```

Child span:

```text
ghaas.function.execute
```

Potential state operations:

```text
ghaas.state.read
ghaas.state.cas
ghaas.state.commit
```

Tracing backend should be optional.

---

# 32. SLOs

Optional manifest:

```yaml
slo:
  success_rate: 99.9
  schedule_delay: 15m
```

Meaning:

```text
99.9% of logical invocations succeed.

99.9% of scheduled logical invocations begin within
15 minutes of their target time.
```

`ghaas status` may eventually report:

```text
SLO: successful invocations

Target:       99.9%
Observed:     99.4%
Window:       30d
Error budget: exhausted
```

Do not overbuild this in the first release.

---

# 33. Failure model documentation

The repository should contain:

```text
docs/failure-model.md
```

It should explicitly examine:

* delayed workflow scheduling
* dropped scheduled runs
* duplicate workflow execution
* concurrent runners
* runner termination
* process crash
* GitHub API failure
* Git state conflicts
* state corruption
* network failure during state commit
* effect succeeds but state commit fails
* state commit succeeds but workflow appears failed

This document is part of the project, not ancillary documentation.

---

# 34. Testing strategy

## Unit tests

Test:

* manifest parsing
* validation
* invocation ID generation
* state transitions
* retry decisions
* lease expiration
* CAS conflict handling
* workflow generation

Golden tests are appropriate for generated workflow YAML.

Example:

```text
testdata/
  basic.input.yaml
  basic.golden.yml
```

---

## Integration tests

Test state backend using temporary Git repositories.

Cases:

```text
create invocation
read invocation
CAS succeeds
CAS conflict
concurrent update
expired lease recovery
```

No GitHub API required.

---

## End-to-end tests

Create a dedicated test workflow that runs a trivial function.

Potential test function:

```bash
echo "$GHAAS_INVOCATION_ID"
```

Do not make every CI run depend on real scheduled GitHub execution.

---

# 35. Logging

The runtime should use structured logging internally.

Use Go's `log/slog`.

Example:

```go
logger.Info(
    "invocation started",
    "function", function,
    "invocation_id", invocationID,
    "attempt", attempt,
)
```

Human-facing CLI output should remain concise.

Support:

```text
--json
```

for machine-readable CLI output later.

---

# 36. Error handling

Errors should preserve semantic context.

Prefer:

```go
return fmt.Errorf("acquire invocation lease: %w", err)
```

Avoid opaque strings.

Define typed errors where caller behavior differs:

```go
ErrStateConflict
ErrInvocationComplete
ErrLeaseHeld
ErrFunctionNotFound
```

Do not use typed errors merely to classify every failure.

---

# 37. GitHub API abstraction

Do not scatter GitHub API calls throughout the application.

Define an interface such as:

```go
type GitHub interface {
    DispatchWorkflow(
        ctx context.Context,
        workflow string,
        ref string,
        inputs map[string]string,
    ) error

    ListWorkflowRuns(
        ctx context.Context,
        workflow string,
        limit int,
    ) ([]WorkflowRun, error)

    GetWorkflowLogs(
        ctx context.Context,
        runID int64,
    ) (io.ReadCloser, error)
}
```

Implementation may use:

* `go-github`
* direct REST calls

Either is acceptable.

Favor the smallest dependency surface initially.

---

# 38. Authentication

Local CLI:

```text
GITHUB_TOKEN
```

or GitHub CLI integration later.

Generated workflow uses:

```text
GITHUB_TOKEN
```

with minimum necessary permissions.

State backend permissions should be explicit.

For branch-backed state:

```yaml
permissions:
  contents: write
```

Document the security implications.

---

# 39. Security model

Initial security model is repository-scoped.

Trust assumptions:

* repository maintainers control function definitions
* generated workflows are trusted
* GitHub-hosted runners are trusted execution environments
* repository secrets are trusted inputs
* the state branch is trusted persistence

Potential threats:

* command injection through generated workflow construction
* secrets appearing in logs
* malicious manifest values
* branch manipulation
* compromised dependencies
* unsafe shell interpolation

Avoid shell interpolation wherever possible.

---

# 40. Version roadmap

## v0.1 — "yes, technically serverless"

Features:

* Go CLI
* manifest parser
* manifest validation
* deterministic workflow generation
* `ghaas deploy`
* `ghaas generate`
* `ghaas invoke`
* `ghaas status`
* scheduled functions
* manual invocation
* command runtime
* secrets
* timeout
* GitHub concurrency groups

Success criterion:

A real application such as the Last.fm Discord updater can be deployed entirely through `ghaas`.

---

## v0.2 — logical invocations

Features:

* logical invocation IDs
* scheduled retry windows
* duplicate suppression
* invocation state
* Git branch backend
* optimistic concurrency
* invocation history
* improved status output

Success criterion:

Multiple GitHub workflow runs can implement one user-visible logical invocation.

This is the release where `ghaas` becomes more than workflow generation.

---

## v0.3 — failure recovery

Features:

* leases
* stale invocation recovery
* execution retries
* idempotency key exposure
* exhausted invocation state
* optional dead-letter GitHub issues
* explicit documented execution guarantees

Success criterion:

The project can meaningfully explain and recover from runner death and competing executions.

---

## v0.4 — reliability

Features:

* semantic metrics
* schedule-delay measurement
* OpenTelemetry traces
* SLO configuration
* reliability status
* failure injection tests

Success criterion:

`ghaas` can measure the reliability of the abstraction it provides rather than merely execute functions.

---

# 41. Example demo

A good demo should deliberately show failure.

Function:

```text
weekly-lastfm
```

Schedule target:

```text
Friday 09:15 America/New_York
```

Execution window:

```text
09:15–19:00
```

Runner sequence:

```text
09:15 attempt #1
      simulated failure

09:20 attempt #2
      acquires invocation
      sends message
      records success

09:25 attempt #3
      detects completed logical invocation
      exits successfully without sending duplicate
```

Then:

```bash
ghaas status weekly-lastfm
```

shows:

```text
Invocation: weekly-lastfm/2026-W35

Status:   succeeded
Attempts: 2
Target:   09:15
Started:  09:20
Delay:    5m
Duration: 8.3s
```

The demo should show the abstraction surviving unreliable execution beneath it.

---

# 42. Design principles

## Semantic objects over infrastructure events

Users reason about functions and logical invocations.

They should not need to reason directly about GitHub workflow runs.

## Explicit guarantees

Do not claim stronger execution semantics than the substrate permits.

## Thin generated workflows

Keep semantics in Go.

Do not implement the runtime as 400 lines of generated Bash and YAML.

## Deterministic generation

Generated infrastructure should be reproducible and inspectable.

## Failure is part of the API

Retries, duplicates, crashes, partial effects, and delayed scheduling are first-class design concerns.

## Minimal abstraction

Do not recreate AWS Lambda.

Build only what GitHub Actions naturally supports.

## The substrate should remain visible

`ghaas` is not trying to convince users that GitHub Actions is actually a good FaaS.

The tension between the abstraction and the substrate is part of the project.

---

# 43. README opening

Suggested tone:

# ghaas

GitHub Actions as a Service.

An extremely questionable serverless platform.

```yaml
functions:
  hello:
    runtime: command
    command: ["./hello"]

    schedule:
      cron: "0 9 * * *"
```

```bash
$ ghaas deploy
✓ deployed hello

$ ghaas invoke hello
✓ dispatched invocation

$ ghaas status hello
last invocation succeeded in 1.2s
```

GitHub Actions already gives us ephemeral compute, scheduling, secrets, logs, timeouts, and an API.

Clearly the only remaining problem was insufficient abstraction.

`ghaas` turns workflow runs into logical function invocations and explores how much serverless-style reliability can be constructed on top of an execution environment that was never intended to provide it.

This project is partly a joke.

The failure model is not.
