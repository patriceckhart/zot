#!/bin/sh
# Automatic compaction is derived from the model's context window; the
# handoff tool lets the model close its context and continue fresh. After the
# run, search shows history before and after the reset.
set -eu
STORE="${1:-./continuous-store}"
zot continuous run "Summarize what you know so far, then call the handoff tool with a note and the instruction 'continue with step two'." --store "$STORE" --tools read,glob
ID="$(zot continuous conversations --store "$STORE" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | head -1)"
zot continuous search "$ID" --type reset --store "$STORE"
