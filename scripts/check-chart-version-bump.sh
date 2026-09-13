#!/usr/bin/env bash
#
# Refuse a commit that changes a Helm chart without bumping that chart's
# `version` in its Chart.yaml.
#
# Why this exists: Helm keys its chart cache on the chart version. Editing a
# template and reinstalling under the same version is a no-op against a warm
# cache — the release keeps serving the old manifests, and nothing reports an
# error. The failure is silent and lands at rollout, far from the commit that
# caused it. Checking at commit time is the only place the two facts (which
# chart changed, what its version was before) are both cheap to know.
#
# Scope: `version` is the CHART version, bumped by any chart edit. `appVersion`
# is the version of the software the chart ships and tracks release tags — it is
# checked separately, at push time, by scripts/check-chart-app-version.sh.
#
# Usage:
#   scripts/check-chart-version-bump.sh [FILE...]   # files default to the index
#   scripts/check-chart-version-bump.sh --against main
#   scripts/check-chart-version-bump.sh --list      # charts it would check
#
# With no FILE arguments the staged file list is used, which is what makes this
# runnable by hand exactly as pre-commit runs it. Vendored subchart archives
# under charts/<name>/charts/ are ignored: they are dependency output, not ours.

set -euo pipefail

# shellcheck source=scripts/lib/semver.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib/semver.sh"

CHARTS_DIR="charts"
AGAINST="HEAD"
MODE="check"

usage() { sed -n '2,26p' "$0" | sed 's/^#//; s/^ //'; }

FILES=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --against) AGAINST="$2"; shift 2 ;;
    --list)    MODE="list"; shift ;;
    -h|--help) usage; exit 0 ;;
    --) shift; FILES+=("$@"); break ;;
    -*) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
    *)  FILES+=("$1"); shift ;;
  esac
done

# read_key FILE KEY — the value of a top-level Chart.yaml key. Chart.yaml keys
# are unindented, so anchoring at ^ is enough to avoid nested `version:` under
# dependencies. Strips quotes and trailing comments.
read_key() {
  sed -n "s/^$2:[[:space:]]*//p" "$1" | head -1 \
    | sed 's/[[:space:]]*#.*$//; s/^["'"'"']//; s/["'"'"']$//; s/[[:space:]]*$//'
}

# read_key_at REF PATH KEY — same, for a blob at a git ref. Prints nothing when
# the path does not exist there (a chart added in this commit).
read_key_at() {
  git show "$1:$2" 2>/dev/null | sed -n "s/^$3:[[:space:]]*//p" | head -1 \
    | sed 's/[[:space:]]*#.*$//; s/^["'"'"']//; s/["'"'"']$//; s/[[:space:]]*$//'
}

# bash 3.2 (the macOS system bash) has no mapfile, so lists are built by hand.
if [[ ${#FILES[@]} -eq 0 ]]; then
  while IFS= read -r line; do
    [[ -n "$line" ]] && FILES+=("$line")
  done < <(git diff --cached --name-only --diff-filter=ACMRD)
fi
[[ ${#FILES[@]} -eq 0 ]] && exit 0

# Map changed files to the charts that own them: charts/<name>/, and only when
# that directory still has a Chart.yaml (a deleted chart has nothing to bump).
charts=()
for f in "${FILES[@]}"; do
  [[ "$f" == "$CHARTS_DIR"/*/* ]] || continue          # charts/README.md et al.
  rest="${f#"$CHARTS_DIR"/}"; name="${rest%%/*}"
  [[ "${rest#*/}" == charts/* ]] && continue           # vendored subchart deps
  [[ -f "$CHARTS_DIR/$name/Chart.yaml" ]] || continue
  charts+=("$name")
done

[[ ${#charts[@]} -eq 0 ]] && exit 0

uniq_charts=()
while IFS= read -r line; do
  uniq_charts+=("$line")
done < <(printf '%s\n' "${charts[@]}" | sort -u)
charts=("${uniq_charts[@]}")

if [[ "$MODE" == "list" ]]; then
  printf '%s\n' "${charts[@]}"
  exit 0
fi

rc=0
for name in "${charts[@]}"; do
  path="$CHARTS_DIR/$name/Chart.yaml"
  new="$(read_key "$path" version)"
  old="$(read_key_at "$AGAINST" "$path" version)"

  if [[ -z "$old" ]]; then
    continue    # new chart: no prior version to bump past
  fi
  if [[ -z "$new" ]]; then
    echo "✗ $name: Chart.yaml has no top-level 'version:' key" >&2
    rc=1
    continue
  fi
  # Helm requires the chart version to be strict SemVer, which excludes the
  # leading v that Go release tags require. version_gt below cannot order a
  # non-numeric component, so a stray v would otherwise pass as "changed".
  if [[ "$new" == v* ]]; then
    {
      echo "✗ $name: chart version is \"$new\"; drop the leading v"
      echo "    Helm requires strict SemVer here (\"${new#v}\"). The v belongs"
      echo "    only on git release tags, where Go requires it."
    } >&2
    rc=1
    continue
  fi
  if version_gt "$new" "$old"; then
    continue
  fi

  {
    if [[ "$new" == "$old" ]]; then
      echo "✗ $name: files changed but chart version is still $old"
    else
      echo "✗ $name: chart version went $old → $new, which is not an increase"
    fi
    echo "    Helm caches by chart version; reinstalling at $old serves the old"
    echo "    manifests with no error. Bump it:"
    echo "      $path"
  } >&2
  rc=1
done

if [[ $rc -eq 0 ]]; then
  for name in "${charts[@]}"; do
    echo "✓ $name chart version $(read_key "$CHARTS_DIR/$name/Chart.yaml" version)"
  done
fi

exit $rc
