#!/usr/bin/env bash
#
# Prove that `make lint-new` reports findings when the working directory is
# reached through a symlink. Regression test for #98.
#
# Why this exists: golangci-lint's --new-from-rev filter keeps a finding only
# when its path, made relative to the working directory, matches a path in
# `git diff --relative`. The working directory is symlink-resolved
# (pkg/fsutils/fsutils.go, Getwd → EvalSymlinks) but the finding's own path
# comes from `go list`, which reports whatever $PWD says. From a directory
# reached through a symlink the relative path becomes ../../private/tmp/...,
# matches nothing in the diff, and every finding is dropped: the run prints
# "0 issues" over a tree that has plenty. macOS /tmp is a symlink to
# /private/tmp and the sprint tooling puts its worktrees there, which is how
# sprint-10 got a false pass for three review cycles. A full `golangci-lint
# run` is unaffected — only the new-from-rev filter compares paths.
#
# The Makefile target enters the physical path before running. This script
# checks that it still does: it checks HEAD out into a throwaway detached
# worktree, reaches it through a freshly created symlink, appends a deliberate
# misspelling to a Go file, and requires `make lint-new` to report it from BOTH
# the symlinked path and the physical one. The physical run is the control: if
# it does not see the canary either, the canary is broken and the check would
# be vacuous, so that is reported as its own failure rather than as a pass.
#
# The Makefile under test is the one in this checkout, not the one at HEAD, so
# the check is meaningful before the fix is committed. LINT_NEW_MAKEFILE
# overrides it, which is how the check itself was shown to fail against the
# pre-fix target.
#
# Usage:
#   scripts/check-lint-new-symlink.sh
#   LINT_NEW_MAKEFILE=/path/to/old/Makefile scripts/check-lint-new-symlink.sh

set -euo pipefail

root=$(git rev-parse --show-toplevel)
makefile=${LINT_NEW_MAKEFILE:-$root/Makefile}

# mktemp on macOS answers under /var, which is itself a symlink to /private/var.
# Resolve it, or the "physical" control run would go through a symlink too.
tmp=$(cd "$(mktemp -d)" && pwd -P)
physical="$tmp/physical"
link="$tmp/link"
wt="$physical/wt"

cleanup() {
  git -C "$root" worktree remove --force "$wt" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT

mkdir -p "$physical"
ln -s "$physical" "$link"
git -C "$root" worktree add --detach --quiet "$wt" HEAD

canary=cmd/accretion-api/main.go
printf '\n// lint-new canary: this behaviour is misspelled on purpose.\n' >> "$wt/$canary"

# Runs lint-new from $1 with the cwd entered by `cd`, so $PWD carries the
# symlink exactly as it does for a developer's shell. `make -C` would not do:
# make chdir()s without touching PWD, the recipe shell notices the mismatch and
# recomputes a physical cwd, and the symlink would never reach golangci-lint.
reports_canary() {
  local out
  out=$(cd "$1" && make -f "$makefile" lint-new 2>&1) && return 1
  grep -q "$canary:[0-9]*:[0-9]*: .*misspelling" <<<"$out"
}

if ! reports_canary "$wt"; then
  echo "lint-new did not report the canary even from the physical path $wt" >&2
  echo "The canary itself is not being detected, so this check is vacuous." >&2
  echo "Is misspell still enabled in .golangci.yml, and is origin/main present?" >&2
  exit 1
fi
echo "  ok  lint-new reports the canary from the physical path"

if ! reports_canary "$link/wt"; then
  echo "lint-new reported nothing from the symlinked path $link/wt" >&2
  echo "The same tree reports a finding from $wt, so the new-from-rev filter is" >&2
  echo "dropping findings on a symlinked cwd again. See Makefile lint-new (#98)." >&2
  exit 1
fi
echo "  ok  lint-new reports the canary from the symlinked path"
