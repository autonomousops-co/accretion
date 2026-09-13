#!/usr/bin/env bash
#
# Refuse to push a release tag whose name disagrees with the `appVersion` of the
# charts that ship the accretion binary.
#
# Why this exists: `appVersion` is the only place a deployed pod says which
# release it is running. A tag pushed without the matching bump produces charts
# that report the previous version forever — `helm list` and every bug report
# then name a release whose code is not what is installed. Nothing detects this
# afterwards, because both values are individually valid; only their
# disagreement is wrong, and a tag is immutable once others have fetched it.
#
# Scope: `appVersion` is the version of the software the chart ships, so it
# tracks release tags. The chart's own `version` is bumped by any chart edit and
# is checked at commit time by scripts/check-chart-version-bump.sh.
#
# Tag forms:
#   v1.2.3                  every chart in RELEASE_CHARTS must have appVersion 1.2.3
#   1.2.3                   same; the leading v is optional
#   accretion-api-v1.2.3    only that chart is checked, for when charts diverge
#
# Usage:
#   scripts/check-chart-app-version.sh --tag v1.2.3     # check one tag by name
#   scripts/check-chart-app-version.sh                  # read git's pre-push stdin
#   scripts/check-chart-app-version.sh --list           # charts it guards
#
# Values are read from the commit the tag points at, not the working tree, so
# pushing an old tag is validated against what that tag actually contains.

set -euo pipefail

CHARTS_DIR="charts"

# Charts whose appVersion tracks the release tag: the ones that deploy the
# accretion-api image (one binary, different subcommands). accretion-db ships
# pgvector and otel-collector ships upstream collector images — their
# appVersions track those projects, not ours, so they are deliberately absent.
RELEASE_CHARTS=(
  accretion-api
  accretion-collector
  accretion-discord
)

usage() { sed -n '2,29p' "$0" | sed 's/^#//; s/^ //'; }

TAG=""
MODE="check"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --tag)     TAG="$2"; shift 2 ;;
    --list)    MODE="list"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ "$MODE" == "list" ]]; then
  printf '%s\n' "${RELEASE_CHARTS[@]}"
  exit 0
fi

# Collect the tags being pushed. Three input modes, because this script is
# driven three ways: by hand or in CI (--tag), by git's native pre-push hook
# (stdin), and by pre-commit's pre-push stage (which consumes stdin itself and
# reports the ref through the environment instead).
TAGS=()
if [[ -n "$TAG" ]]; then
  TAGS+=("${TAG#refs/tags/}")
elif [[ -n "${PRE_COMMIT_REMOTE_BRANCH:-}" ]]; then
  # Under pre-commit. Checked before stdin on purpose: pre-commit reads git's
  # ref lines itself and hands the hook an empty-but-not-a-tty stdin, so the
  # stdin branch below would silently find nothing and pass everything.
  [[ "$PRE_COMMIT_REMOTE_BRANCH" == refs/tags/* ]] \
    && TAGS+=("${PRE_COMMIT_REMOTE_BRANCH#refs/tags/}")
elif [[ ! -t 0 ]]; then
  # git pre-push stdin: "<local ref> <local sha> <remote ref> <remote sha>"
  while read -r _local_ref local_sha remote_ref _remote_sha; do
    [[ "$remote_ref" == refs/tags/* ]] || continue
    [[ "$local_sha" =~ ^0+$ ]] && continue        # tag deletion, nothing to check
    TAGS+=("${remote_ref#refs/tags/}")
  done
fi

[[ ${#TAGS[@]} -eq 0 ]] && exit 0                 # branch push: not our business

# read_app_version REF PATH — the top-level appVersion of a Chart.yaml as it
# exists at REF. Chart.yaml keys are unindented, so ^ anchoring is enough.
read_app_version() {
  git show "$1:$2" 2>/dev/null | sed -n 's/^appVersion:[[:space:]]*//p' | head -1 \
    | sed 's/[[:space:]]*#.*$//; s/^["'"'"']//; s/["'"'"']$//; s/[[:space:]]*$//'
}

rc=0
for tag in "${TAGS[@]}"; do
  # Split an optional "<chart>-" prefix off the front of the tag.
  scoped=""
  want="$tag"
  for name in "${RELEASE_CHARTS[@]}"; do
    if [[ "$tag" == "$name"-* ]]; then
      scoped="$name"
      want="${tag#"$name"-}"
      break
    fi
  done
  # The two conventions differ on the leading v and both are load-bearing:
  # a Go module release tag MUST be vX.Y.Z or the module proxy cannot see it,
  # while Helm's appVersion is written bare. So the v is required here, stripped
  # once, and required absent on the chart side below.
  if [[ "$want" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+].*)?$ ]]; then
    {
      echo "✗ $tag: release tags must be v-prefixed (vX.Y.Z), not bare"
      echo "    Go resolves module versions only from v-prefixed tags, so \"$want\""
      echo "    is invisible to \`go get\` and the module proxy. Retag as v$want."
    } >&2
    rc=1
    continue
  fi
  if ! [[ "$want" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-+].*)?$ ]]; then
    echo "• $tag: not a release tag (no semver), skipping appVersion check"
    continue
  fi
  want="${want#v}"

  if [[ -n "$scoped" ]]; then
    check=("$scoped")
  else
    check=("${RELEASE_CHARTS[@]}")
  fi

  for name in "${check[@]}"; do
    path="$CHARTS_DIR/$name/Chart.yaml"
    got="$(read_app_version "$tag" "$path")"

    if [[ -z "$got" ]]; then
      echo "✗ $tag: $path has no appVersion at that tag (or the file is absent)" >&2
      rc=1
    elif [[ "$got" == v* ]]; then
      # Reported separately from a plain mismatch: "v1.2.3" vs "1.2.3" is a
      # convention error, and saying "expected 1.2.3" alone reads as a version
      # disagreement rather than a stray character.
      {
        echo "✗ $tag: $name appVersion is \"$got\"; drop the leading v"
        echo "    Helm appVersion is written bare (\"$want\"); only the git tag"
        echo "    carries the v, because Go requires it there."
      } >&2
      rc=1
    elif [[ "$got" != "$want" ]]; then
      {
        echo "✗ $tag: $name appVersion is \"$got\", expected \"$want\""
        echo "    A pod deployed from this tag would report $got, so \`helm list\`"
        echo "    and every bug report would name the wrong release."
      } >&2
      rc=1
    else
      echo "✓ $tag: $name appVersion $got"
    fi
  done
done

if [[ $rc -ne 0 ]]; then
  {
    echo
    echo "Fix the chart, then move the tag before anyone fetches it:"
    echo "    git tag -f <tag> && git push --force origin <tag>   # only if unpushed"
    echo "Tags others have already fetched must be superseded, not moved."
  } >&2
fi

exit $rc
