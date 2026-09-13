#!/usr/bin/env bash
#
# Semver comparison, sourced by scripts that gate on versions:
# check-chart-version-bump.sh (a chart version must increase) and preflight.sh
# (an installed tool must meet a minimum).
#
# Written for bash 3.2 — the macOS system bash, which Tilt and the git hooks
# both run under — so no mapfile, associative arrays, or `${var,,}`.

# semver_cmp A B — prints -1, 0 or 1 for A<B, A==B, A>B.
#
# Build metadata (+...) is ignored, per semver. A prerelease (-...) sorts below
# its release, and two prereleases compare lexically, which is enough for the
# tags and tool versions this repo deals with. A non-numeric component cannot be
# ordered, so such versions compare as merely equal-or-different.
semver_cmp() {
  local a="${1%%+*}" b="${2%%+*}"
  local a_core="${a%%-*}" b_core="${b%%-*}"
  local a_pre="" b_pre=""
  [[ "$a" == *-* ]] && a_pre="${a#*-}"
  [[ "$b" == *-* ]] && b_pre="${b#*-}"

  local i a_part b_part
  local a_arr b_arr
  IFS=. read -r -a a_arr <<< "$a_core"
  IFS=. read -r -a b_arr <<< "$b_core"
  for i in 0 1 2; do
    a_part="${a_arr[$i]:-0}"; b_part="${b_arr[$i]:-0}"
    if ! [[ "$a_part" =~ ^[0-9]+$ && "$b_part" =~ ^[0-9]+$ ]]; then
      [[ "$a" == "$b" ]] && { echo 0; return; }
      echo 1; return
    fi
    ((10#$a_part > 10#$b_part)) && { echo 1; return; }
    ((10#$a_part < 10#$b_part)) && { echo -1; return; }
  done

  if [[ -z "$a_pre" && -n "$b_pre" ]]; then echo 1; return; fi   # 1.0.0 > 1.0.0-rc1
  if [[ -n "$a_pre" && -z "$b_pre" ]]; then echo -1; return; fi
  if [[ "$a_pre" > "$b_pre" ]]; then echo 1; return; fi
  if [[ "$a_pre" < "$b_pre" ]]; then echo -1; return; fi
  echo 0
}

# version_gt A B — true when A is strictly greater than B.
version_gt() { [[ "$(semver_cmp "$1" "$2")" == "1" ]]; }

# version_ge A B — true when A is at least B.
version_ge() { [[ "$(semver_cmp "$1" "$2")" != "-1" ]]; }

# version_of "<command output>" — the first bare X.Y.Z (or X.Y) in a version
# banner, which is how every tool checked here reports itself.
version_of() {
  printf '%s' "$1" | grep -oE '[0-9]+\.[0-9]+(\.[0-9]+)?' | head -1
}
