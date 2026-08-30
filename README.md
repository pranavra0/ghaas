# ghaas

GitHub Actions as a Service.

An extremely serious serverless platform.

## a tour

In a target repository containing `ghaas.yaml` and `examples/hello/hello.sh`:

```yaml
version: 1

functions:
  hello:
    runtime: command
    command: [bash, examples/hello/hello.sh]
```

```console
$ ghaas deploy
✓ hello  deployed
  .github/workflows/ghaas-hello.yml

$ ghaas invoke hello
◆ hello  dispatched
  hello/...

$ ghaas status hello
hello
✓ succeeded in 1.2s
```

Install the released CLI using the [verified binary instructions](#install), then
commit the generated workflow before invoking:

```console
$ ghaas validate
$ ghaas deploy
$ git add ghaas.yaml .github/workflows/ghaas-hello.yml
$ git commit -m 'Add hello function' && git push
$ ghaas invoke hello
$ ghaas status hello
```

`deploy` writes generated files; it does not push them. Local `invoke` first creates
durable pending state and then dispatches, so its `GITHUB_TOKEN` needs both
`Contents: write` and `Actions: write`. State-first `status` and `logs` use the durable
aggregate as their source of truth; `logs` then reads provider data, so their token
needs `Contents: read` and `Actions: read`. Set `GITHUB_REPOSITORY=OWNER/REPOSITORY`
when the target cannot be inferred from `origin`.

Development builds report `dev` and require an explicit released installer version:

```console
$ GHAAS_RUNTIME_VERSION=v0.1.0 /tmp/ghaas generate hello
```

The variable must be a released `v`-prefixed SemVer; absent or invalid values fail
with the compiler error. Tagged release builds ignore it and use their embedded tag.

## Install

Install the released CLI from the Linux or macOS archive. Replace `version` with
the exact `vX.Y.Z` release tag you intend to run; generated workflows pin that
same tag and verify its checksum before execution:

```bash
set -eu
version=vX.Y.Z
case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "unsupported operating system" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) echo "unsupported architecture" >&2; exit 1 ;;
esac
archive="ghaas-${version}-${os}-${arch}.tar.gz"
base_url="https://github.com/pranavra0/ghaas/releases/download/${version}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl --fail --location --silent --show-error --retry 3 \
  --output "$tmp/$archive" "$base_url/$archive"
curl --fail --location --silent --show-error --retry 3 \
  --output "$tmp/SHA256SUMS" "$base_url/SHA256SUMS"
expected="$(awk -v archive="$archive" '$2 == archive { print $1; exit }' "$tmp/SHA256SUMS")"
test -n "$expected"
if command -v sha256sum >/dev/null 2>&1; then
  (cd "$tmp" && printf '%s  %s\n' "$expected" "$archive" | sha256sum --check --status)
else
  test "$expected" = "$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')"
fi
tar --extract --gzip --file "$tmp/$archive" --directory "$tmp"
mkdir -p "$HOME/.local/bin"
cp "$tmp/ghaas" "$HOME/.local/bin/ghaas"
chmod 755 "$HOME/.local/bin/ghaas"
export PATH="$HOME/.local/bin:$PATH"
ghaas --version
```

For Windows, download the matching `ghaas-${version}-windows-amd64.zip` and
`SHA256SUMS`, verify the archive with `Get-FileHash -Algorithm SHA256`, then
extract `ghaas.exe`.

## Manifest

`ghaas.yaml` is strict YAML. It must contain `version: 1` and at least one function.
Unknown fields and multiple documents are rejected.

- `defaults.timeout`: positive duration inherited by functions without `timeout`; absent
  values use the 15-minute runtime default.
- `functions.<name>.runtime`: the scalar `command` only.
- `command`: non-empty argv with preserved argument boundaries; no implicit shell.
- `timeout`: positive duration for one command and the generated job timeout.
- `env`: literal environment values; names must be valid identifiers.
- `secrets`: GitHub secret names, never secret values.
- `schedule`: optional; when present, `cron` is a validated five-field expression and
  `timezone` is a required validated IANA timezone.
- `concurrency.max`: omitted or `1`; no other value is supported.
- `retry.max_attempts`: optional positive integer, default `1`.
- `retry.backoff`: optional positive duration between failed attempts, default `0`.

An environment name cannot occur in both `env` and `secrets`, and `GHAAS_*` names are
reserved for ghaas metadata. Environment precedence is ambient, manifest `env`, then
GHAAS-owned metadata. Secret values are supplied by GitHub only at run time.

## Commands

Run commands from the directory containing `ghaas.yaml`:

| Command | Purpose |
| --- | --- |
| `init [--force]` | Create a starter manifest; refuse to overwrite by default. |
| `validate` | Parse and validate the manifest without contacting GitHub. |
| `invoke [--ref REF] FUNCTION` | Create a durable pending invocation, then dispatch it; omitted ref means the repository default branch. |
| `status FUNCTION [--invocation ID] [--json]` | Show durable state first; explicit IDs are matched exactly. |
| `logs FUNCTION [--invocation ID]` | Read logs for the exact state-bound provider run/attempt, or a selected run. |
| `version` / `--version` / `-v` | Print the exact release tag, or `dev` for an unreleased binary. |
| `completion bash\|zsh\|fish` | Print shell completion from command metadata. |
| `runtime invoke FUNCTION` | Hidden workflow entrypoint; execute the selected argv command. |

`-h` and `--help` are accepted where a command has help. Output is concise plain text;
`NO_COLOR` and noninteractive execution do not add banners or spinners. `status --json`
uses a stable, newline-terminated JSON object suitable for scripts.

Generated workflows begin with `# Code generated by ghaas. DO NOT EDIT.`. They contain
checkout, `contents: write` for durable state, the pinned released ghaas archive
installer with SHA256 verification, manifest environment and secrets, and
`ghaas runtime invoke <function>`. Schedule, timezone, timeout, retry, and
`concurrency.max: 1` are expressed in workflow YAML. State is one aggregate
`.ghaas/state/v1.json` on `refs/heads/ghaas-state-v1`; there is no issue or
dead-letter write.

Generated run names and exact invocation matching are documented in
[semantics](docs/semantics.md). `--ref` wins when supplied to `invoke`; otherwise
dispatch resolves the repository default branch.

## Invocation environment and semantics

The runtime supplies `GHAAS_FUNCTION`, logical `GHAAS_INVOCATION_ID`,
logical `GHAAS_ATTEMPT`, `GHAAS_TRIGGER` (`manual` or `schedule`),
`GHAAS_WORKFLOW_RUN_ID`, `GHAAS_WORKFLOW_RUN_ATTEMPT`, and
`GHAAS_ATTEMPT_REASON`. A manual invocation uses `<function>/<uuid>`; the CLI
passes only its UUID as the dispatch input. A scheduled run uses
`<function>/<positive-GITHUB_RUN_ID>`. Schedules have no intended timestamp,
catch-up, or timestamp-based dedupe semantics.

These values correlate a command with a run; they do not make side effects exactly
once. Use `GHAAS_INVOCATION_ID` as an idempotency key with a destination API that
supports one.

Commands execute directly as argv with inherited stdout and stderr. Use `bash -c` or
`sh -c` explicitly for shell behavior and treat its string as code. A non-zero exit,
cancellation, or timeout makes the command unsuccessful; GitHub may cancel or lose a run
before a final result is visible.

## GitHub setup and security

Local `invoke`, `status`, and `logs` call the GitHub API. Set a token without committing
or printing it:

```bash
export GITHUB_TOKEN=...
export GITHUB_REPOSITORY=OWNER/REPOSITORY  # optional in a Git checkout
```
Use the narrowest token that fits: local `invoke` needs `Contents: write` and
`Actions: write`; state-first `status` and `logs` need `Contents: read` and
`Actions: read` (plus ordinary repository visibility). A direct provider-run
`logs --invocation RUN_ID` lookup needs Actions read. The generated workflow gets a
separate run-scoped `GITHUB_TOKEN` with `contents: write` because it commits only
the durable state aggregate to `refs/heads/ghaas-state-v1`.


A repository maintainer can change the manifest, scripts, workflow, or installer and
therefore what runs. Review those changes and protect the deployment branch. The
configured command is trusted repository code, not an untrusted tenant: the runtime
strips `GITHUB_TOKEN`, `GH_TOKEN`, and `GHAAS_STATE_*` credentials before starting
the command, while declared secrets are intentionally supplied to it. The runtime
still has normal runner permissions and network access; it is not a multi-tenant
sandbox. See [security.md](docs/security.md) for the complete boundary.

## Examples and target repositories

Examples are source-controlled fixtures, not a local durable execution environment. To use
hello elsewhere, copy the manifest and `examples/hello/hello.sh` to the target, then deploy:

```bash
cp /path/to/ghaas/examples/hello/ghaas.yaml ./ghaas.yaml
mkdir -p examples/hello
cp /path/to/ghaas/examples/hello/hello.sh examples/hello/hello.sh
ghaas validate
ghaas deploy --function hello
```

`examples/weekly-lastfm` shows a scheduled command using `LASTFM_API_KEY` and
`DISCORD_WEBHOOK_URL`; configure those as target-repository secrets and review the script.

## Limitations

GitHub Actions can delay, drop, duplicate, rerun, or cancel workflows. ghaas provides no
exactly-once execution or external effects, no queue, and no catch-up scheduler. A
schedule timezone is provider input and metadata, not a start-time SLA. Schedule records
use `<function>/<positive-GITHUB_RUN_ID>` and have no intended timestamp or inferred
deduplication behavior.

Durable state is production-only GitHub Git Data CAS on
`refs/heads/ghaas-state-v1`, with aggregate `.ghaas/state/v1.json` and schema `1`.
Records use statuses `pending`, `running`, `succeeded`, `failed`, or `exhausted`;
retries retain the same ID and advance attempts monotonically. Leases fence stale
writers. This is coordination state, not a transactional database or exactly-once
external-effects guarantee. There is no dead-letter, issue, SLO, or execution-window
feature in manifest version 1.

Read [semantics](docs/semantics.md), [failure model](docs/failure-model.md), [state boundary](docs/state.md), and [security model](docs/security.md) before using external side effects in production. Development and CI conventions are in `.github/workflows/ci.yml`.
