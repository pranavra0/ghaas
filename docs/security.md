# v1 security model

`ghaas` executes the configured command on a GitHub-hosted runner. It is not a
sandbox or multi-tenant boundary: a repository maintainer can change the
manifest, scripts, generated workflow, or release reference and therefore what
runs. Protect the deployment branch and review generated YAML.

## Commands and data

The runtime executes `command` as an argv array. It does not join arguments into
a shell string or add shell parsing, quoting, globbing, or interpolation. If
shell behavior is intended, make it explicit (`command: [sh, -c, ...]`) and keep
user-controlled values out of the script string.

`env` contains literal values and `secrets` contains only GitHub secret names.
Secret values are supplied by GitHub at run time and are never read by local
manifest validation. Environment names cannot occur in both collections, and
`GHAAS_*` names are reserved for ghaas-owned metadata. Metadata is applied after
manifest environment values; ambient values cannot override it.

## Installer and actions

Released generated workflows pin an exact v-prefixed ghaas release. The installer
uses bounded HTTP retries, downloads the matching archive and `SHA256SUMS`,
requires a valid SHA-256 entry, verifies it before extraction, and writes only a
runner-temp directory to `GITHUB_PATH`. All GitHub Action references are immutable
commit pins. Development binaries identify as `dev` and refuse to generate a
workflow with an implicit installer pin.

## Permissions and state

The generated workflow requests the least permission needed for v1:

```yaml
permissions:
  contents: write
```

That token is used to persist only the bounded state aggregate
`.ghaas/state/v1.json` on `refs/heads/ghaas-state-v1` through GitHub Git Data CAS.
It does not write issues, dead-letter records, source files, or arbitrary refs.
Lease owner, opaque token, fence, and expiry prevent stale workers from writing
newer state. State is coordination metadata, not a secret store or an exactly-once
transaction system.

Local commands use the developer token. Scope it to the target repository and
operation:

| Operation | Minimum repository permission |
| --- | --- |
| `validate`, `generate`, local `deploy` | None |
| local `invoke` | Contents: write and Actions: write (plus repository visibility) |
| state-first `status`, `logs` | Contents: read and Actions: read (plus repository visibility) |
| `logs --invocation RUN_ID` (direct provider lookup) | Actions: read (plus repository visibility) |
| generated workflow state | Contents: write on the run-scoped token |

Local `invoke` needs `Contents: write` because it creates the pending durable
record before dispatching the workflow; Actions write is needed for the dispatch.
State-first `status` reads the durable aggregate as its source of truth. State-first
`logs` reads that aggregate, then reads provider data; both operations require
Contents read and Actions read. A direct provider-run logs lookup does not select
a logical invocation record first and therefore needs only Actions read.

Set local discovery explicitly when needed, but never commit the token:

```bash
export GITHUB_TOKEN=...
export GITHUB_REPOSITORY=OWNER/REPOSITORY
```

## Trusted command and token boundary

The manifest, scripts, generated workflow, and pinned installer release are
trusted repository inputs. A maintainer who can change them can change what runs,
which secrets are named, and which external systems the command contacts. This
project is not a multi-tenant sandbox.

The local `GITHUB_TOKEN` is a bearer credential held by the CLI for GitHub API
control-plane calls; it is not forwarded by local commands to a child process. In
a generated workflow, the run-scoped `github.token` is supplied to the ghaas
runtime so it can write durable state. Before executing the configured command,
the runtime strips `GITHUB_TOKEN`, `GH_TOKEN`, and `GHAAS_STATE_*` credentials
from the child environment. Declared GitHub secrets are intentionally passed to
the command, and the command retains runner network access and other non-state
ambient environment. This boundary protects the state credential from the
configured command; it does not make that command untrusted code safe to run.


## Review checklist

1. Review the manifest, scripts, generated workflow, and pinned release tag.
2. Confirm only intended repository secrets are named and scoped.
3. Confirm local tokens have the Contents and Actions access required by the
   selected operation, and no broader access than necessary.
4. Confirm generated permissions are limited to `contents: write`.
5. Confirm state mutations stay on `ghaas-state-v1` and use CAS.
6. Test timeout, cancellation, failed retry, and unknown-dispatch paths safely.
7. Use `GHAAS_INVOCATION_ID` as a destination idempotency key where supported.
8. Apply an appropriate workflow-log retention and access policy.
