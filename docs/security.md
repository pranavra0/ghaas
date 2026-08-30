# Security model

The target repository is the primary trust boundary. Anyone who can change its manifest,
command, dependencies, installer, or generated workflow can change what runs. Review
those files as code and protect the branch that contains deployed workflows.

`ghaas` is not a sandbox or multi-tenant runtime. The command has the normal GitHub
Actions runner's process permissions and network access. A workflow can read files in its
checkout and any credentials made available to it.

## Trust and injection controls

The v0.1 runtime executes `command` as an argv array. It does not join elements into a
shell string or add shell parsing, quoting, globbing, or interpolation. If shell behavior
is intended, make it explicit, such as `command: [sh, -c, ./script]`, and keep
user-controlled values out of the script string.

Manifest decoding is strict: unsupported fields, malformed values, invalid environment
names, and multiple YAML documents are rejected. Validation is not authorization; a
maintainer can still choose a command that exfiltrates data. Generated YAML is data from
the compiler, not a safe place to splice untrusted text into a hand-written `run:` step.
Do not edit generated files; change the manifest and regenerate them.

The generated installer is pinned to the reviewed v0.1.0 GitHub release. It downloads the
selected Linux archive and `SHA256SUMS`, verifies the archive's exact SHA-256 entry before
extracting the binary, and adds only the runner-temp install directory to `GITHUB_PATH`.
The target repository need not contain ghaas source or a Go toolchain, but it does need
network access and the runner's `curl`, `sha256sum`, and `tar` tools. The release and its
checksum file remain trusted inputs; review release changes, installer scripts, and action
versions. A compromised release or changed checksum is inside the trust boundary.

## Environment and secrets

`env` contains literal values and `secrets` contains only GitHub secret names. Local
validation cannot prove that a named secret exists. An environment name cannot occur in
both collections, and `GHAAS_*` names are reserved for ghaas-owned metadata.

The effective environment order is:

1. ambient runner environment;
2. manifest `env` values; then
3. ghaas-owned `GHAAS_*` metadata.

Secret values are supplied by GitHub at run time. Never put a secret value in the
manifest, command arguments, source, generated YAML, invocation ID, or diagnostic text.
Do not print the environment, enable `set -x`, or include credential-bearing URLs in
logs. GitHub masking reduces accidental disclosure but does not prevent transformation,
subprocess leaks, or exfiltration. Treat workflow logs as sensitive.

The runtime exports `GHAAS_FUNCTION`, `GHAAS_INVOCATION_ID`, `GHAAS_ATTEMPT`,
`GHAAS_TRIGGER`, and `GHAAS_WORKFLOW_RUN_ID`. An invocation ID is useful as an external
idempotency key, not as a secret and not as proof of exactly-once effects.

## Workflow permissions

Generated workflows request only:

```yaml
permissions:
  contents: read
```

Checkout needs that read permission. v0.1 has no state branch, issue notification, or
other generated write operation. Review generated YAML and reject unexpected permission
upgrades. The workflow's run-scoped `GITHUB_TOKEN` is separate from a developer's local
`GITHUB_TOKEN`.

Local API commands use the developer token. Scope it to the target repository and the
operation:

| Operation | Minimum repository permission |
| --- | --- |
| `validate`, `generate`, local `deploy` | None |
| `invoke` | Actions: write (and visibility/read as required by GitHub) |
| `status`, `logs` | Actions: read |
| generated checkout | Contents: read, on the run-scoped token |

Set local discovery explicitly when needed, but never commit the token:

```bash
export GITHUB_TOKEN=...
export GITHUB_REPOSITORY=OWNER/REPOSITORY
```

Keep tokens out of shell history and CI output. Prefer short-lived, repository-scoped
credentials. Organization policy, SSO, environment protection, and Actions policy can
require additional approval.

## Operational checklist

Before deploying a function:

1. Run `ghaas validate` and review `ghaas generate` output.
2. Run `ghaas deploy --check` in CI so generated files cannot drift.
3. Require review for the manifest, scripts, dependencies, installer, and workflows.
4. Configure only the named target-repository secrets and verify their environment scope.
5. Confirm the local token has only the Actions access needed by the command.
6. Confirm the generated workflow still has `contents: read` and no write permissions.
7. Test timeout, cancellation, and failure paths without irreversible effects.
8. Use `GHAAS_INVOCATION_ID` for destination idempotency where supported.
9. Apply an appropriate workflow-log retention and access policy.
10. Rotate credentials after suspected exposure.

No permission setting makes external effects exactly once. A runner can disappear after
a destination accepts a request, and a rerun can repeat it. The destination or command
must provide idempotency or deduplication.

## Deferred work

Durable state, retries, leases, remote branch persistence, execution windows, SLOs, and
additional isolation are roadmap items. They are not hidden security guarantees or v0.1
manifest fields.
