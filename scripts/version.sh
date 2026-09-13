#!/usr/bin/env bash
#
# Print the version string stamped into every binary via `-ldflags -X`.
#
# On a commit carrying a release tag this is exactly the tag. On any later commit
# the short SHA is appended as SemVer build metadata, so a binary built between
# releases names both the release it descends from and the commit it was actually
# built from:
#
#   v0.1.0          built from the v0.1.0 commit
#   v0.1.0+9fc21ab  built from a commit after v0.1.0
#   dev+9fc21ab     built before any release tag exists
#   dev             built outside a git checkout (a source tarball, say)
#
# The separator is "+" rather than "-" on purpose. SemVer reads "-" as a
# prerelease, which sorts BELOW the release it names — so v0.1.0-9fc21ab would
# claim to be older than v0.1.0, when it is strictly newer. "+" is build
# metadata: same version, identified build.
#
# Only vX.Y.Z tags are considered. This repo carries non-release tags such as
# backup/main-pre-squash, which --abbrev=0 would otherwise happily return.
#
# Usage: scripts/version.sh
# Override the result by setting VERSION in the environment for make targets.

set -uo pipefail

TAG_GLOB='v[0-9]*'

# On the tagged commit itself, the tag alone is the version.
if tag="$(git describe --tags --exact-match --match "$TAG_GLOB" 2>/dev/null)"; then
  printf '%s\n' "$tag"
  exit 0
fi

sha="$(git rev-parse --short HEAD 2>/dev/null)"
tag="$(git describe --tags --abbrev=0 --match "$TAG_GLOB" 2>/dev/null)"

if [[ -n "$tag" && -n "$sha" ]]; then
  printf '%s+%s\n' "$tag" "$sha"
elif [[ -n "$sha" ]]; then
  printf 'dev+%s\n' "$sha"
else
  printf 'dev\n'
fi
