#!/usr/bin/env bash
#
# Assert that no Role or ClusterRole in a rendered chart grants any verb on
# secrets. Regression test for #114, which generalizes the property PR #108
# proved for the collector chart alone (see check-collector-rbac.sh, which
# still owns that collector-specific assertion). `make rbac-secrets-check`
# renders accretion-api, accretion-collector, accretion-discord and
# otel-collector, in every environment, and passes them all to one
# invocation of this script.
#
# The one documented exception (accretion-mcp reading its own Secret by
# name, see internal/mcp/secrets/secrets.go) is allowed ONLY as `get` plus a
# non-empty resourceNames -- resourceNames does not apply to list/watch, so
# that combination can never widen into a blanket read. Anything else that
# touches secrets fails, naming the file, the role, and why.
#
# Cycle-1 review found this fail-open in three ways, all fixed here:
#   1. A rule reaches secrets not only via a literal "secrets" resources
#      entry, but via the "*" resources wildcard scoped to the core group
#      (where secrets live) -- e.g. apiGroups:["*"], resources:["*"],
#      verbs:["*"] is cluster-admin and was passing clean. is_core_group()
#      recognizes both an explicit "" apiGroups entry and an apiGroups
#      wildcard.
#   2. `resourceNames: []` (an empty list) was treated as a non-empty
#      resourceNames field because record() stores raw fragments and the old
#      check only asked "did we see this field at all", not "does it have
#      any actual entries". hasnames now counts real tokens via tokenize(),
#      so an empty list correctly reads as no restriction -- the blanket
#      `get secrets` the exception exists to forbid, not a documented one.
#   3. Helm's `toYaml` quotes a bare `*` with SINGLE quotes ('*'), since an
#      unquoted `*` is YAML's alias-node syntax. clean_list strips single
#      quotes too now, so a values-driven wildcard rule is not missed just
#      because of which quote character rendered it.
#
# Rendered rules: list show up in this repo in two shapes -- a hardcoded
# template's single-line flow style (`- apiGroups: [""]` /
# `  resources: [...]`) and Helm's `toYaml` on a values-driven list, which
# renders every field bare with its own block sequence, and lets that
# sequence's `- item` lines sit at the SAME indentation as the key
# introducing them (valid YAML). So a field's continuation values are told
# apart from the next sibling key by whether the line starts with `- `, not
# by indent arithmetic; only the indentation of where a rule ITEM begins is
# load-bearing.
#
# Usage: scripts/check-secrets-grant.sh RENDERED.yaml...

set -euo pipefail

