#!/usr/bin/env bash
# Checks a running TapirPost from the outside: creates a content type, writes
# an entry, publishes it, reads it the way a website would, and removes the
# type again. Needs an admin token.
#
#   deploy/smoke.sh http://localhost:8080 "$TOKEN"
set -euo pipefail

usage="usage: smoke.sh BASE_URL ADMIN_TOKEN"
base=${1:?$usage}
token=${2:?$usage}
type="smoke_$(date +%s)_$$"

# call METHOD PATH [BODY] prints the response and fails on a status of 400 or above.
call() {
  local args=(-fsS -X "$1" "$base$2" -H "Authorization: Bearer $token")
  if [ $# -ge 3 ]; then
    args+=(-H 'Content-Type: application/json' -d "$3")
  fi
  curl "${args[@]}"
}

curl -fsS "$base/readyz" >/dev/null

call POST /v1/types "{\"name\": \"$type\", \"fields\": [{\"name\": \"title\", \"type\": \"string\", \"required\": true}]}" >/dev/null
trap 'call DELETE "/v1/types/$type" >/dev/null' EXIT

entry=$(call POST "/v1/types/$type/entries" '{"fields": {"title": "smoke"}}')
id=$(printf '%s' "$entry" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
test -n "$id"

draft_status=$(curl -s -o /dev/null -w '%{http_code}' "$base/v1/content/$type/$id" -H "Authorization: Bearer $token")
if [ "$draft_status" != 404 ]; then
  echo "a draft answered $draft_status to a reader, expected 404" >&2
  exit 1
fi

call POST "/v1/types/$type/entries/$id/publish" >/dev/null
call GET "/v1/content/$type/$id" | grep -q '"title":"smoke"'
call GET "/v1/content/$type" | grep -q '"total":1'

echo "ok: $base serves what was published"
