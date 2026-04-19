#!/usr/bin/env bash
# Minimal shell client for the `zot rpc` JSON protocol.
#
# Usage:
#   ./prompt.sh "fix the failing test"
#
# Sends one prompt, streams assistant text deltas to stdout, exits
# when the turn finishes. Requires `jq`.

set -euo pipefail

if [ $# -lt 1 ]; then
  echo "usage: $0 <prompt>" >&2
  exit 2
fi
prompt="$*"

# Build the prompt command frame with jq so quotes are escaped properly.
cmd=$(jq -nc --arg msg "$prompt" '{id:"1",type:"prompt",message:$msg}')

# Pipe the command into zot rpc; pipe its stdout through jq to react
# to events. The trailing `cat` keeps the input pipe open until the
# subprocess exits on its own (after `done`).
{
  if [ -n "${ZOTCORE_RPC_TOKEN:-}" ]; then
    jq -nc --arg t "$ZOTCORE_RPC_TOKEN" '{id:"0",type:"hello",token:$t}'
  fi
  echo "$cmd"
  # Block here so stdin stays open until zot exits.
  cat
} | zot rpc | while IFS= read -r line; do
  type=$(echo "$line" | jq -r '.type // empty')
  case "$type" in
    text_delta)
      echo "$line" | jq -rj '.delta // ""'
      ;;
    tool_call)
      name=$(echo "$line" | jq -r '.name // ""')
      echo >&2 ""
      echo >&2 "[tool] $name"
      ;;
    error)
      echo >&2 "[error] $(echo "$line" | jq -r '.message // ""')"
      exit 1
      ;;
    done)
      echo
      exit 0
      ;;
  esac
done
