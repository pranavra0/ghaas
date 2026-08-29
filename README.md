# ghaas

GitHub Actions as a Service.

An extremely serious serverless platform

```yaml
version: 1

functions:
  hello:
    runtime: command
    command: ["echo", "hello"]
    schedule:
      cron: "0 9 * * *"
      timezone: UTC
```

```bash
ghaas validate
ghaas generate hello
ghaas deploy
ghaas invoke hello
ghaas status hello
ghaas logs hello
```

`ghaas` compiles command-backed functions into deterministic GitHub Actions workflows. GitHub supplies ephemeral compute, scheduling, secrets, logs, timeouts, and an API; `ghaas` supplies a small function abstraction over those primitives.