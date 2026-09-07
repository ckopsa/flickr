#!/usr/bin/env bash
# A shell file changed without a sw.js cache bump fails the gate (flickr-ake).
#
# web/sw.js is CACHE-FIRST for the files in its SHELL list, and the only file
# a browser re-checks on its own is sw.js itself. So a change to index.html,
# kernel.js, renderers.js, player.js, reader.js — anything on that list —
# without a new CACHE version NEVER REACHES a browser that already has the old
# shell. It bit twice on 2026-09-07 (PR #11, and the tap-to-play fix): the fix
# was deployed, the page ran the old JS against the new API, and nothing said
# so. This is the check that says so.
#
# Usage:  scripts/shell-bump-check.sh <base-ref>
#
# It reads the SHELL list out of web/sw.js (so the list has one home), maps
# each entry to its file in the repo, and asks git which of them the diff
# against <base-ref> touched. If any did and web/sw.js's CACHE constant is the
# same on both sides, it fails.
set -euo pipefail

base="${1:-}"
if [ -z "$base" ]; then
  echo "usage: $0 <base-ref>" >&2
  exit 2
fi

sw=web/sw.js
[ -f "$sw" ] || { echo "::error::$sw is missing"; exit 1; }

# The SHELL list, one path per line, straight out of sw.js — nothing here
# keeps a second copy of it.
shell_paths() {
  sed -n '/^const SHELL = \[/,/\];/p' "$sw" |
    grep -o "'[^']*'" | tr -d "'"
}

# One SHELL entry as a file in the repo: '/' is the page itself, and every
# other entry is that path under web/.
repo_path() {
  case "$1" in
    /) echo "web/index.html" ;;
    /*) echo "web${1}" ;;
    *) echo "web/$1" ;;
  esac
}

cache_of() { # cache_of <ref|WORKTREE>
  if [ "$1" = "WORKTREE" ]; then cat "$sw"; else git show "$1:$sw" 2>/dev/null || true; fi |
    sed -n "s/^const CACHE = '\\(.*\\)';.*/\\1/p"
}

changed="$(git diff --name-only "$base"...HEAD -- web/ || true)"

# '/' and '/index.html' are the same file, so the list is deduped before the
# match: a message naming it twice would read like two changes.
touched="$(
  while read -r entry; do
    [ -n "$entry" ] || continue
    repo_path "$entry"
  done <<EOF
$(shell_paths)
EOF
)"
touched="$(printf '%s\n' "$touched" | sort -u | grep -Fxf <(printf '%s\n' "$changed") - || true)"

# Deleting a shell file is a shell change too: sw.js would go on precaching an
# address that 404s, and the install would fail outright.
if [ -z "$touched" ]; then
  echo "no shell file changed against $base — nothing to bump"
  exit 0
fi

before="$(cache_of "$base")"
after="$(cache_of WORKTREE)"

if [ -z "$after" ]; then
  echo "::error file=web/sw.js::web/sw.js has no CACHE constant to compare"
  exit 1
fi
if [ "$before" = "$after" ]; then
  echo "::error file=web/sw.js::the app shell changed ($(printf '%s' "$touched" | tr '\n' ' ')) but web/sw.js's CACHE is still '$after' — the shell is served cache-first and sw.js is the only file a browser re-checks, so without a bump no browser that already has the old shell will ever see this change."
  exit 1
fi

echo "shell changed ($(printf '%s' "$touched" | tr '\n' ' ')) and CACHE went $before -> $after"
