{{/*
Fail-closed guards for the accretion-collector chart, in the same spirit as the
accretion-api chart's. These run at render time, so an incoherent combination is
a failed `helm template`/`helm upgrade` rather than a cluster that comes up
quietly wrong.
*/}}

{{- define "accretion-collector.validate" -}}

{{- $scaled := include "accretion-collector.scaleOutEnabled" . -}}

{{/*
Scaling a collector past one replica without the coordination layer is the
incoherent combination this exists to catch (#93). Two collector replicas
sharing no queue both dispatch and both collect: duplicate records, and no
failover between them. scaleOutEnabled turns the controller and Redis on
automatically, so reaching this means an operator disabled collector.enabled by
hand while scaling a worker.
*/}}
{{- if and $scaled (not .Values.collector.enabled) -}}
{{- fail (printf "accretion-collector: a collector is scaled beyond one replica (or has autoscaling enabled) but collector.enabled is false. Sharding work across replicas needs the controller and Redis; either set collector.enabled=true or return every collector to replicaCount 1 with autoscaling disabled.") -}}
{{- end -}}

{{/*
redis.enabled selects the Bitnami subchart INSTEAD of this chart's own
standalone Redis template -- and this chart declares no Bitnami dependency, so
true means no Redis is deployed at all. Harmless while nothing needs a queue;
fatal the moment scale-out does, and the symptom is collectors wedged on an
initContainer waiting for a host that will never exist. Every checked-in env
values file used to set it true.
*/}}
{{- if and $scaled (index .Values "redis" "enabled") -}}
{{- fail "accretion-collector: scale-out is enabled, which needs Redis, but redis.enabled is true -- that selects the Bitnami subchart instead of this chart's standalone Redis template, and this chart declares no such dependency, so no Redis would be deployed. Set redis.enabled=false to use the chart's own Redis." -}}
{{- end -}}

{{/*
A Deployment that would run no collector at all. Rendering it produces a pod
that starts, finds nothing enabled and exits -- a CrashLoopBackOff whose cause
is a values file, not a bug.
*/}}
{{- if .Values.statusCollector.enabled -}}
{{- $anyStatus := false -}}
{{- range $name, $k := .Values.statusCollector.kinds -}}
{{- if $k.enabled -}}{{- $anyStatus = true -}}{{- end -}}
{{- end -}}
{{- if not $anyStatus -}}
{{- fail "accretion-collector: statusCollector.enabled is true but every kind under statusCollector.kinds is disabled, which would deploy a pod that collects nothing. Set statusCollector.enabled=false instead." -}}
{{- end -}}
{{- end -}}

{{/*
The trace collector's OTLP listener is unauthenticated, so its identity must
hold nothing but namespaces and pods get/list (#100). Every other collector
widens the role of a process it shares: log brings cluster-wide pods/log,
metric brings metrics.k8s.io and, on the in-process path, pods watch, event
brings events. So trace may share the data-collector process with nothing.
Split is the isolation control, the chart ships trace split, and this refuses
the values that would quietly undo it -- including the "split log out instead"
escape an earlier message offered, which left metric and event beside it.
*/}}
{{- if .Values.dataCollector.enabled -}}
{{- $sharedSet := include "accretion-collector.sharedDataCollectors" . | fromJsonArray -}}
{{- if and (has "trace" $sharedSet) (gt (len $sharedSet) 1) -}}
{{- fail (printf "accretion-collector: the trace collector shares the data-collector process with %s, which would widen the identity behind the unauthenticated OTLP listener (:4318) beyond namespaces and pods get/list. Keep dataCollector.kinds.trace.split=true, the default." (join ", " (without $sharedSet "trace"))) -}}
{{- end -}}
{{- end -}}

{{- if .Values.dataCollector.enabled -}}
{{- $anyData := false -}}
{{- range $name := (list "log" "metric" "event" "trace") -}}
{{- if (index $.Values.dataCollector.kinds $name).enabled -}}{{- $anyData = true -}}{{- end -}}
{{- end -}}
{{- if not $anyData -}}
{{- fail "accretion-collector: dataCollector.enabled is true but every collector under dataCollector.kinds is disabled, which would deploy a pod that collects nothing. Set dataCollector.enabled=false instead." -}}
{{- end -}}
{{- end -}}

{{- end -}}
