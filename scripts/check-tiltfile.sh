#!/usr/bin/env bash
#
# Evaluate the Tiltfile and assert two things about it:
#
#   1. it evaluates at all, with the collector enabled and disabled (#101);
#   2. the set of objects the Tiltfile EXPECTS and the set the charts RENDER
#      under Tilt's own configuration are the same set, in both directions
#      (#119).
#
# Why (1) exists: `make tilt-server-only` failed at Tiltfile load for a whole
# sprint and nothing noticed. The pre-commit hook of the day ran `tilt validate`,
# which is not a tilt command, behind a `|| echo "skipping"` fallback, so it
# passed on every commit whatever the Tiltfile did. No CI runner has tilt, so a
# Tiltfile that loads in one configuration and not the other ships green and
# breaks only for the contributor who picks the other one.
#
# Why (2) exists, and why it is a CLASS rather than an instance. The Tiltfile
# names rendered objects by fragment -- `accretion-collector-config:configmap`
# and so on -- to pin cluster-scoped objects to a resource. That correspondence
# can break in two directions, and they fail completely differently:
#
#   the Tiltfile names an object the chart does not render
#       Tilt fails at LOAD on the first unresolvable fragment, so NOTHING
#       deploys. Loud, and (1) catches it. This is what happened when the
#       collector's Redis Secret was gated on scale-out (#123) while the
#       Tiltfile still named it: no API, no bot, no collector.
#
#   the chart renders an object the Tiltfile does not name
#       Tilt fails NOTHING. It buckets the object into a resource called
#       `uncategorized` and carries on. The stack comes up, the object is
#       applied with no dependencies and no ordering, and the only trace is a
#       resource in the UI nobody is looking at.
#
# Only the first direction was ever enforced, and only by accident of Tilt's own
# strictness. The second is asserted here.
#
# This is the third form of the caller-to-chart correspondence problem. Pulumi's
# value keys are checked in Go at full depth; the Tiltfile's value keys are
# checked in bash at top level (scripts/check-values-matrix.sh). Both compare
# VALUE KEYS. This compares RENDERED OBJECTS, which no amount of rendering a
# chart on its own can reach: a review that renders the chart and looks for
# consumers inside it is answering "does anything in this chart still reference
# the Secret", and the Tiltfile is a consumer outside the chart. That is the
# boundary this check exists to cross, and stating it is the point -- the Redis
# Secret gating was reviewed, correctly, as safe within the chart.
#
# `tilt alpha tiltfile-result` evaluates the Tiltfile without a cluster or a
# running tilt session: exit 0 on success, 5 on an evaluation error, and it
# reports the resources the load produced. Settings after `--` reach
# config.parse() the same way `tilt up -- ...` passes them.
#
# tilt is a required development tool (see CONTRIBUTING.md), so its absence is
# an error here, not a skip: a skip is how this check was inert before.
#
# Usage:
#   scripts/check-tiltfile.sh              # prove the assertions fire, then check the tree
#   scripts/check-tiltfile.sh --selfcheck  # prove the assertions fire, and stop
#   scripts/check-tiltfile.sh --tree-only  # check the tree, and stop
#
# The default runs BOTH, in that order, because this script is reached through
# `make tiltfile-check` and a pre-commit hook that each invoke it with no
# arguments -- and a selfcheck no caller runs is the thing it exists to prevent.
# Elsewhere in this repository the two halves are separate `make` lines
# (values-matrix-check runs `--selfcheck` and then the real check); if this one
# gains that shape, the default here should become --tree-only. The whole
# selfcheck is four Tiltfile evaluations, well under a second each.

set -euo pipefail

if ! command -v tilt >/dev/null 2>&1; then
  echo "tilt not found: the Tiltfile cannot be checked. Install it (make preflight lists it)." >&2
  exit 1
fi

cd "$(git rev-parse --show-toplevel)"

RED=$'\033[0;31m'
GREEN=$'\033[0;32m'
NC=$'\033[0m'

# The central values file the Tiltfile reads.
CENTRAL_VALUES=deploy/tilt.yaml
# Where the selfcheck writes its deliberately broken copies. Created per run,
# not at a fixed path: this script runs from `make tiltfile-check` AND from a
# pre-commit hook, so two invocations can overlap, and each removes its fixture
# directory on exit. At a fixed path the first to finish deletes the other's
# fixtures mid-run, and the victim fails for a reason that has nothing to do
# with the tree -- a flaky gate, which is worse than no gate, because it teaches
# people to re-run until it is green. mktemp with a RELATIVE template because a
# Tiltfile resolves paths against its own directory, so --tilt_values must name
# a path relative to the repository root.
#
# Assigned after the cd above, so the template resolves under the repository's
# build/ rather than wherever the caller happened to be. mkdir -p first: on a
# fresh clone build/ does not exist yet and mktemp does not create parents.
mkdir -p build
FIXTURE_DIR=$(mktemp -d build/tilt-selfcheck.XXXXXX)
# Set here rather than inside the selfcheck, so every exit path removes it --
# including --tree-only, which never writes a fixture but has still created the
# directory by this line.
# shellcheck disable=SC2064  # expand FIXTURE_DIR now, not at trap time
trap "rm -rf '$FIXTURE_DIR'" EXIT

