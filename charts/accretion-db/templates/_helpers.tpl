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
{{- .Values.config.database.existingSecretKey | default "db-password" -}}
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
