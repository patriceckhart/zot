#!/bin/sh
# A persistent local coding conversation. Every invocation continues the same
# workspace root conversation with its full committed history.
set -eu
STORE="${1:-./continuous-store}"
zot continuous run "List the Go packages in this repository." --store "$STORE" --tools read,glob
zot continuous run "Which of those has the most files?" --store "$STORE" --tools read,glob
zot continuous usage "$(zot continuous conversations --store "$STORE" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p' | head -1)" --store "$STORE"
