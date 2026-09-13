#!/usr/bin/env bash
#
# Assert that a credential value never renders into a ConfigMap's data, a
# container's command, or a container's args. Regression test for #109.
#
# Why this exists: reading a Kubernetes Secret needs `get secrets` RBAC.
# Copying that same value into a ConfigMap, or interpolating it into an
# initContainer's `command` (e.g. `PGPASSWORD=... psql ...`, `redis-cli -a
# ...`) or a container's `args` (e.g. `--requirepass ...`), makes it readable
# to anyone with the far lower `get configmaps` or `get pods` bar instead.
# This exact class of bug was closed before, in #16, #17 and #18, and came
# back when the Deployments that carried the fix were replaced -- so this
# gate renders every chart and every env values file with ONE shared
# credential value (the caller's sentinel, standing in for every
# password/token field) and asserts that value appears in none of the three
# unsafe places.
#
# What this does NOT check, on purpose:
#   - Secret `data`/`stringData` -- that is where a credential is *meant* to
#     live; the bar to read it (`get secrets`) is the one this gate exists to
#     protect.
#   - A plain env `value:` sourced from a values field (rather than
#     `valueFrom.secretKeyRef`) -- a real hygiene concern, but a different one
#     from "copied somewhere with a lower RBAC bar", and not what #109 scoped.
#   - Anything transformed before it renders (e.g. base64'd Basic-Auth
#     headers) -- the sentinel would not appear in the render even if the
#     value were mishandled, so a check built on literal substring matching
#     cannot see through it; that needs a different, targeted check.
#
# Usage:
#   scripts/check-credential-plaintext.sh SENTINEL RENDERED.yaml...
#   scripts/check-credential-plaintext.sh --selfcheck SENTINEL
#
# --selfcheck proves this script itself has teeth: it runs the scanner below
# against small, hand-built fixtures (not a real chart render, so its result
# never depends on which credential-hygiene fixes have already landed) and
# fails unless a clean fixture passes and each of the three tainted fixtures
# is rejected BY NAME.

set -euo pipefail

sentinel_scan() {
  # $1 = sentinel, $2 = file, $3 = label used in messages
  awk -v sentinel="$1" -v label="$3" '
  function reset_doc() {
    if (kind == "ConfigMap" && saw_data) nconfigmaps++
    kind = ""; name = ""; sect = ""; saw_data = 0
    containers_indent = -1; last_container = ""
    armed_key = ""; armed_indent = -1
  }
  function indent_of(s,    t) { t = s; sub(/[^ ].*/, "", t); return length(t) }
  function where() {
    if (last_container != "") return kind " " name " (container \"" last_container "\")"
    return kind " " name
  }
  function fail(msg) { printf "%s: FAIL %s\n", label, msg; fails++ }

  BEGIN { reset_doc() }
  /^---/ { reset_doc(); next }
  /^kind: / { kind = $2; next }
  /^metadata:$/ { sect = "metadata"; next }
  sect == "metadata" && /^  name: / { if (name == "") name = $2 }
  # Any other top-level (0-indent) key ends the metadata section, so nested
  # `template: metadata: name:` blocks deeper in the document never overwrite
  # the object'\''s own name.
  /^[A-Za-z]/ && !/^(apiVersion|kind|metadata):/ { sect = (sect == "metadata") ? "" : sect }

  # 1. ConfigMap data: a top-level (0-indent) `data:` key while kind ==
  #    ConfigMap opens the section; it runs until the next 0-indent line,
  #    since block-scalar content is always indented at least one more than
  #    the key that introduces it.
  kind == "ConfigMap" && /^data:$/ { sect = "data"; saw_data = 1; next }
  kind == "ConfigMap" && sect == "data" && /^[A-Za-z]/ { sect = "" }
  kind == "ConfigMap" && sect == "data" {
    if (index($0, sentinel) > 0) fail("sentinel found in ConfigMap data of " where())
  }

  # 2. Track the nearest enclosing containers:/initContainers: list, purely
  #    to name the offending container in a failure message -- detection
  #    itself does not depend on this.
  {
    ind = indent_of($0)
    if (containers_indent >= 0 && ind <= containers_indent && $0 !~ /^[ ]*$/) {
      containers_indent = -1; last_container = ""
    }
  }
  /^[ ]*(initContainers|containers):[ ]*$/ {
    containers_indent = indent_of($0); next
  }
  containers_indent >= 0 && $0 ~ ("^ {" (containers_indent + 2) "}- name: ") {
    last_container = $0
    sub(/^ *- name: */, "", last_container)
    next
  }

  # 3. command:/args:, either flow-style on one line (`command: ["a", "b"]`)
  #    or block-style (`command:` followed by indented `- item` lines). A
  #    block-style key stays "armed" until a line at or below its own
  #    indentation ends the list.
  armed_key != "" {
    ind = indent_of($0)
    if (ind <= armed_indent) { armed_key = ""; armed_indent = -1 }
    else if (index($0, sentinel) > 0) fail("sentinel found in " armed_key " of " where())
  }
  /^[ ]*(command|args):[ ]*\[.*\][ ]*$/ {
    key = $0; sub(/^[ ]*/, "", key); sub(/:.*/, "", key)
    ncontainers++
    if (index($0, sentinel) > 0) fail("sentinel found in " key " of " where())
    next
  }
  /^[ ]*(command|args):[ ]*$/ {
    key = $0; sub(/^[ ]*/, "", key); sub(/:.*/, "", key)
    armed_key = key; armed_indent = indent_of($0)
    ncontainers++
    next
  }

  END {
    reset_doc()
    if (nconfigmaps == 0 && ncontainers == 0) {
      fail("no ConfigMap data and no container command/args found in " label "; the scan is vacuous")
    } else if (fails == 0) {
      printf "  ok  %s: no sentinel in any of %d ConfigMap(s) and %d command/args block(s)\n", label, nconfigmaps, ncontainers
    }
    exit fails > 0
  }' "$2"
}

