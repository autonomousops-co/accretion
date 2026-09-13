#!/usr/bin/env bash
#
# Assert the RBAC shape of rendered accretion-collector manifests. Regression
# test for #100 and #103; `make rbac-check` renders the DEFAULT install and
# every env values file and runs this over each.
#
# Layering: templates/_validation.tpl refuses the VALUES that would widen the
# trace pod's role; this asserts the RENDER, so a template edit that widens it
# under default values still fails here. The two do not depend on each other.
# Scope: ClusterRole/ClusterRoleBinding only. A namespaced Role bound with a
# RoleBinding is invisible to this script; the chart renders none.
#
# Why this exists: sprint-10's RBAC fix was verified against the prod render,
# and the shipped default install was wrong -- one pod carried the
# unauthenticated OTLP listener (label accretion.io/collects-traces="true") and
# a ServiceAccount whose ClusterRole granted cluster-wide pods/log. Nobody
# rendered `helm install` with no values file. This check reads the render the
# way the apiserver would: Deployment -> serviceAccountName -> ClusterRoleBinding
# subjects -> ClusterRole rules, so a regression in any of those links fails.
#
# What it asserts, per rendered file:
#   1. Every Deployment whose pod template carries collects-traces="true" is
#      bound only to rules that are a subset of {namespaces get/list, pods
#      get/list} in the core group -- no pods/log, no events, no watch, no
#      other API group (#100). The OTLP listener's identity is minimal, and
#      minimal is asserted as a whole, not as the absence of one grant.
#   2. On the single-replica path (no controller Deployment rendered), every
#      role of a process that runs the log or metric collector -- recognisable
#      by pods/log or metrics.k8s.io in its rules -- includes pods "watch":
#      that process embeds the controller, whose informer watches (#103). A
#      trace-only or event-only process embeds none and is not held to it.
#      Under scale-out (a controller Deployment is rendered) those roles must
#      NOT have watch -- the grant widens only where the controller runs --
#      and the controller's own role must.
#   3. A Deployment named *-trace-collector is never granted pods watch
#      (subsumed by 1 for the trace-labelled pod; kept as a named check).
#   4. NO role in the render grants ANY verb on secrets, and the status
#      collector's role carries configmaps exactly when the configmap kind is
#      enabled. Cluster secrets are out of scope for monitoring: Kubernetes has
#      no metadata-only read for a Secret, so collecting their metadata would
#      mean cluster-wide read of every secret value. Accretion's components
#      still authenticate from Secrets the kubelet resolves via secretKeyRef,
#      which needs no RBAC, so a secrets rule here is always wrong.
#
# Each assertion refuses to pass vacuously: a render with no trace-receiving
# pod, no pods/log grant, or no data-collector role to inspect is reported as a
# failure of the check, not as a pass.
#
# Usage: scripts/check-collector-rbac.sh RENDERED.yaml...
#
# The broader, chart-agnostic "no Role/ClusterRole anywhere grants a verb on
# secrets" property (#114) is a separate script, scripts/check-secrets-grant.sh
# -- it does not reuse anything here, since this file's assertions are
# specific to the collector chart's own Deployment/ClusterRoleBinding shape.
# It used to live here as a second mode; cycle-1 review flagged that as a
# second program wearing the host script's name, ~140 lines of private awk
# behind an early exit, so it moved out.

set -euo pipefail