failures=0
fail() { printf '%b\n' "${RED}$*${NC}" >&2; failures=$((failures + 1)); }

# Evaluate the Tiltfile, leaving the output in $RESULT either way. Returns
# non-zero on an evaluation error. Settings are passed after `--`, exactly as
# `tilt up` passes them.
RESULT=
evaluate() {
  RESULT=$(tilt alpha tiltfile-result -- "$@" 2>&1) || return 1
}

# How many resources the load produced. Reported by every assertion below,
# because a check that does not say how much it examined cannot tell a clean run
# from an empty one -- which is the failure this repository keeps finding in its
# own gates.
resource_count() {
  printf '%s' "$1" | grep -cE '"Name": *"[A-Za-z0-9._-]+"' || true
}

# Tilt's name for the bucket it puts objects in when no k8s_resource claims them.
has_uncategorized() {
  printf '%s' "$1" | grep -q '"uncategorized"'
}

# ---------------------------------------------------------------------------
# The real tree
# ---------------------------------------------------------------------------

check_configuration() {
  local collector="$1" n
  if ! evaluate --enable_collector="$collector"; then
    fail "Tiltfile failed to evaluate with enable_collector=$collector:"
    printf '%s\n' "$RESULT" | tail -n 5 >&2
    return 1
  fi
  n=$(resource_count "$RESULT")
  # A load that produced no resources would satisfy "no uncategorized resource"
  # vacuously, so the count is asserted rather than merely printed.
  if [ "$n" -eq 0 ]; then
    fail "enable_collector=$collector: the Tiltfile evaluated but produced NO resources, so the object check below would pass on an empty set"
    return 1
  fi
  if has_uncategorized "$RESULT"; then
    fail "enable_collector=$collector: the charts render objects no Tilt resource claims, so Tilt bucketed them into \`uncategorized\` -- they are applied with no dependencies and no ordering. Name them in the Tiltfile (see collector_shared_objects) or stop deploying them."
    return 1
  fi
  printf '  ok  enable_collector=%-5s %s resources, every rendered object claimed\n' "$collector" "$n"
}

# ---------------------------------------------------------------------------
# Selfcheck: prove both assertions fire.
#
# Every case is a PAIR -- what must fail beside what must pass -- so a run in
# which nothing is obliged to succeed cannot read as a run that found nothing
# wrong. Each perturbation is also asserted to have LANDED: an edit that matched
# nothing produces a fixture identical to the original, and the case would then
# report a guard as broken when in fact it was never given anything to catch.
#
# The perturbations are driven entirely through the central values file, using
# the Tiltfile's own `tilt_values` setting, so no fixture Tiltfile is needed --
# a Tiltfile resolves relative paths against its own directory, so a copy
# anywhere but the repository root would not see charts/ or deploy/.
# ---------------------------------------------------------------------------

# Assert an evaluation FAILS, and fails for the reason under test.
must_fail_naming() {
  local label="$1" needle="$2"
  shift 2
  if evaluate "$@"; then
    fail "selfcheck/$label: the Tiltfile evaluated successfully when it should have refused. The guard is not firing, so it is a comment."
    return 1
  fi
  case "$RESULT" in
    *"$needle"*) printf '  ok  selfcheck/%-14s refused, and the message names %s\n' "$label" "$needle" ;;
    *)
      fail "selfcheck/$label: the evaluation failed, but never mentions \`$needle\` -- so this case could be passing on an unrelated error:"
      printf '%s\n' "$RESULT" | tail -n 3 >&2
      return 1
      ;;
  esac
}

# Assert a fixture actually differs from the file it was derived from.
fixture_differs() {
  local label="$1" fixture="$2" what="$3"
  if cmp -s "$CENTRAL_VALUES" "$fixture"; then
    fail "selfcheck/$label: the fixture is identical to $CENTRAL_VALUES -- $what, so this case tests nothing."
    return 1
  fi
}