selfcheck() {
  local sentinel="$1" d
  d=$(mktemp -d)
  trap 'rm -rf "$d"' RETURN

  cat > "$d/clean.yaml" <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: clean-config
data:
  config.yaml: |
    database:
      password: "not-the-sentinel"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: clean-deploy
spec:
  template:
    spec:
      initContainers:
        - name: wait-for-postgres
          command: ['sh', '-c', 'until pg_isready; do sleep 2; done;']
      containers:
        - name: app
          command:
            - redis-server
            - "--maxmemory"
            - "192mb"
          args: ["serve", "--config", "/etc/config.yaml"]
EOF
  if ! sentinel_scan "$sentinel" "$d/clean.yaml" "selfcheck/clean" >"$d/out-clean.txt" 2>&1; then
    echo "selfcheck FAILED: the clean fixture was rejected -- the scanner is not fail-closed on real content:" >&2
    cat "$d/out-clean.txt" >&2
    return 1
  fi

  cat > "$d/tainted-configmap.yaml" <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: tainted-config
data:
  config.yaml: |
    database:
      password: "${sentinel}"
EOF
  cat > "$d/tainted-command.yaml" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: tainted-command-deploy
spec:
  template:
    spec:
      initContainers:
        - name: wait-for-postgres
          command: ['sh', '-c', 'PGPASSWORD=${sentinel} psql -c "SELECT 1"']
EOF
  cat > "$d/tainted-args.yaml" <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: tainted-args-deploy
spec:
  template:
    spec:
      containers:
        - name: redis
          command:
            - redis-server
            - "--requirepass"
            - "${sentinel}"
EOF

  local case file want
  for case in "tainted-configmap:tainted-config:ConfigMap data" \
              "tainted-command:tainted-command-deploy:command" \
              "tainted-args:tainted-args-deploy:args"; do
    file="${case%%:*}"
    rest="${case#*:}"
    name="${rest%%:*}"
    want="${rest#*:}"
    if ! sentinel_scan "$sentinel" "$d/$file.yaml" "selfcheck/$file" >"$d/out-$file.txt" 2>&1; then
      if grep -q "$name" "$d/out-$file.txt" && grep -q "$want" "$d/out-$file.txt"; then
        echo "  ok  selfcheck/$file: rejected by name ($name, $want)"
      else
        echo "selfcheck FAILED: $file was rejected, but not by name (expected \"$name\" and \"$want\" in the message):" >&2
        cat "$d/out-$file.txt" >&2
        return 1
      fi
    else
      echo "selfcheck FAILED: $file was NOT rejected -- the $want assertion is inert:" >&2
      cat "$d/out-$file.txt" >&2
      return 1
    fi
  done

  # An empty render must not read as "zero occurrences, therefore clean"
  # (cycle-1 review, found live: discord-default renders 0 objects, and an
  # entirely empty file passed with an "ok" line). A render that silently
  # produced nothing is a failure of the scan, not evidence of hygiene.
  : > "$d/empty.yaml"
  if sentinel_scan "$sentinel" "$d/empty.yaml" "selfcheck/empty" >"$d/out-empty.txt" 2>&1; then
    echo "selfcheck FAILED: an empty render passed -- the vacuity guard is inert:" >&2
    cat "$d/out-empty.txt" >&2
    return 1
  fi
  grep -q "vacuous" "$d/out-empty.txt" || {
    echo "selfcheck FAILED: an empty render was rejected, but not for being vacuous:" >&2
    cat "$d/out-empty.txt" >&2
    return 1
  }
  echo "  ok  selfcheck/empty: an empty render is rejected as vacuous, not read as clean"

  echo "  ok  selfcheck: clean fixture passes, all three tainted fixtures are rejected by name"
}

if [ $# -eq 0 ]; then
  echo "usage: $0 SENTINEL RENDERED.yaml..." >&2
  echo "       $0 --selfcheck SENTINEL" >&2
  exit 2
fi

if [ "$1" = "--selfcheck" ]; then
  shift
  [ $# -eq 1 ] || { echo "usage: $0 --selfcheck SENTINEL" >&2; exit 2; }
  selfcheck "$1"
  exit $?
fi

sentinel="$1"
shift
if [ $# -eq 0 ]; then
  echo "usage: $0 SENTINEL RENDERED.yaml..." >&2
  exit 2
fi

status=0
for f in "$@"; do
  label="$f"
  label="${label##*/}"
  sentinel_scan "$sentinel" "$f" "$label" || status=1
done
exit $status
