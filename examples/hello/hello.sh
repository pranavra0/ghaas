#!/usr/bin/env bash
set -euo pipefail

printf '%s\n' "${GREETING:-hello}"
printf 'function=%s\n' "${GHAAS_FUNCTION:-local}"
printf 'invocation=%s\n' "${GHAAS_INVOCATION_ID:-none}"
printf 'attempt=%s trigger=%s run=%s\n' \
  "${GHAAS_ATTEMPT:-0}" \
  "${GHAAS_TRIGGER:-local}" \
  "${GHAAS_WORKFLOW_RUN_ID:-none}"
