#!/usr/bin/env bash
#
# Assert the shape of OTLP trace ingest in rendered accretion-collector
# manifests, and that no PodDisruptionBudget can block a node drain forever.
# Regression test for #104; `make trace-ingest-check` renders every profile
# and runs this over each.
#
# Why this exists: trace ingest is the only push-based path in the system --
# what is not absorbed is gone, where the pull-based collectors simply catch
# up. Sprint-10 folded it from its own Deployment (2 replicas, HPA 2-5, PDB
# minAvailable 1) into the shared data-collector pod at one replica with no
# HPA and no PDB, and nothing rendered checked the difference. The obvious
# "fix" of enabling the shared pod's PDB at one replica is worse: minAvailable
# 1 over one replica blocks every drain indefinitely, so that is asserted
# against too, for every PDB in every render.
#
# Modes:
#   --redundant FILE...   staging/prod: the trace receiver must be its own
#                         Deployment (split), scaled to at least 2 replicas or
#                         autoscaled with minReplicas >= 2, and covered by a PDB.
#   FILE...               any render: PDB sanity only.
#
# In both modes every PodDisruptionBudget must target a workload whose floor
# (replicas, or the HPA's minReplicas when autoscaled) exceeds minAvailable.
# A render with no PDB at all passes that check but says so, and the
# --redundant assertions refuse to pass when they find no trace receiver.
#
# Usage:
#   scripts/check-trace-ingest.sh --redundant staging.yaml prod.yaml
#   scripts/check-trace-ingest.sh default.yaml dev.yaml test.yaml

set -euo pipefail

redundant=0
if [ "${1:-}" = "--redundant" ]; then
  redundant=1
  shift
fi
if [ $# -eq 0 ]; then
  echo "usage: $0 [--redundant] RENDERED.yaml..." >&2
  exit 2
fi

status=0
for f in "$@"; do
  awk -v file="$f" -v redundant="$redundant" '
  function flush() {
    if (kind == "Deployment") {
      dep_component[name] = component
      dep_replicas[name] = replicas
      dep_traces[name] = traces
    } else if (kind == "HorizontalPodAutoscaler") {
      hpa_min[target] = minrep
    } else if (kind == "PodDisruptionBudget") {
      pdb_min[name] = minavail
      pdb_component[name] = selcomponent
    }
  }
  function reset() {
    kind = ""; name = ""; sect = ""; component = ""; replicas = ""
    traces = 0; target = ""; minrep = ""; minavail = ""; selcomponent = ""
  }
  function fail(msg) { printf "%s: FAIL %s\n", label, msg; fails++ }
  BEGIN { reset(); label = file; sub(/.*\//, "", label) }
  /^---/ { flush(); reset(); next }
  /^kind: / { kind = $2; next }
  /^metadata:$/ { sect = "metadata"; next }
  /^spec:$/ { sect = "spec"; next }
  /^  name: / { if (sect == "metadata" && name == "") name = $2; next }
  # Top-level labels sit at four spaces; the pod template and selectors are deeper.
  sect == "metadata" && /^    app\.kubernetes\.io\/component: / { component = $2; next }
  kind == "Deployment" && sect == "spec" && /^  replicas: / { replicas = $2; next }
  kind == "Deployment" && /accretion\.io\/collects-traces: "true"/ { traces = 1; next }
  kind == "HorizontalPodAutoscaler" && /^    name: / { target = $2; next }
  kind == "HorizontalPodAutoscaler" && /^  minReplicas: / { minrep = $2; next }
  kind == "PodDisruptionBudget" && /^  minAvailable: / { minavail = $2; next }
  kind == "PodDisruptionBudget" && /^      app\.kubernetes\.io\/component: / { selcomponent = $2; next }
  END {
    flush()

    # Floor of a Deployment: its replicas, or its HPA minReplicas when autoscaled.
    for (d in dep_component) {
      if (d in hpa_min) floor[d] = hpa_min[d]
      else floor[d] = dep_replicas[d]
    }

    # 1. No PDB may block a drain: floor must exceed minAvailable.
    npdb = 0
    for (p in pdb_min) {
      npdb++
      hit = ""
      for (d in dep_component) if (dep_component[d] == pdb_component[p]) hit = d
      if (hit == "") { fail("PDB " p " selects component " pdb_component[p] " but no Deployment carries it"); continue }
      if (pdb_min[p] == "") { fail("PDB " p " has no minAvailable; this check only understands minAvailable"); continue }
      if (floor[hit] + 0 <= pdb_min[p] + 0)
        fail("PDB " p " minAvailable=" pdb_min[p] " over " hit " with a floor of " floor[hit] " replicas blocks node drains indefinitely")
    }
    if (fails == 0) {
      if (npdb == 0) printf "  ok  %s: no PodDisruptionBudget rendered, nothing can block a drain\n", label
      else printf "  ok  %s: every PodDisruptionBudget (%d) leaves at least one disruptable replica\n", label, npdb
    }
    if (!redundant) exit fails > 0

    # 2. Redundant trace ingest: split, >= 2 replicas, PDB-covered.
    before = fails; receiver = ""
    for (d in dep_component) if (dep_traces[d]) receiver = d
    if (receiver == "") {
      fail("no Deployment carries collects-traces=\"true\"; the trace ingest check is vacuous")
    } else {
      if (receiver !~ /-trace-collector$/)
        fail("trace receiver is " receiver ", a shared pod: a sibling collector failing readiness deregisters the OTLP endpoint. Split trace out (dataCollector.kinds.trace.split=true)")
      if (floor[receiver] + 0 < 2)
        fail("trace receiver " receiver " has a floor of " floor[receiver] " replica(s); OTLP ingest is push-based and needs at least 2")
      covered = 0
      for (p in pdb_component) if (pdb_component[p] == dep_component[receiver]) covered = 1
      if (!covered) fail("trace receiver " receiver " has no PodDisruptionBudget; a drain can take every replica at once")
    }
    if (fails == before)
      printf "  ok  %s: trace receiver %s is its own Deployment, floor %s replicas, PDB-covered\n", label, receiver, floor[receiver]

    exit fails > 0
  }' "$f" || status=1
done
exit $status