if [ $# -eq 0 ]; then
  echo "usage: $0 RENDERED.yaml..." >&2
  exit 2
fi

status=0
for f in "$@"; do
  label="$f"
  label="${label##*/}"
  awk -v label="$label" -f /dev/stdin "$f" <<'AWK_PROGRAM' || status=1
function clean_list(s) {
  # Strips flow-list punctuation, including BOTH quote styles: a hardcoded
  # template double-quotes ("secrets"), Helm's toYaml single-quotes a bare
  # wildcard ('*') because an unquoted * is a YAML alias marker.
  gsub(/[\[\],"']/, " ", s)
  return s
}
function tokenize(s, out,    cleaned, tmp, n, i, j) {
  cleaned = clean_list(s)
  n = split(cleaned, tmp, /[ \t]+/)
  j = 0
  for (i = 1; i <= n; i++) if (tmp[i] != "") out[++j] = tmp[i]
  return j
}
function has_token(s, tok,    n, arr, i) {
  n = tokenize(s, arr)
  for (i = 1; i <= n; i++) if (arr[i] == tok) return 1
  return 0
}
function is_core_group(s) {
  # An explicit empty-string entry (apiGroups: [""], or block "- \"\"")
  # means the core group, and reads differently from an empty list
  # (apiGroups: [], meaning no groups at all -- vacuous, matches nothing).
  # tokenize() cannot distinguish these: an empty string is discarded as
  # noise everywhere else it is used (verbs, resources, resourceNames),
  # correctly, since there it never denotes anything meaningful. Here it
  # does, so it is detected structurally instead: an explicit "" entry
  # survives clean_list as an adjacent quote pair -- checked for both quote
  # styles, since a hand-written template could spell it apiGroups: [''] as
  # well as [""] (cycle-2 review: latent, not reachable through any render
  # in this repo today, but clean_list already treats both styles as
  # equivalent, so this check should not silently know only one).
  return (index(s, "\"\"") > 0) || (index(s, "''") > 0) || has_token(s, "*")
}
function indent_of(s,    t) { t = s; sub(/[^ ].*/, "", t); return length(t) }
function fail(msg) { printf "%s: FAIL %s\n", label, msg; fails++ }
function record(key, val) {
  if (key == "apiGroups") cur_apiGroups = cur_apiGroups " " val
  else if (key == "resources") cur_resources = cur_resources " " val
  else if (key == "verbs") cur_verbs = cur_verbs " " val
  else if (key == "resourceNames") cur_resourceNames = cur_resourceNames " " val
}

function flush_rule() {
  if (!have_rule) return
  touches_secrets = has_token(cur_resources, "secrets") || (has_token(cur_resources, "*") && is_core_group(cur_apiGroups))
  if (touches_secrets) {
    nv = tokenize(cur_verbs, varr)
    onlyget = (nv == 1 && varr[1] == "get")
    nrn = tokenize(cur_resourceNames, rnarr)
    hasnames = (nrn > 0)
    if (onlyget && hasnames) {
      printf "  ok  %s: %s %s grants get+resourceNames on secrets (documented exception): resourceNames=[%s]\n", label, kind, name, clean_list(cur_resourceNames)
    } else {
      extra = hasnames ? "" : " (no resourceNames -- a blanket grant)"
      fail(kind " " name " grants a verb on secrets: apiGroups=[" clean_list(cur_apiGroups) "] resources=[" clean_list(cur_resources) "] verbs=[" clean_list(cur_verbs) "]" extra)
    }
  }
  have_rule = 0; cur_apiGroups = ""; cur_resources = ""; cur_verbs = ""; cur_resourceNames = ""
}

function reset_doc() {
  flush_rule()
  if (kind == "ClusterRole" || kind == "Role") nrole++
  kind = ""; name = ""; sect = ""
  item_indent = -1; field = ""
  have_rule = 0; cur_apiGroups = ""; cur_resources = ""; cur_verbs = ""; cur_resourceNames = ""
}

BEGIN { reset_doc() }
/^---/ { reset_doc(); next }
/^kind: / { kind = $2; next }
/^metadata:$/ { sect = "metadata"; next }
sect == "metadata" && /^  name: / { if (name == "") name = $2 }
/^[A-Za-z]/ && !/^(apiVersion|kind|metadata):/ { if (sect == "metadata") sect = "" }

(kind == "ClusterRole" || kind == "Role") && /^rules:$/ { sect = "rules"; next }

sect == "rules" {
  ind = indent_of($0)
  is_dash = ($0 ~ /^[ ]*- /)
  is_new_item = is_dash && (item_indent == -1 || ind == item_indent) && $0 ~ /^[ ]*- [A-Za-z]/

  if (is_new_item) {
    flush_rule()
    have_rule = 1
    item_indent = ind
    line = $0; sub(/^[ ]*- /, "", line)
    key = line; sub(/:.*/, "", key)
    val = line; sub(/^[A-Za-z][A-Za-z0-9]*:[ ]*/, "", val)
    field = key
    if (val != "") { record(key, val); field = "" }
    next
  }
  if (!have_rule) next
  if (is_dash) {
    if (field != "") {
      line = $0; sub(/^[ ]*-[ ]*/, "", line)
      record(field, line)
    }
    next
  }
  if ($0 ~ /^[ ]*[A-Za-z][A-Za-z0-9]*:/ && ind > item_indent) {
    line = $0; sub(/^[ ]*/, "", line)
    key = line; sub(/:.*/, "", key)
    val = line; sub(/^[A-Za-z][A-Za-z0-9]*:[ ]*/, "", val)
    field = key
    if (val != "") { record(key, val); field = "" }
    next
  }
  flush_rule()
  sect = ""
}

END {
  reset_doc()
  printf "%s: %d role(s) inspected\n", label, nrole
  if (nrole == 0) fail("no Role or ClusterRole rendered in " label "; the secrets check is vacuous")
  exit fails > 0
}
AWK_PROGRAM
done
exit $status