selfcheck() {
  # FIXTURE_DIR is created and trapped at the top of the file, per run.

  # POSITIVE CONTROL, first and unconditionally: the real file must LOAD, and
  # must load through the same `tilt_values` path every failing case uses.
  # Without it, each refusal below could be the setting itself being broken.
  if ! evaluate --tilt_values="$CENTRAL_VALUES"; then
    fail "selfcheck/control: the real $CENTRAL_VALUES does not load through --tilt_values, so every refusal below would prove nothing:"
    printf '%s\n' "$RESULT" | tail -n 5 >&2
    return 1
  fi
  printf '  ok  selfcheck/%-14s the real %s loads (%s resources) through the setting the failing cases use\n' \
    "control" "$CENTRAL_VALUES" "$(resource_count "$RESULT")"

  # 1. A top-level key naming no chart. Helm ignores an unknown value key
  #    silently, so the release it was meant for would come up on chart
  #    defaults -- for accretion-api that means no ingress, no TLS, and a
  #    fail-closed empty identity list.
  sed 's/^accretion-discord:/accretion-discrod:/' "$CENTRAL_VALUES" > "$FIXTURE_DIR/typo.yaml"
  fixture_differs "typo-key" "$FIXTURE_DIR/typo.yaml" "no \`accretion-discord:\` key was renamed" &&
    must_fail_naming "typo-key" "accretion-discrod" --tilt_values="$FIXTURE_DIR/typo.yaml" || true

  # 2. A chart with no key at all: the same defect from the other side, and the
  #    reason the guard compares both directions rather than a count.
  awk '$0 != "accretion-db: {}"' "$CENTRAL_VALUES" > "$FIXTURE_DIR/missing.yaml"
  fixture_differs "missing-key" "$FIXTURE_DIR/missing.yaml" "no \`accretion-db: {}\` line was removed" &&
    must_fail_naming "missing-key" "accretion-db" --tilt_values="$FIXTURE_DIR/missing.yaml" || true

  # 3. THE OBJECT-CORRESPONDENCE CASE. Scaling a collector brings back the
  #    controller, Redis, its Secret, RBAC and Services -- none of which the
  #    Tiltfile names -- so they must surface as `uncategorized`. This is the
  #    exact scenario the Tiltfile's own comment predicts in prose; here it is
  #    asserted. The Redis password is required because the chart refuses to
  #    render a scaled collector without one; it is a render-only literal that
  #    reaches no cluster.
  awk '
    /^accretion-collector:$/ {
      print
      print "  redis:"
      print "    auth:"
      print "      password: render-only-not-a-credential"
      next
    }
    /^    replicaCount: 1$/ { print "    replicaCount: 2"; next }
    { print }
  ' "$CENTRAL_VALUES" > "$FIXTURE_DIR/scaled.yaml"
  fixture_differs "uncategorized" "$FIXTURE_DIR/scaled.yaml" "no collector replicaCount was scaled" || return 1
  if ! grep -q '^    replicaCount: 2$' "$FIXTURE_DIR/scaled.yaml"; then
    fail "selfcheck/uncategorized: the fixture carries no scaled collector, so it cannot render an object the Tiltfile does not name."
    return 1
  fi
  if ! evaluate --tilt_values="$FIXTURE_DIR/scaled.yaml"; then
    fail "selfcheck/uncategorized: the scaled fixture did not evaluate at all, so it cannot demonstrate an unclaimed object:"
    printf '%s\n' "$RESULT" | tail -n 3 >&2
    return 1
  fi
  if has_uncategorized "$RESULT"; then
    printf '  ok  selfcheck/%-14s a scaled collector renders objects the Tiltfile does not name, and they surface as uncategorized\n' "uncategorized"
  else
    fail "selfcheck/uncategorized: scaling a collector rendered the controller, Redis and their RBAC -- none of which the Tiltfile names -- and NO uncategorized resource appeared. The object-correspondence assertion is unable to detect an unclaimed object, so it passes for the wrong reason."
    return 1
  fi
}

mode="${1:-both}"
case "$mode" in
  --selfcheck) mode=selfcheck ;;
  --tree-only) mode=tree ;;
  both)        ;;
  *) echo "unknown argument: $1 (expected --selfcheck or --tree-only)" >&2; exit 2 ;;
esac

if [ "$mode" != "tree" ]; then
  selfcheck || true
  if [ "$failures" -gt 0 ]; then
    printf '%b\n' "${RED}$failures selfcheck case(s) failed: an assertion in this file does not fire, so a green run below would mean nothing${NC}" >&2
    exit 1
  fi
  printf '%b\n' "${GREEN}The Tiltfile detector fires: a mis-keyed central file is refused by name, and an object no resource claims is reported${NC}"
fi

if [ "$mode" != "selfcheck" ]; then
  check_configuration true || true
  check_configuration false || true
  [ "$failures" -eq 0 ] || exit 1
fi
