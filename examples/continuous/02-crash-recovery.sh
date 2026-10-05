#!/bin/sh
# Safe crash recovery with an interrupted tool. Start a run that uses bash,
# interrupt it with Ctrl+C while the tool runs, then inspect and resume.
# The interrupted bash call is reported to the model as an error and never
# repeated automatically; a read or glob would be replayed after a fresh
# authorization check.
set -eu
STORE="${1:-./continuous-store}"
echo "Start the run and press Ctrl+C while 'sleep 30' is executing:"
zot continuous run "Run 'sleep 30 && echo done' with bash, then summarize." --store "$STORE" --tools bash || true
echo
echo "Recovery plan (nothing executes):"
zot continuous recover --dry-run --store "$STORE"
echo
echo "Resume. The model sees the interruption as a tool error:"
zot continuous run --resume --store "$STORE" --tools bash
