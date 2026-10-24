{{/*
Expand the name of the chart.
*/}}
{{- define "accretion-db.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "accretion-db.fullname" -}}
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
{{- define "accretion-db.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "accretion-db.labels" -}}
helm.sh/chart: {{ include "accretion-db.chart" . }}
{{ include "accretion-db.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "accretion-db.selectorLabels" -}}
app.kubernetes.io/name: {{ include "accretion-db.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Name of the Secret holding the database password, and the key inside it.

Two halves of one decision, so they are defined once and read wherever a
secretKeyRef is built (templates/schema-job.yaml,
templates/partition-maintenance-cronjob.yaml). When the operator supplies
config.database.existingSecret, that Secret and its existingSecretKey are what
the jobs reference and the chart creates nothing of its own; otherwise
templates/secret.yaml provides "<fullname>-db" with a db-password key.

Spelling the pair out at each call site is how one of them drifts: a renamed key
on one side and not the other renders perfectly well and fails at rollout.

NO `| default` on existingSecretKey, deliberately (#134 cycle-3 review). It had
one, defended as fail-safe, and it was the opposite. values.yaml already ships
`existingSecretKey: "db-password"`, so an operator who names an existingSecret
and says nothing about the key is covered by a default that is visible,
greppable and documented -- a `| default` here never sees that case. The only
input it could act on is an operator EXPLICITLY blanking the key, which
_validation.tpl refuses by name, and if that guard were ever removed the
`| default` would substitute a guessed conventional name for a value the
operator deliberately emptied. That is a second fallback, not defence in depth;
defence in depth would be a second refusal. It is the same shape as the
`database.password: "postgres"` default this whole change exists to delete, one
level up and on a key name rather than a credential. Returning what the operator
actually set lets the failure happen where it is legible.
*/}}
{{- define "accretion-db.dbSecretName" -}}
{{- if .Values.config.database.existingSecret -}}
{{- .Values.config.database.existingSecret -}}
{{- else -}}
{{- printf "%s-db" (include "accretion-db.fullname" .) -}}
{{- end -}}
{{- end }}

{{- define "accretion-db.dbSecretKey" -}}
{{- if .Values.config.database.existingSecret -}}
{{- .Values.config.database.existingSecretKey -}}
{{- else -}}
db-password
{{- end -}}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "accretion-db.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "accretion-db.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}
