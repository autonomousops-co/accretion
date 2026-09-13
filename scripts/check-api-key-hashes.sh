#!/usr/bin/env bash
#
# Assert that every configured API-key identity's `hash` is the SHA-256 digest
# of that identity's plaintext in .env. Gate for #122.
#
# Why this exists: the chart's `apiKeys` list used to carry each caller's
# plaintext `key`, and the chart derived BOTH the server's digest and the
# client's copy from it -- one input, so the two halves could not drift. #122
# moved the plaintext into .env, because a working credential must not ship in a
# public repository, and that deliberately split one input into two that can now
# disagree:
#
#   .env                  the plaintext the CLIENT sends
#   apiKeys[].hash        the digest the SERVER compares against
#
# When they disagree the only symptom is a 401 on every request from that
# caller, with nothing in any log, manifest or render saying why -- the same
# silent class as the Secret-key mismatch scripts/make-secret.sh exists to
# prevent. scripts/make-secret.sh documents excluding API keys for exactly this
# reason, so the split has to be paid for with a gate rather than a comment.
#
# Usage:
#   scripts/check-api-key-hashes.sh IDENTITIES.yaml [--env-file PATH]
#   scripts/check-api-key-hashes.sh --selfcheck
#
# IDENTITIES.yaml is whichever values file declares `apiKeys` for the
# deployment being checked. It is an argument rather than a constant because
# that file is moving: charts/accretion-api/values-dev.yaml today, a central
# per-caller file once #119/#120 land.
#
# helm reads the YAML. It is the one parser every path already has -- CI's
# charts job installs helm and nothing else -- and it applies the chart's own
# merge semantics rather than a regex's guess at them.
#
# --selfcheck proves this script has teeth against hand-built fixtures, so its
# result never depends on which identities happen to be configured here.

set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

RED=$'\033[0;31m'
GREEN=$'\033[0;32m'
BLUE=$'\033[0;34m'
NC=$'\033[0m'

# name + hash for every entry in the file's apiKeys list, one per line, read
# through helm rather than parsed. A throwaway chart is the cheapest way to ask
# helm for a value: it has no "read this key" subcommand.
read_identities() {
  local file="$1" d="$2" chart="$2/reader" out
  if [ ! -d "$chart" ]; then
    mkdir -p "$chart/templates"
    printf 'apiVersion: v2\nname: reader\nversion: 0.0.0\n' > "$chart/Chart.yaml"
    cat > "$chart/templates/dump.yaml" <<'TPL'
{{- range .Values.apiKeys }}
# IDENTITY {{ .name }} {{ .hash | default "-" }}
{{- end }}
TPL
  fi
  if ! out=$(helm template reader "$chart" -f "$file" 2>&1); then
    echo "could not read apiKeys out of $file:" >&2
    printf '%s\n' "$out" >&2
    return 1
  fi
  printf '%s\n' "$out" | sed -n 's/^# IDENTITY //p'
}