if [ $# -eq 0 ]; then
  echo "usage: $0 RENDERED.yaml..." >&2
  exit 2
fi

status=0
for f in "$@"; do
  awk -v file="$f" '
  function flush() {
    if (kind == "ClusterRole") {
      cr_seen[name] = 1
      cr_log[name] = haslog
      cr_metrics[name] = hasmetrics
      cr_pods[name] = haspods
      cr_watch[name] = podswatch
      cr_secrets[name] = hassecrets
      cr_configmaps[name] = hasconfigmaps
      cr_rules[name] = rules
      cr_nrules[name] = nrules
      cr_aggregated[name] = aggregated
    } else if (kind == "ClusterRoleBinding") {
      n = split(subs, arr, " ")
      for (i = 1; i <= n; i++) bind[arr[i]] = bind[arr[i]] " " role
    } else if (kind == "Deployment") {
      dep_sa[name] = sa
      dep_traces[name] = traces
      dep_ctrl[name] = controller
      if (controller) controllers++
    }
  }
  function reset() {
    kind = ""; name = ""; sect = ""; role = ""; subs = ""; grp = ""; res = ""
    haslog = 0; haspods = 0; podswatch = 0; sa = ""; traces = 0; controller = 0
    hassecrets = 0; hasconfigmaps = 0; cmkind_next = 0; rules = ""
    hasmetrics = 0; nrules = 0; aggregated = 0
  }
  function fail(msg) { printf "%s: FAIL %s\n", label, msg; fails++ }
  BEGIN { reset(); label = file; sub(/.*\//, "", label) }
  /^---/ { flush(); reset(); next }
  /^kind: / { kind = $2; next }
  /^metadata:$/ { sect = "metadata"; next }
  /^roleRef:$/ { sect = "roleRef"; next }
  /^subjects:$/ { sect = "subjects"; next }
  /^rules:$/ { sect = "rules"; next }
  /^spec:$/ { sect = "spec"; next }
  /^  name: / {
    if (sect == "metadata" && name == "") name = $2
    else if (sect == "roleRef") role = $2
    else if (sect == "subjects") subs = subs " " $2
    next
  }
  sect == "rules" && /^- apiGroups:/ { grp = $0; res = ""; next }
  sect == "rules" && /^  resources:/ { res = $0; next }
  sect == "rules" && /^  verbs:/ {
    rules = rules SUBSEP grp "|" res "|" $0
    nrules++
    if (res ~ /"pods\/log"/) haslog = 1
    if (grp ~ /metrics\.k8s\.io/) hasmetrics = 1
    if (res ~ /"secrets"/) hassecrets = 1
    if (res ~ /"configmaps"/) hasconfigmaps = 1
    if (grp ~ /\[""\]/ && res ~ /\["pods"\]/) { haspods = 1; if ($0 ~ /"watch"/) podswatch = 1 }
    next
  }
  # The rendered config carries the kind toggles under collectors:; the
  # configmap kind is the one that reads Secrets.
  kind == "ConfigMap" && /^      configmap:$/ { cmkind_next = 1; next }
  kind == "ConfigMap" && cmkind_next && /^        enabled: / { cmkind = $2; cmkind_next = 0; next }
  kind == "ClusterRole" && /^aggregationRule:/ { aggregated = 1 }
  kind == "Deployment" && /^    app\.kubernetes\.io\/component: collector$/ { controller = 1 }
  kind == "Deployment" && /serviceAccountName:/ { sa = $2 }
  kind == "Deployment" && /accretion\.io\/collects-traces: "true"/ { traces = 1 }
  END {
    flush()

    # 1. The trace-receiving pod holds a minimal identity: every bound rule is
    #    core-group namespaces or pods with verbs drawn from get/list only.
    found_trace = 0; found_log_role = 0
    for (d in dep_sa) {
      if (!dep_traces[d]) continue
      found_trace++
      n = split(bind[dep_sa[d]], roles, " ")
      if (n == 0) fail(d " carries collects-traces=\"true\" but ServiceAccount " dep_sa[d] " is bound to no ClusterRole")
      for (i = 1; i <= n; i++) {
        r = roles[i]
        if (!cr_seen[r]) { fail(d " (collects-traces=\"true\") is bound to ClusterRole " r ", which is not in the render; a role outside the chart (cluster-admin?) cannot be minimal"); continue }
        if (cr_aggregated[r]) { fail(d " (collects-traces=\"true\") is bound to ClusterRole " r ", which carries an aggregationRule; its effective rules are not in the render"); continue }
        if (cr_nrules[r] == 0) { fail(d " (collects-traces=\"true\") is bound to ClusterRole " r ", which has no rules in the render; nothing to prove minimal"); continue }
        if (cr_log[r]) fail(d " (collects-traces=\"true\", serviceAccount " dep_sa[d] ") is bound to ClusterRole " r ", which grants pods/log")
        nr = split(cr_rules[r], rl, SUBSEP)
        for (j = 1; j <= nr; j++) {
          if (rl[j] == "") continue
          split(rl[j], parts, "|")
          g = parts[1]; rs = parts[2]; vb = parts[3]
          sub(/.*verbs: \[/, "", vb); sub(/\].*/, "", vb); gsub(/[" ]/, "", vb)
          nv = split(vb, verbs, ",")
          badverb = ""
          for (k = 1; k <= nv; k++) if (verbs[k] != "get" && verbs[k] != "list") badverb = verbs[k]
          if (g !~ /apiGroups: \[""\]/ || (rs !~ /resources: \["namespaces"\]/ && rs !~ /resources: \["pods"\]/) || badverb != "")
            fail(d " (collects-traces=\"true\") is bound to ClusterRole " r " carrying [" g " / " rs " / verbs " vb "], outside the minimal identity of namespaces get/list + pods get/list")
        }
      }
    }
    for (r in cr_log) if (cr_log[r]) found_log_role++
    if (!found_trace) fail("no Deployment carries collects-traces=\"true\"; the minimal-identity check is vacuous")
    if (!found_log_role) fail("no ClusterRole grants pods/log; the pods/log half of the check is vacuous")
    if (fails == 0) printf "  ok  %s: the collects-traces=\"true\" pod holds namespaces get/list + pods get/list and nothing else\n", label

    # 2. pods watch lands exactly where the controller runs: a process running
    #    log or metric, which its role betrays through pods/log or metrics.k8s.io.
    before = fails; checked = 0
    for (d in dep_sa) {
      if (d !~ /-(data|log|metric)-collector$/) continue
      n = split(bind[dep_sa[d]], roles, " ")
      for (i = 1; i <= n; i++) {
        r = roles[i]
        if (!cr_pods[r]) continue
        if (!cr_log[r] && !cr_metrics[r]) continue
        checked++
        if (controllers == 0 && !cr_watch[r]) fail(d " embeds the controller (no controller Deployment rendered) but ClusterRole " r " grants pods without watch")
        if (controllers > 0 && cr_watch[r]) fail(d " runs under a standalone controller but ClusterRole " r " was widened to pods watch")
      }
    }
    for (d in dep_sa) {
      if (!dep_ctrl[d]) continue
      n = split(bind[dep_sa[d]], roles, " ")
      ok = 0
      for (i = 1; i <= n; i++) if (cr_watch[roles[i]]) ok = 1
      if (!ok) fail("controller " d " has no ClusterRole granting pods watch; its informer cannot run")
    }
    if (checked == 0) fail("no data-collector ClusterRole with a pods rule found; the watch check is vacuous")
    if (fails == before) {
      if (controllers == 0) printf "  ok  %s: single-replica path, every data-collector pods rule includes watch\n", label
      else printf "  ok  %s: scale-out path, data-collector pods rules stay get/list and the controller has watch\n", label
    }

    # 3. A trace-only pod lists pods; it never watches them.
    before = fails
    for (d in dep_sa) {
      if (d !~ /-trace-collector$/) continue
      n = split(bind[dep_sa[d]], roles, " ")
      for (i = 1; i <= n; i++) if (cr_watch[roles[i]]) fail(d " is trace-only but ClusterRole " roles[i] " grants pods watch")
    }
    if (fails == before) printf "  ok  %s: no trace-only pod is granted pods watch\n", label

    # 4. No role anywhere grants secrets; configmaps track the kind toggle.
    before = fails; status = ""; nrole = 0
    for (r in cr_seen) {
      nrole++
      if (cr_secrets[r]) fail("ClusterRole " r " grants a verb on secrets; cluster secrets are out of scope for monitoring")
    }
    if (nrole == 0) fail("no ClusterRole rendered; the secrets check is vacuous")
    for (d in dep_sa) if (d ~ /-status-collector$/) status = d
    if (status == "") {
      fail("no status-collector Deployment rendered; the configmap check is vacuous")
    } else if (cmkind != "true" && cmkind != "false") {
      fail("could not read collectors.configmap.enabled from the rendered ConfigMap; the configmap check is vacuous")
    } else {
      n = split(bind[dep_sa[status]], roles, " ")
      if (n == 0) fail(status " ServiceAccount " dep_sa[status] " is bound to no ClusterRole")
      for (i = 1; i <= n; i++) {
        r = roles[i]
        if (cmkind == "true" && !cr_configmaps[r]) fail("configmap kind enabled but ClusterRole " r " does not grant configmaps")
        if (cmkind == "false" && cr_configmaps[r]) fail("configmap kind disabled but ClusterRole " r " still grants configmaps")
      }
    }
    if (fails == before) {
      printf "  ok  %s: no role grants secrets (%d roles checked); configmap kind=%s matches its grant\n", label, nrole, cmkind
    }

    exit fails > 0
  }' "$f" || status=1
done
exit $status
