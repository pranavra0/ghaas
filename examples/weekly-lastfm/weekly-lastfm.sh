#!/usr/bin/env bash
# Requires curl and jq (both are available on the standard Ubuntu runner).
# Configure before invoking:
#   gh secret set LASTFM_API_KEY
#   gh secret set DISCORD_WEBHOOK_URL
# Set LASTFM_USERNAME in ghaas.yaml or replace it with a repository variable.
set -euo pipefail

: "${LASTFM_USERNAME:?set LASTFM_USERNAME in ghaas.yaml}"
: "${LASTFM_API_KEY:?configure the LASTFM_API_KEY repository secret}"
: "${DISCORD_WEBHOOK_URL:?configure the DISCORD_WEBHOOK_URL repository secret}"

# Keep the response in memory so credentials never appear in a logged command.
response="$(curl --fail --silent --show-error --get \
  'https://ws.audioscrobbler.com/2.0/' \
  --data-urlencode 'method=user.getrecenttracks' \
  --data-urlencode "user=${LASTFM_USERNAME}" \
  --data-urlencode "api_key=${LASTFM_API_KEY}" \
  --data-urlencode 'format=json')"

if jq -e '.error? != null' >/dev/null <<<"${response}"; then
  printf '%s\n' 'Last.fm returned an API error' >&2
  exit 1
fi

track="$(jq -er '.recenttracks.track[0] // empty' <<<"${response}")" || {
  printf 'No recent Last.fm track for %s\n' "${LASTFM_USERNAME}"
  exit 0
}
artist="$(jq -r '.artist["#text"] // .artist.name // "unknown artist"' <<<"${track}")"
title="$(jq -r '.name // "unknown track"' <<<"${track}")"
url="$(jq -r '.url // empty' <<<"${track}")"
now_playing="$(jq -r 'if ."@attr".nowplaying == "true" then " (now playing)" else "" end' <<<"${track}")"

message="${artist} — ${title}${now_playing}"
if [[ -n "${url}" ]]; then
  message+=$'\n'"${url}"
fi

# jq performs JSON escaping; the webhook URL is never printed.
payload="$(jq -cn --arg content "${message}" '{content: $content}')"
curl --fail --silent --show-error \
  -H 'Content-Type: application/json' \
  --data "${payload}" \
  "${DISCORD_WEBHOOK_URL}" >/dev/null

printf 'Published Last.fm update for %s (invocation %s)\n' \
  "${LASTFM_USERNAME}" "${GHAAS_INVOCATION_ID:-local}"
