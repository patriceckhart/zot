#!/bin/sh
# Backup, verification, and restore. Archives are lossless journal copies with
# a checksum; they are not encrypted. Restore requires a new directory.
set -eu
STORE="${1:-./continuous-store}"
ARCHIVE="${2:-./history.zotbackup}"
RESTORED="${3:-./restored-store}"
zot continuous verify --store "$STORE"
zot continuous backup "$ARCHIVE" --store "$STORE"
zot continuous verify-backup "$ARCHIVE"
zot continuous restore "$ARCHIVE" --store "$RESTORED"
zot continuous verify --store "$RESTORED"
zot continuous check-state --store "$RESTORED"