# name + expected hash for every identity whose plaintext .env holds. The
# digests come from make-secret.sh so that the identity-to-variable mapping
# lives in exactly one place -- the same file that publishes the plaintext to
# the client, which is the half this gate is comparing against.
expected_identities() {
  local env_file="$1"
  bash scripts/make-secret.sh --env-file "$env_file" --print-identities 2>/dev/null |
    awk '/^  - name: /   { name = $3 }
         /^    hash: /   { h = $2; gsub(/"/, "", h); print name, h }'
}

# $3 = "strict": treat "there was nothing to compare" as a failure.
#
# Two states verify nothing: a file that declares no identities, and a file
# whose identities all have their plaintext somewhere other than this machine.
# Neither is a bug in a REAL run -- an empty list is already refused at render
# time by the chart's own fail-closed guard, and a cluster install legitimately
# holds hashes for callers whose keys live elsewhere -- so a real run reports
# them and moves on rather than blocking `make secrets`. They ARE failures under
# --strict, which is how the selfcheck proves that a run comparing nothing
# cannot be mistaken for a clean one.
check() {
  local file="$1" env_file="$2" strict="${3:-}" failures=0 checked=0 name hash want
  local d
  d=$(mktemp -d)
  # shellcheck disable=SC2064
  trap "rm -rf '$d'" RETURN

  read_identities "$file" "$d" > "$d/declared" || return 1
  expected_identities "$env_file" > "$d/expected"

  # The other direction FIRST, and deliberately ahead of the empty-declared
  # return below: a plaintext .env holds for an identity the file never declares
  # is a key nothing accepts, and that is MORE likely when the file declares
  # nothing at all, not less. Running it after the early return is what made
  # this gate pass in the repository's own default state -- IDENTITIES_FILE
  # points at a file with `apiKeys: []`, so every real run took the early return
  # and this loop never executed. The mode with teeth ran on fixtures and the
  # mode on real data had the hole.
  while read -r name _; do
    if ! awk -v n="$name" '$1 == n { found = 1 } END { exit !found }' "$d/declared"; then
      printf '%b\n' "${RED}$name: $env_file holds a plaintext for it, but $file declares no such identity. That caller holds a key the server will never accept.${NC}" >&2
      failures=$((failures + 1))
    fi
  done < "$d/expected"

  if [ ! -s "$d/declared" ]; then
    if [ "$strict" = "strict" ]; then
      printf '%b\n' "${RED}$file declares no apiKeys, so nothing was verified -- a run that compares nothing must not read as clean. An empty list is also refused at render time by the chart's fail-closed guard.${NC}" >&2
      return 1
    fi
    [ "$failures" -eq 0 ] || return 1
    printf '  --  %s declares no apiKeys, so there is nothing to verify. The chart refuses an empty list at render time; declare your callers with: scripts/make-secret.sh --print-identities\n' "$file"
    return 0
  fi

  # Every declared identity whose plaintext .env holds must agree with it. An
  # identity with no local plaintext is reported, not failed: a cluster install
  # legitimately holds hashes for callers whose keys live somewhere else.
  while read -r name hash; do
    want=$(awk -v n="$name" '$1 == n { print $2 }' "$d/expected")
    if [ -z "$want" ]; then
      printf '  --  %-20s no plaintext in %s, not verified\n' "$name" "$env_file"
      continue
    fi
    checked=$((checked + 1))
    if [ "$hash" != "$want" ]; then
      printf '%b\n' "${RED}$name: the hash in $file is not the digest of its plaintext in $env_file.${NC}" >&2
      printf '%b\n' "${RED}      declared $hash${NC}" >&2
      printf '%b\n' "${RED}      .env implies $want${NC}" >&2
      printf '%b\n' "${RED}      That client would send a key the server rejects, and the only symptom is a 401 with nothing explaining it. Paste the block from: scripts/make-secret.sh --print-identities${NC}" >&2
      failures=$((failures + 1))
      continue
    fi
    printf '  ok  %-20s hash matches its plaintext in %s\n' "$name" "$env_file"
  done < "$d/declared"

  if [ "$failures" -gt 0 ]; then
    return 1
  fi
  if [ "$checked" -eq 0 ]; then
    if [ "$strict" = "strict" ]; then
      printf '%b\n' "${RED}not one declared identity had a plaintext in $env_file, so nothing was actually compared -- a run that compares nothing must not read as clean.${NC}" >&2
      return 1
    fi
    printf '  --  no declared identity has a plaintext in %s, so nothing was compared. That is normal for a deployment whose keys live elsewhere.\n' "$env_file"
    return 0
  fi
  printf '%b\n' "${GREEN}$checked identity hash(es) match their plaintext in $env_file${NC}"
}

selfcheck() {
  local d out
  d=$(mktemp -d)
  # shellcheck disable=SC2064
  trap "rm -rf '$d'" RETURN

  printf '%b\n' "${BLUE}Proving the API-key hash gate fires...${NC}"

  # devkey-mcp / devkey-ci digests, computed here rather than pasted so the
  # fixture cannot rot against a change in how the digest is taken.
  local mcp_hash ci_hash
  mcp_hash=$(printf '%s' devkey-mcp | shasum -a 256 | cut -d' ' -f1)
  ci_hash=$(printf '%s' devkey-ci | shasum -a 256 | cut -d' ' -f1)

  printf 'ACCRETION_API_KEY_MCP=devkey-mcp\nACCRETION_API_KEY_CI=devkey-ci\n' > "$d/env"

  printf 'apiKeys:\n  - name: accretion-mcp\n    hash: "%s"\n    permissions: ["read:memory"]\n  - name: ci\n    hash: "%s"\n    permissions: ["read:memory"]\n' \
    "$mcp_hash" "$ci_hash" > "$d/agree.yaml"
  if ! out=$(check "$d/agree.yaml" "$d/env" strict 2>&1); then
    printf '%b\n' "${RED}selfcheck FAILED: agreeing hashes were rejected -- the gate is not usable on real content:${NC}" >&2
    printf '%s\n' "$out" >&2
    return 1
  fi
  printf '  ok  selfcheck/agree: two identities whose hashes match their plaintext pass\n'

  printf 'apiKeys:\n  - name: accretion-mcp\n    hash: "%s"\n    permissions: ["read:memory"]\n  - name: ci\n    hash: "%s"\n    permissions: ["read:memory"]\n' \
    "$mcp_hash" "$(printf '%s' rotated-but-not-updated | shasum -a 256 | cut -d' ' -f1)" > "$d/stale.yaml"
  if out=$(check "$d/stale.yaml" "$d/env" strict 2>&1); then
    printf '%b\n' "${RED}selfcheck FAILED: a hash that is not the digest of its plaintext was accepted -- the gate is inert${NC}" >&2
    return 1
  fi
  case "$out" in
    *"ci: the hash in"*) ;;
    *) printf '%b\n' "${RED}selfcheck FAILED: the stale hash was rejected, but the identity was not named: $out${NC}" >&2; return 1 ;;
  esac
  printf '  ok  selfcheck/stale: a rotated plaintext whose hash was not updated is rejected by name\n'

  printf 'apiKeys:\n  - name: accretion-mcp\n    hash: "%s"\n    permissions: ["read:memory"]\n' "$mcp_hash" > "$d/undeclared.yaml"
  if out=$(check "$d/undeclared.yaml" "$d/env" strict 2>&1); then
    printf '%b\n' "${RED}selfcheck FAILED: a plaintext for an identity the chart never declares was accepted -- that client holds a key nothing accepts${NC}" >&2
    return 1
  fi
  case "$out" in
    *"ci:"*"declares no such identity"*) ;;
    *) printf '%b\n' "${RED}selfcheck FAILED: the undeclared identity was rejected, but not by name: $out${NC}" >&2; return 1 ;;
  esac
  printf '  ok  selfcheck/undeclared: a plaintext with no matching identity is rejected by name\n'

  printf 'apiKeys: []\n' > "$d/empty.yaml"
  # NOT under --strict, deliberately: this is the repository's own default state
  # (IDENTITIES_FILE points at a values file this sprint set to `apiKeys: []`)
  # and it is the state in which this gate passed vacuously until cycle-1
  # review. The existing 'undeclared' case above cannot reach it, because it
  # exercises the reverse loop only when the declared list is non-empty. A run
  # over real data must refuse a plaintext that no identity declares whether the
  # declared list has five entries or none -- if anything, none is the more
  # dangerous shape, because then every key in .env is undeclared.
  if out=$(check "$d/empty.yaml" "$d/env" 2>&1); then
    printf '%b\n' "${RED}selfcheck FAILED: a file declaring NO identities passed while .env held keys for two of them. Every one of those clients holds a credential the server will never accept, and this is the state the repository ships in.${NC}" >&2
    return 1
  fi
  case "$out" in
    *"declares no such identity"*) ;;
    *) printf '%b\n' "${RED}selfcheck FAILED: the empty-declared-with-plaintext state was rejected, but not for the plaintext having no identity: $out${NC}" >&2; return 1 ;;
  esac
  printf '  ok  selfcheck/empty-with-plaintext: a plaintext whose identity list is EMPTY is rejected by name, not read as nothing to verify\n'

  if out=$(check "$d/empty.yaml" "$d/env" strict 2>&1); then
    printf '%b\n' "${RED}selfcheck FAILED: a file declaring no identities passed -- the vacuity guard is inert${NC}" >&2
    return 1
  fi
  case "$out" in
    *"must not read as clean"*) ;;
    *) printf '%b\n' "${RED}selfcheck FAILED: an empty identity list was rejected, but not for verifying nothing: $out${NC}" >&2; return 1 ;;
  esac
  printf '  ok  selfcheck/empty: under --strict a file declaring no identities is rejected, not read as clean\n'

  : > "$d/no-keys.env"
  if out=$(check "$d/agree.yaml" "$d/no-keys.env" strict 2>&1); then
    printf '%b\n' "${RED}selfcheck FAILED: an env file holding no plaintext at all passed -- nothing was compared and it read as clean${NC}" >&2
    return 1
  fi
  case "$out" in
    *"must not read as clean"*) ;;
    *) printf '%b\n' "${RED}selfcheck FAILED: an env file with no plaintext was rejected, but not for verifying nothing: $out${NC}" >&2; return 1 ;;
  esac
  printf '  ok  selfcheck/no-plaintext: under --strict an env file that verifies nothing is rejected\n'

  printf '%b\n' "${GREEN}The API-key hash gate fires: stale, undeclared, empty and unverifiable identity sets are all rejected by name${NC}"
}

if [ "${1:-}" = "--selfcheck" ]; then
  [ $# -eq 1 ] || { echo "usage: $0 --selfcheck" >&2; exit 2; }
  selfcheck
  exit $?
fi

[ $# -ge 1 ] || { echo "usage: $0 IDENTITIES.yaml [--env-file PATH]" >&2; exit 2; }
identities_file="$1"
shift
env_file=".env"
while [ $# -gt 0 ]; do
  case "$1" in
    -f|--env-file) env_file="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[ -f "$identities_file" ] || { echo "$identities_file does not exist" >&2; exit 1; }
[ -f "$env_file" ] || { echo "$env_file does not exist — copy env.example to .env and set your API keys" >&2; exit 1; }
check "$identities_file" "$env_file"
