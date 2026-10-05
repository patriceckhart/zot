#!/bin/sh
# Live extension reload. The host resolves tools per engine generation: a
# SIGHUP (or the runtime.reload protocol method) respawns the extension
# processes, builds a new engine, validates it, and publishes it atomically.
# Calls that already started finish under the old generation. A failing
# reload keeps the previous generation active.
set -eu
STORE="${1:-./continuous-store}"
EXT="${2:-./examples/extensions/clock}"
zot continuous serve --store "$STORE" --ext "$EXT" &
HOST=$!
sleep 1
zot continuous attach "Use the extension's tool once." --socket "$STORE/host.sock" --workspace demo
echo "Edit the extension, then reload without restarting the host:"
kill -HUP "$HOST"
sleep 1
zot continuous attach "Use the extension's tool again." --socket "$STORE/host.sock" --workspace demo
kill "$HOST"
wait "$HOST" || true
