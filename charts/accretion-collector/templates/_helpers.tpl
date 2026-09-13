{{/*
Expand the name of the chart.
*/}}
{{- define "accretion-collector.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "accretion-collector.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "accretion-collector.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "accretion-collector.labels" -}}
helm.sh/chart: {{ include "accretion-collector.chart" . }}
{{ include "accretion-collector.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "accretion-collector.selectorLabels" -}}
app.kubernetes.io/name: {{ include "accretion-collector.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "accretion-collector.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "accretion-collector.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the Secret holding the database password (#113).

When config.database.existingSecret is set, the operator provisions the
Secret out of band (kubectl, External Secrets, Vault, SOPS) and this chart
only ever references it by name -- it renders no postgresql-secret of its own
and never sees the value (templates/secrets.yaml). Leave "" (the default) to
keep the chart creating and owning the Secret from config.database.password.
*/}}
{{- define "accretion-collector.postgresqlSecretName" -}}
{{- if .Values.config.database.existingSecret -}}
{{- .Values.config.database.existingSecret -}}
{{- else -}}
{{- include "accretion-collector.fullname" . }}-postgresql-secret
{{- end -}}
{{- end -}}

{{/*
Key inside the Postgres Secret that holds the password. Fixed at "password"
for the chart-owned Secret (what templates/secrets.yaml actually writes);
overridable via config.database.existingSecretKey only once existingSecret
names an operator-supplied Secret that may use a different key.
*/}}
{{- define "accretion-collector.postgresqlSecretKey" -}}
{{- if .Values.config.database.existingSecret -}}
{{- .Values.config.database.existingSecretKey | default "password" -}}
{{- else -}}
password
{{- end -}}
{{- end -}}

{{/*
Name of the Secret holding the Redis credentials (#113). Same existingSecret
pattern as the Postgres helper above, keyed off redis.auth.existingSecret.

templates/secrets.yaml's password-shape guard (fail-closed on an empty or
non-printable-ASCII redis.auth.password) only runs on the chart-owned path;
supplying an existingSecret hands both that check and rotation-driven pod
restarts to whatever created it -- see the values.yaml comment on
redis.auth.existingSecret.
*/}}
{{- define "accretion-collector.redisSecretName" -}}
{{- if .Values.redis.auth.existingSecret -}}
{{- .Values.redis.auth.existingSecret -}}
{{- else -}}
{{- include "accretion-collector.fullname" . }}-redis-secret
{{- end -}}
{{- end -}}

{{/*
Key inside the Redis Secret holding the raw client password, read via
secretKeyRef by every client (the controller and data-collector processes).
Fixed at "password" for the chart-owned Secret; overridable via
redis.auth.existingSecretPasswordKey only once existingSecret is set.
*/}}
{{- define "accretion-collector.redisSecretPasswordKey" -}}
{{- if .Values.redis.auth.existingSecret -}}
{{- .Values.redis.auth.existingSecretPasswordKey | default "password" -}}
{{- else -}}
password
{{- end -}}
{{- end -}}

{{/*
Key inside the Redis Secret holding the requirepass config-file directive,
mounted as a file by templates/redis.yaml because redis-server has no env-var
form of requirepass. Fixed at "redis.conf" for the chart-owned Secret;
overridable via redis.auth.existingSecretConfKey only once existingSecret is
set. Both this key and redisSecretPasswordKey above must be present in
whatever Secret is named -- one client-side key alone renders fine and fails
the Redis pod's volume mount at rollout.
*/}}
{{- define "accretion-collector.redisSecretConfKey" -}}
{{- if .Values.redis.auth.existingSecret -}}
{{- .Values.redis.auth.existingSecretConfKey | default "redis.conf" -}}
{{- else -}}
redis.conf
{{- end -}}
{{- end -}}

{{/*
Which data collectors run inside the SHARED data-collector process: those that
are enabled and not split out. Returns a JSON array so callers can use `has`.

The split escape hatch of #92 lives here so that "enabled and not split" is
decided in exactly one place -- the deployment template, the ConfigMap toggles
and the validation guard must all agree on it, and computing it three times is
how they would drift.
*/}}
{{- define "accretion-collector.sharedDataCollectors" -}}
{{- $out := list -}}
{{- range $name := (list "log" "metric" "event" "trace") -}}
{{- $k := index $.Values.dataCollector.kinds $name -}}
{{- if and $k.enabled (not $k.split) -}}
{{- $out = append $out $name -}}
{{- end -}}
{{- end -}}
{{- $out | toJson -}}
{{- end -}}

{{/*
Which data collectors get their own Deployment: those that are enabled AND
split. A collector that is disabled is not deployed however split is set --
"off" beats "separate".
*/}}
{{- define "accretion-collector.splitDataCollectors" -}}
{{- $out := list -}}
{{- range $name := (list "log" "metric" "event" "trace") -}}
{{- $k := index $.Values.dataCollector.kinds $name -}}
{{- if and $k.enabled $k.split -}}
{{- $out = append $out $name -}}
{{- end -}}
{{- end -}}
{{- $out | toJson -}}
{{- end -}}

{{/*
Whether anything in this release is actually scaled out (#93).

The controller and Redis exist to shard work across horizontally scaled
collectors: the controller detects dead workers and reassigns their pods, and
Redis holds that assignment state. In a default install every collector runs
1/1 with no HPA, so the controller coordinates a fleet of one and its failover
has no second worker to fail over to -- two pods and a Redis dependency for a
capability the install cannot use.

So they deploy only when some collector is genuinely scaled: more than one
replica, or autoscaling on. Turning scale-out on brings both back automatically
with no separate flag to remember, because this is the same expression the
controller, Redis and the queue mode all read.

Returns "true" or "" so callers can use it directly in an if.
*/}}
{{- define "accretion-collector.scaleOutEnabled" -}}
{{- $scaled := false -}}
{{- if .Values.dataCollector.enabled -}}
  {{- $shared := include "accretion-collector.sharedDataCollectors" . | fromJsonArray -}}
  {{- if $shared -}}
    {{- if or (gt (int .Values.dataCollector.replicaCount) 1) .Values.dataCollector.autoscaling.enabled -}}
      {{- $scaled = true -}}
    {{- end -}}
  {{- end -}}
  {{- range $name := (include "accretion-collector.splitDataCollectors" . | fromJsonArray) -}}
    {{- $k := index $.Values.dataCollector.kinds $name -}}
    {{- if or (gt (int $k.replicaCount) 1) $k.autoscaling.enabled -}}
      {{- $scaled = true -}}
    {{- end -}}
  {{- end -}}
{{- end -}}
{{- if .Values.statusCollector.enabled -}}
  {{- if or (gt (int .Values.statusCollector.replicaCount) 1) .Values.statusCollector.autoscaling.enabled -}}
    {{- $scaled = true -}}
  {{- end -}}
{{- end -}}
{{- if $scaled -}}true{{- end -}}
{{- end -}}

{{/*
The queue mode the collectors should run in. Sharding across pods needs Redis;
a single replica of each collector does not, and runs the dispatcher in-process.
One expression so the ConfigMap, the controller Deployment and Redis cannot
disagree about whether a queue exists.
*/}}
{{- define "accretion-collector.queueMode" -}}
{{- if include "accretion-collector.scaleOutEnabled" . -}}redis{{- else -}}inprocess{{- end -}}
{{- end -}}

{{/*
The ClusterRole rules a given set of data collectors needs, and nothing more.

Takes a dict of "root" and "collectors" (a list of log/metric/event/trace) so
the shared process and each split-out collector can each be granted exactly what
the collectors it actually runs require. That matters beyond tidiness: the trace
collector is the only accretion component with an unauthenticated inbound
listener (OTLP on :4318), and granting it the log collector's cluster-wide
pods/log read would mean an RCE in that listener yields every container log in
the cluster -- which is where credentials leak. Splitting a collector out is
therefore an isolation control as well as a scaling one, which is why trace
ships split (values.yaml kinds.trace.split) and why _validation.tpl refuses a
shared process that holds both trace and log (#100).
*/}}
{{- define "accretion-collector.dataCollectorRules" -}}
{{- $c := .collectors -}}
# Namespace listing backs label-selector / "*" namespace scoping (issue #146).
# Unconditional: every collector resolves a namespace scope at startup.
- apiGroups: [""]
  resources: ["namespaces"]
  verbs: ["get", "list"]
{{- if or (has "log" $c) (has "metric" $c) (has "trace" $c) }}
# The log and metric collectors enumerate pods to collect from; the trace
# collector lists them to resolve a label/"*" namespace scope.
- apiGroups: [""]
  resources: ["pods"]
{{- if and (or (has "log" $c) (has "metric" $c)) (not (include "accretion-collector.scaleOutEnabled" .root)) }}
# On the single-replica path a process running log or metric embeds the
# controller (queue mode "inprocess", #93), whose pod informer WATCHes. Without
# the verb the informer's LIST still syncs, so readiness passes, but every
# watch is forbidden: a permanent error loop and a cluster-wide pod LIST on
# every backoff (#103). Under scale-out the standalone controller holds the
# informer and this role stays get/list.
  verbs: ["get", "list", "watch"]
{{- else }}
  verbs: ["get", "list"]
{{- end }}
{{- end }}
{{- if has "log" $c }}
# Cluster-wide container log read. This is the widest grant in the chart, and it
# belongs only to a process actually running the log collector.
- apiGroups: [""]
  resources: ["pods/log"]
  verbs: ["get", "list"]
{{- end }}
{{- if has "metric" $c }}
- apiGroups: ["metrics.k8s.io"]
  resources: ["pods"]
  verbs: ["get", "list"]
{{- end }}
{{- if has "event" $c }}
- apiGroups: [""]
  resources: ["events"]
  verbs: ["get", "list", "watch"]
{{- end }}
{{- end -}}

{{/*
The ServiceAccount name for a data-collector pod: the shared process has its
own, and each split-out collector has its own so its ClusterRole can be narrower.
Takes a dict of "root" and "collector" ("" for the shared process).
*/}}
{{- define "accretion-collector.dataCollectorServiceAccountName" -}}
{{- if .collector -}}
{{ include "accretion-collector.fullname" .root }}-{{ .collector }}-collector
{{- else -}}
{{ include "accretion-collector.fullname" .root }}-data-collector
{{- end -}}
{{- end -}}
