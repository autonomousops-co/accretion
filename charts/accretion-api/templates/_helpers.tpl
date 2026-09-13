{{/*
Expand the name of the chart.
*/}}
{{- define "accretion-api.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "accretion-api.fullname" -}}
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
{{- define "accretion-api.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "accretion-api.labels" -}}
helm.sh/chart: {{ include "accretion-api.chart" . }}
{{ include "accretion-api.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "accretion-api.selectorLabels" -}}
app.kubernetes.io/name: {{ include "accretion-api.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Name of the Secret holding the database password (key: db-password).
When config.database.existingSecret is set, the operator provisions the secret
out-of-band and the chart references it without creating one. Otherwise the
chart owns a distinctly-named "<fullname>-db" secret so it never clobbers the
externally-provisioned "accretion-api-secrets" (oauth-client-id, session-key,
etc.).
*/}}
{{- define "accretion-api.dbSecretName" -}}
{{- if .Values.config.database.existingSecret -}}
{{- .Values.config.database.existingSecret -}}
{{- else -}}
{{- printf "%s-db" (include "accretion-api.fullname" .) -}}
{{- end -}}
{{- end }}

{{/*
Name of the chart-owned Secret carrying per-client API-key plaintext, one
`api-key-<name>` entry per apiKeys entry that declares a `key`.

Distinct from the operator-provisioned "accretion-api-secrets" (session-key,
oauth-client-id, ...) for the same reason as the db secret above: Helm cannot
adopt a Secret it did not create, so writing into that name would break every
install where the operator (or scripts/make-secret.sh) provisions it first.

A FIXED LITERAL, not a fullname-derived name — the rule for every Secret this
chart publishes to consumers OUTSIDE it (the same rule as accretion-mcp-secrets).
Those consumers are separate charts and standalone client configs that cannot
compute this release's fullname: the accretion-discord chart's api.secretName
and `make test-functional` both name it directly. Deriving it would leave them
pinned to the default release name and wedge the bot in
CreateContainerConfigError on any other. Two accretion-api releases in one
namespace collide, which Helm 3 rejects at install as an ownership conflict —
loud, and the same trade accretion-mcp-secrets already makes.

Secrets consumed only INSIDE this chart keep the fullname prefix (see
accretion-api.dbSecretName above), because nothing outside has to name them.
*/}}
{{- define "accretion-api.clientKeysSecretName" -}}
accretion-api-client-keys
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "accretion-api.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "accretion-api.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The Secret holding the API listener's keypair: the operator's own when
config.api.tls.existingSecret is set, otherwise the one cert-manager issues into.
Every consumer — the volume, the file paths, the probes — resolves it through
here so a BYO Secret and an issued one are the same shape downstream.
*/}}
{{- define "accretion-api.tlsSecretName" -}}
{{- $tls := .Values.config.api.tls -}}
{{- if $tls.existingSecret -}}
{{- $tls.existingSecret -}}
{{- else -}}
{{- printf "%s-tls" (include "accretion-api.fullname" .) -}}
{{- end -}}
{{- end }}

{{/*
The Secret cert-manager issues the self-signed CA into, and the Issuer that then
signs the serving certificate from it. Only created when the chart bootstraps its
own CA (certManager.enabled with no issuerRef).
*/}}
{{- define "accretion-api.tlsCASecretName" -}}
{{- printf "%s-tls-ca" (include "accretion-api.fullname" .) -}}
{{- end }}

{{/*
Whether the chart bootstraps its own self-signed CA. False when the operator
points at an issuer they already run, in which case no Issuer is created here.
*/}}
{{- define "accretion-api.tlsBootstrapCA" -}}
{{- $tls := .Values.config.api.tls -}}
{{- if and $tls.enabled $tls.certManager.enabled (not $tls.existingSecret) (not $tls.certManager.issuerRef.name) -}}
true
{{- end -}}
{{- end }}

{{/*
The certificate's subject alternative names.

The in-cluster Service DNS in all four forms, plus localhost and 127.0.0.1. The
loopback entries are mandatory, not a convenience: every local client reaches the
API as localhost:41337 through a port-forward, so a certificate without them
verifies correctly in-cluster and fails on every contributor's machine — the
failure most likely to ship unnoticed, because CI never dials through a
port-forward.
*/}}
{{- define "accretion-api.tlsDNSNames" -}}
{{- $fullname := include "accretion-api.fullname" . -}}
{{- $ns := .Release.Namespace -}}
{{- $names := list
      $fullname
      (printf "%s.%s" $fullname $ns)
      (printf "%s.%s.svc" $fullname $ns)
      (printf "%s.%s.svc.cluster.local" $fullname $ns)
      "localhost" -}}
{{- $names = concat $names (.Values.config.api.tls.certManager.extraDNSNames | default list) -}}
{{- toYaml (uniq $names) -}}
{{- end }}

{{- define "accretion-api.tlsIPAddresses" -}}
{{- $ips := list "127.0.0.1" -}}
{{- $ips = concat $ips (.Values.config.api.tls.certManager.extraIPAddresses | default list) -}}
{{- toYaml (uniq $ips) -}}
{{- end }}
