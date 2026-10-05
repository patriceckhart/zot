#!/bin/sh
# Two clients on one conversation: one follows commits, the other submits and
# steers. Closing either client never cancels host work.
set -eu
STORE="${1:-./continuous-store}"
zot continuous serve --store "$STORE" --tools read,glob &
HOST=$!
sleep 1
SOCK="$STORE/host.sock"
zot continuous attach --follow --socket "$SOCK" --workspace shared &
FOLLOWER=$!
zot continuous attach "Describe this repository in three paragraphs." --socket "$SOCK" --workspace shared
# While a run is active, when_busy=steer joins at the next request boundary.
printf '{"id":"s1","method":"conversation.list","params":{}}\n' | nc -U "$SOCK" | head -1
kill "$FOLLOWER" "$HOST"
wait || true
