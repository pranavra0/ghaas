# Security model

The primary security boundary is the GitHub repository. Anyone who can change the
manifest, command, installer, or generated workflow can change what a function executes.
Treat ghaas configuration as code, review it like code, and protect branches that contain
it.

## Trust assumptions

The default design assumes:

- repository maintainers control function definitions, scripts, and generated workflows;
- GitHub-hosted runners and the Actions service are the execution environment;
- repository/environment secrets are trusted inputs;
- the local state directory is trusted persistence only for the lifetime and access scope
  of its checkout; and
- dependencies fetched by the command and install step are acceptable.

This is not a sandbox for untrusted tenants. A command runtime intentionally has the
runner's normal process capabilities and network access.

## Threats and controls

### Command and workflow injection

`command` is an argv array. The runtime passes each element directly to the OS process
API and does not join it into a shell string. This preserves argument boundaries and
avoids accidental shell interpretation. Keep user-controlled data out of an explicit
shell command. If shell behavior is necessary, make it visible, for example
`["sh", "-c", "./script --fixed-flag"]`, and validate/quote all dynamic data inside
that script.

Generated workflow values are emitted as YAML data, not as a function command fragment.
The function command is loaded from the checked-out manifest by `ghaas runtime invoke`.
Do not hand-edit generated files to splice untrusted values into `run:`. `ghaas deploy`
rejects unsafe generated paths and existing symlink path components when writing
workflows; still review generated output before committing it.

### Manifest, installer, and repository changes

Manifest names, environment names, secret names, durations, cron, timezone, state,
retry, and runtime are strictly validated. Validation is not authorization: a maintainer
can still choose a command that exfiltrates data. Require pull requests/review for
manifest, script, installer, and workflow changes, pin action versions where policy
requires it, and use protected branches for deployed workflows.

The default generated install step is
`go install ./cmd/ghaas && echo "$(go env GOPATH)/bin" >> "$GITHUB_PATH"`,
followed by `ghaas runtime invoke <function>`. The checked-out repository must contain
the ghaas source and Go module (or use a reviewed installer override), the runner must
have Go 1.23+, dependencies must be fetchable, and `GITHUB_PATH` must be supported.

### Secret exposure

A manifest `secrets` entry contains only a GitHub secret **name**. The generated workflow
references `${{ secrets.NAME }}`; the value is supplied by GitHub at run time. ghaas does
not validate the secret's existence locally, and state records never store secret values
or a complete environment.

The generated invocation environment also wires the run-scoped `${{ secrets.GITHUB_TOKEN }}`
when the function does not already define `GITHUB_TOKEN`; this is separate from the
developer's local token and is subject to the workflow `permissions` block.

Do not:

- put secret values in `ghaas.yaml`, command arguments, source, or generated YAML;
- print the environment or request/response URLs containing credentials;
- enable shell tracing (`set -x`) in a function that handles secrets;
- include secrets in invocation IDs, issue bodies, state, or debug output; or
- pass secrets to a function that does not need them.

GitHub may mask a configured secret in logs, but masking is not a guarantee against
transformation, truncation, subprocess leaks, or exfiltration. Treat logs as sensitive
and grant only the people/apps that need to read them.

### State and lease manipulation

The memory backend is process-local. The built-in branch backend is a JSON file store
under the checkout's `.ghaas-state/<branch>` namespace, with a
`functions/<function>/` subtree; it uses validation, path namespacing, optimistic CAS,
and atomic updates, but does not commit or push to a remote GitHub branch. A user or
process with write access to that checkout can alter state. A remote Store supplied by an
embedding application must protect its state branch, reject malformed records, and use
optimistic ref updates rather than force-pushing stale data.

Leases reduce concurrent execution but do not sandbox a command or roll back an external
effect. A runner that retains a lease past a network partition may still act after another
runner recovers the expired lease. Use external idempotency keys for side effects.

### Logs and metadata

Workflow logs are a data-exfiltration surface. Invocation state intentionally stores
metadata (function, ID, attempt, timestamps, trigger, workflow run ID, lease, exit code,
and error), not stdout/stderr or secrets. Avoid putting credentials in error strings.
Apply the repository's log retention and access policy.

## API token and workflow permissions

Local API commands use `GITHUB_TOKEN` and discover the repository from
`GITHUB_REPOSITORY=OWNER/NAME` or the Git `origin` remote. Set the token before commands
that call GitHub:

```bash
export GITHUB_REPOSITORY=OWNER/NAME   # optional inside a Git checkout
export GITHUB_TOKEN=...                # never commit or print this value
```

A fine-grained token should be scoped to the target repository:

| Operation | Required repository permission |
| --- | --- |
| `validate`, `generate`, local `deploy` | none |
| `invoke` (workflow dispatch) | Actions: write (GitHub may also require read) |
| `status` (list runs) | Actions: read |
| `logs` (download logs) | Actions: read |
| local/remote state operations, if enabled by an embedding service | repository-specific contents access |
| exhausted-invocation issue | Issues: write |
| checkout in generated workflow | Contents: read |

The token also needs ordinary repository visibility. A classic token generally needs
`repo` for a private repository and corresponding Actions access. Organization policy,
SSO, private-repository visibility, Actions policy, and fine-grained token approval can
impose additional requirements. Keep local tokens out of shell history and CI logs.
Prefer a short-lived token with only the repository and permissions needed for the
operation.

The generated workflow receives a separate, run-scoped GitHub-provided `GITHUB_TOKEN`;
it is not the local developer token. The compiler emits `contents: read` for checkout by
default, upgrades to `contents: write` for the branch state backend, and adds
`issues: write` when exhausted-issue handling is configured. Inspect generated YAML and
reduce permissions when no corresponding operation is needed. A contents-write
permission is not harmless merely because the default state backend is local.

## Operational checklist

Before deployment:

1. Review `ghaas generate` output, including the installer, `permissions`, and command.
2. Run `ghaas validate` and `ghaas deploy --check` in CI.
3. Protect the manifest, scripts, installer, and generated workflow paths with code review.
4. Configure only the named repository secrets; verify they are available to the selected
   branch/environment.
5. Confirm local token scope: Actions write only when dispatching; Actions read for
   queries; Issues write only when exhaustion notifications are required.
6. Confirm workflow `permissions` is no broader than the operation requires.
7. Test timeout, retry, lease recovery, and failure behavior without real irreversible
   side effects.
8. Make external operations idempotent with `GHAAS_INVOCATION_ID` where possible.
9. Set an appropriate log-retention and access policy.
10. Rotate tokens and secrets after suspected exposure.

No permission setting can provide exactly-once external effects. A runner can crash
 after an API accepted a request but before ghaas records success; lease recovery or
retry can repeat it. The destination must provide idempotency or the application must
provide its own deduplication.
